package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

type fakeRuntime struct {
	mu          sync.Mutex
	state       runtime.State
	createN     int
	resumeN     int
	pauseN      int
	createWait  time.Duration
	createErr   error
	endpoint    runtime.Endpoint
	pauseCtxErr error
	createBlock chan struct{}
}

func newFakeRuntime(state runtime.State) *fakeRuntime {
	return &fakeRuntime{state: state, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}}
}

func (f *fakeRuntime) Create(context.Context, runtime.Spec) (runtime.Endpoint, error) {
	f.mu.Lock()
	f.createN++
	wait, err, block := f.createWait, f.createErr, f.createBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	time.Sleep(wait)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	f.mu.Lock()
	f.state = runtime.StateRunning
	f.mu.Unlock()
	return f.endpoint, nil
}

func TestCloseHonorsDeadlineDuringLifecycleOperation(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createBlock = make(chan struct{})
	manager := newTestManager(fake, nil, alwaysIdle)
	wakeDone := make(chan struct{})
	go func() {
		_, _ = manager.EnsureRunning(context.Background())
		close(wakeDone)
	}()
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want deadline exceeded", err)
	}
	close(fake.createBlock)
	select {
	case <-wakeDone:
	case <-time.After(time.Second):
		t.Fatal("wake did not finish")
	}
}

func (f *fakeRuntime) Pause(ctx context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseN++
	f.pauseCtxErr = ctx.Err()
	f.state = runtime.StatePaused
	return nil
}

func TestObserverRollbackUsesIndependentContext(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StatePaused)
	serviceCtx, cancelService := context.WithCancel(context.Background())
	manager := NewManager(serviceCtx, fake, runtime.Spec{Name: "demo"}, func(context.Context, runtime.Endpoint, bool) error {
		cancelService()
		return errors.New("observer failed")
	}, alwaysIdle, nil)
	_, _ = manager.EnsureRunning(context.Background())
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseCtxErr != nil {
		t.Fatalf("rollback context was already canceled: %v", fake.pauseCtxErr)
	}
}

func (f *fakeRuntime) Resume(context.Context, runtime.Spec) (runtime.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeN++
	f.state = runtime.StateRunning
	return f.endpoint, nil
}

func (f *fakeRuntime) EnsureRunning(ctx context.Context, spec runtime.Spec) (runtime.Endpoint, bool, error) {
	f.mu.Lock()
	state := f.state
	f.mu.Unlock()
	switch state {
	case runtime.StateAbsent:
		ep, err := f.Create(ctx, spec)
		return ep, true, err
	case runtime.StatePaused, runtime.StateProvisioning:
		ep, err := f.Resume(ctx, spec)
		return ep, true, err
	case runtime.StateRunning:
		return f.endpoint, false, nil
	default:
		return runtime.Endpoint{}, false, runtime.ErrFailed
	}
}

func (f *fakeRuntime) Destroy(context.Context, string) error { return nil }

func (f *fakeRuntime) Status(context.Context, string) (runtime.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.Observation{
		State:       f.state,
		Endpoint:    f.endpoint,
		HasEndpoint: f.state == runtime.StateRunning,
	}, nil
}

func TestConcurrentEnsureRunningCreatesOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createWait = 50 * time.Millisecond
	manager := newTestManager(fake, nil, alwaysIdle)
	const callers = 10
	start := make(chan struct{})
	errorsChannel := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := manager.EnsureRunning(context.Background())
			errorsChannel <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createN != 1 {
		t.Fatalf("Create calls = %d, want 1", fake.createN)
	}
	if fake.resumeN != 0 {
		t.Fatalf("Resume calls = %d, want 0", fake.resumeN)
	}
}

func TestEnsureRunningPropagatesCreateFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("create failed")
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createErr = want
	manager := newTestManager(fake, nil, alwaysIdle)
	_, err := manager.EnsureRunning(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestActiveRequestDefersPauseUntilRelease(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	_, release, err := manager.AcquireRequest(context.Background(), RequestIntent{Hold: true, MayStartWork: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Pause(context.Background()); !errors.Is(err, ErrRequestsActive) {
		t.Fatalf("Pause error = %v, want ErrRequestsActive", err)
	}
	release()
	if err := manager.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 1 {
		t.Fatalf("Pause calls = %d, want 1", fake.pauseN)
	}
}

func TestAuthoritativeBusyStatusDefersPause(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, func(context.Context, runtime.Endpoint) (bool, error) { return false, nil })
	if err := manager.Pause(context.Background()); !errors.Is(err, ErrSessionsActive) {
		t.Fatalf("Pause error = %v, want ErrSessionsActive", err)
	}
}

func TestWakeAttachesObserverBeforeReturning(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StatePaused)
	updated := false
	manager := newTestManager(fake, func(_ context.Context, ep runtime.Endpoint, force bool) error {
		updated = true
		if ep.Port != 4096 || !force {
			t.Fatalf("observer got endpoint=%+v force=%t", ep, force)
		}
		return nil
	}, alwaysIdle)
	if _, err := manager.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("EnsureRunning returned before updating endpoint observer")
	}
}

func TestObserverFailureRollsBackWake(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StatePaused)
	want := errors.New("observer failed")
	manager := newTestManager(fake, func(context.Context, runtime.Endpoint, bool) error { return want }, alwaysIdle)
	if _, err := manager.EnsureRunning(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want observer failure", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.state != runtime.StatePaused || fake.pauseN != 1 {
		t.Fatalf("wake rollback state=%s pauses=%d", fake.state, fake.pauseN)
	}
}

func TestCloseWaitsForActiveRequestAndRejectsNewWork(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	_, release, err := manager.AcquireRequest(context.Background(), RequestIntent{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- manager.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned with active request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after request release")
	}
	if _, _, err := manager.AcquireRequest(context.Background(), RequestIntent{}); err == nil {
		t.Fatal("manager accepted request after Close")
	}
}

func newTestManager(fake lifecycleRuntime, observe EndpointObserver, idle IdleChecker) *Manager {
	return NewManager(context.Background(), fake, runtime.Spec{Name: "demo"}, observe, idle, nil)
}

func alwaysIdle(context.Context, runtime.Endpoint) (bool, error) { return true, nil }
