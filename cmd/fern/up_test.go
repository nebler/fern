package main

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/workspace"
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
