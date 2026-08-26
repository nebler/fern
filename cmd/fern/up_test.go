package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/taskresultcoord"
	"github.com/nebler/fern/internal/workspace"
	"golang.org/x/sync/errgroup"
)

type healthWaker struct {
	err error
}

func (waker *healthWaker) AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	return workspace.RequestTarget{}, func() {}, waker.err
}

func (*healthWaker) InvalidateEndpoint(workspace.RequestTarget) {}

func TestStatusWakerReportsTransientFailureAndRecovery(t *testing.T) {
	status := observability.NewRegistry()
	delegate := &healthWaker{err: errors.New("docker unavailable")}
	waker := &statusWaker{Waker: delegate, status: status}
	if _, release, err := waker.AcquireRequest(context.Background(), workspace.RequestRead); err == nil {
		release()
		t.Fatal("wake unexpectedly succeeded")
	}
	snapshot := status.Snapshot()
	if snapshot.Components[0].State != observability.StateDegraded || !snapshot.Ready {
		t.Fatalf("failed wake snapshot = %+v", snapshot)
	}
	delegate.err = nil
	_, release, err := waker.AcquireRequest(context.Background(), workspace.RequestRead)
	release()
	if err != nil || status.Snapshot().Components[0].State != observability.StateHealthy {
		t.Fatalf("recovered wake error=%v snapshot=%+v", err, status.Snapshot())
	}
}

func TestTrustedProxyOriginsPreserveLocalCompatibility(t *testing.T) {
	t.Parallel()
	cfg := config.Default(t.TempDir())
	origins := trustedProxyOrigins(cfg)
	if origins.Remote != "http://127.0.0.1:8080" || origins.Operator != "http://127.0.0.1:8081" {
		t.Fatalf("local origins = %+v", origins)
	}
	cfg.RemoteOrigin = "https://fern.example.ts.net"
	origins = trustedProxyOrigins(cfg)
	if origins.Remote != cfg.RemoteOrigin || origins.Operator != "http://127.0.0.1:8081" {
		t.Fatalf("published origins = %+v", origins)
	}
}

func TestListenProxySurfacesReleasesRemoteWhenOperatorBindFails(t *testing.T) {
	t.Parallel()
	operator, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteAddress := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenProxySurfaces(remoteAddress, operator.Addr().String()); err == nil {
		t.Fatal("listener setup succeeded with occupied operator address")
	}
	rebound, err := net.Listen("tcp", remoteAddress)
	if err != nil {
		t.Fatalf("remote listener remained bound after operator failure: %v", err)
	}
	_ = rebound.Close()
}

type blockingTaskService struct {
	started chan struct{}
	once    sync.Once
}

func (service *blockingTaskService) Run(ctx context.Context) error {
	service.once.Do(func() { close(service.started) })
	<-ctx.Done()
	return ctx.Err()
}

func (*blockingTaskService) Wake() {}

type idleResultService struct{}

func (idleResultService) RunOnce(context.Context) error { return taskresultcoord.ErrNoWork }

func TestTaskCoordinatorServiceMatrixStartsAndStopsAsOneOwner(t *testing.T) {
	for _, test := range []struct {
		name                      string
		publication, verification bool
	}{
		{name: "required"},
		{name: "verification", verification: true},
		{name: "publication", publication: true},
		{name: "full", publication: true, verification: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			group, serviceCtx := errgroup.WithContext(ctx)
			services := make([]*blockingTaskService, 0, 4)
			newService := func() *blockingTaskService {
				service := &blockingTaskService{started: make(chan struct{})}
				services = append(services, service)
				return service
			}
			tasks := &taskServices{
				coordinator: newService(), execution: newService(), result: idleResultService{},
				resultWake: make(chan struct{}, 1), status: observability.NewRegistry(),
			}
			if test.publication {
				tasks.publication = &publicationTaskService{blockingTaskService: newService()}
			}
			if test.verification {
				tasks.verification = newService()
			}
			startTaskCoordinators(group, tasks, serviceCtx, slog.New(slog.NewTextHandler(io.Discard, nil)), "demo")
			for _, service := range services {
				select {
				case <-service.started:
				case <-time.After(time.Second):
					t.Fatal("configured task service did not start")
				}
			}
			cancel()
			if err := group.Wait(); err != nil {
				t.Fatalf("coordinator shutdown: %v", err)
			}
		})
	}
}

type publicationTaskService struct{ *blockingTaskService }

type orderedManagerLifecycle struct {
	mu    sync.Mutex
	order []string
}

func (manager *orderedManagerLifecycle) record(value string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.order = append(manager.order, value)
}

func (manager *orderedManagerLifecycle) PrepareShutdown(context.Context) error {
	manager.record("prepare")
	return nil
}

func (manager *orderedManagerLifecycle) Close(context.Context) error {
	manager.record("manager-close")
	return nil
}

type orderedStreamStopper struct{ manager *orderedManagerLifecycle }

func (stream orderedStreamStopper) Stop(context.Context) error {
	stream.manager.record("stream-stop")
	return nil
}

func TestAwaitShutdownPreservesSignalOwnershipOrder(t *testing.T) {
	for _, test := range []struct {
		name, want string
		signal     bool
	}{
		{name: "service failure", want: "manager-close,stream-stop"},
		{name: "signal", signal: true, want: "prepare,manager-close,stream-stop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootCtx, cancel := context.WithCancel(context.Background())
			group, _ := errgroup.WithContext(rootCtx)
			if test.signal {
				cancel()
			} else {
				defer cancel()
			}
			manager := &orderedManagerLifecycle{}
			runtime := &upRuntime{
				lifecycle:        &dockerLifecycle{manager: manager, managerStarted: true},
				streamController: orderedStreamStopper{manager: manager},
			}
			if err := awaitShutdown(group, runtime, rootCtx); err != nil {
				t.Fatal(err)
			}
			manager.mu.Lock()
			got := strings.Join(manager.order, ",")
			manager.mu.Unlock()
			if got != test.want || !runtime.lifecycle.managerClosed {
				t.Fatalf("shutdown order = %q, closed=%v", got, runtime.lifecycle.managerClosed)
			}
		})
	}
}
