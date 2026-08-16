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
	mu             sync.Mutex
	state          runtime.State
	createN        int
	ensureN        int
	resumeN        int
	pauseN         int
	statusN        int
	createWait     time.Duration
	createErr      error
	endpoint       runtime.Endpoint
	statusEndpoint *runtime.Endpoint
	pauseCtxErr    error
	createBlock    chan struct{}
	createStart    chan struct{}
	respectCtx     bool
}

func newFakeRuntime(state runtime.State) *fakeRuntime {
	return &fakeRuntime{state: state, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}}
}

func (f *fakeRuntime) Create(ctx context.Context, _ runtime.Spec) (runtime.Endpoint, error) {
	f.mu.Lock()
	f.createN++
	wait, err, block, started, respectCtx := f.createWait, f.createErr, f.createBlock, f.createStart, f.respectCtx
	f.mu.Unlock()
	if started != nil {
		close(started)
	}
	if block != nil {
		if respectCtx {
			select {
			case <-block:
			case <-ctx.Done():
				return runtime.Endpoint{}, ctx.Err()
			}
		} else {
			<-block
		}
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

func TestCanceledCallerDoesNotCancelSharedWake(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createBlock = make(chan struct{})
	fake.createStart = make(chan struct{})
	fake.respectCtx = true
	requests := 0
	manager := NewManager(context.Background(), fake, runtime.Spec{Name: "demo"}, nil, alwaysIdle, func() { requests++ })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, release, err := manager.AcquireRequest(ctx, RequestWork)
		release()
		done <- err
	}()
	<-fake.createStart
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v, want context canceled", err)
	}
	if requests != 0 {
		t.Fatalf("canceled request emitted %d activity observations", requests)
	}
	close(fake.createBlock)
	fake.mu.Lock()
	fake.createStart = nil
	fake.mu.Unlock()
	if _, err := manager.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("wake after cancellation failed: %v", err)
	}
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
	f.ensureN++
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

func TestNonWakingRequestDoesNotResumePausedWorkspace(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StatePaused)
	manager := newTestManager(fake, nil, alwaysIdle)
	if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("non-waking request error = %v, want ErrNotRunning", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.ensureN != 0 || fake.resumeN != 0 {
		t.Fatalf("non-waking request ensured=%d resumed=%d", fake.ensureN, fake.resumeN)
	}
}

func TestWakeTimeoutCoversHealthAndObserverBudgets(t *testing.T) {
	t.Parallel()
	manager := newTestManager(newFakeRuntime(runtime.StateRunning), nil, alwaysIdle)
	if manager.wakeOperationTimeout < 70*time.Second {
		t.Fatalf("wake operation timeout = %s, want at least 70s", manager.wakeOperationTimeout)
	}
}

func (f *fakeRuntime) Destroy(context.Context, string) error { return nil }

func (f *fakeRuntime) Status(context.Context, string) (runtime.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusN++
	endpoint := f.endpoint
	if f.statusEndpoint != nil {
		endpoint = *f.statusEndpoint
	}
	return runtime.Observation{
		State:       f.state,
		Endpoint:    endpoint,
		HasEndpoint: f.state == runtime.StateRunning,
	}, nil
}

func TestPauseRetriesPendingProvisioningState(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateProvisioning)
	manager := newTestManager(fake, nil, alwaysIdle)
	if err := manager.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 1 || fake.state != runtime.StatePaused {
		t.Fatalf("pause calls=%d state=%s", fake.pauseN, fake.state)
	}
}

func TestRunningTargetRejectsClosingManager(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	manager.wakeMu.Lock()
	manager.closing = true
	manager.wakeMu.Unlock()
	if _, err := manager.runningTarget(); err == nil {
		t.Fatal("runningTarget succeeded while closing")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.statusN != 0 {
		t.Fatalf("status calls after closing = %d", fake.statusN)
	}
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

func TestFailedWakeCanBeRetriedImmediately(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createErr = errors.New("transient create failure")
	manager := newTestManager(fake, nil, alwaysIdle)
	if _, err := manager.EnsureRunning(context.Background()); err == nil {
		t.Fatal("first wake succeeded")
	}
	fake.mu.Lock()
	fake.createErr = nil
	fake.mu.Unlock()
	if _, err := manager.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("immediate retry failed: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.ensureN != 2 {
		t.Fatalf("runtime ensure calls = %d, want 2", fake.ensureN)
	}
}

func TestSequentialRequestsReuseRunningEndpointUntilInvalidated(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	var firstTarget RequestTarget
	for range 2 {
		target, release, err := manager.AcquireRequest(context.Background(), RequestRead)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if target.Endpoint != fake.endpoint || target.Generation == 0 {
			t.Fatalf("target = %+v, want endpoint %+v with generation", target, fake.endpoint)
		}
		if target.Generation != 1 {
			t.Fatalf("generation = %d, want 1", target.Generation)
		}
		if firstTarget.Generation == 0 {
			firstTarget = target
		}
	}
	fake.mu.Lock()
	if fake.ensureN != 1 {
		t.Fatalf("runtime ensure calls = %d, want 1", fake.ensureN)
	}
	fake.mu.Unlock()

	manager.InvalidateEndpoint(firstTarget)
	_, release, err := manager.AcquireRequest(context.Background(), RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	release()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.ensureN != 2 {
		t.Fatalf("runtime ensure calls after invalidation = %d, want 2", fake.ensureN)
	}
}

func TestStaleFailureDoesNotInvalidateNewGeneration(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	first, release, err := manager.AcquireRequest(context.Background(), RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	release()
	manager.InvalidateEndpoint(first)
	second, release, err := manager.AcquireRequest(context.Background(), RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if second.Generation == first.Generation {
		t.Fatalf("generation was not advanced: first=%+v second=%+v", first, second)
	}
	manager.InvalidateEndpoint(first)
	current, _, err := manager.AcquireRequest(context.Background(), RequestObserve)
	if err != nil {
		t.Fatal(err)
	}
	if current != second {
		t.Fatalf("stale failure invalidated new generation: current=%+v second=%+v", current, second)
	}
}

func TestCloseWaitsForPauseAdmittedBeforeLifecycle(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	if err := manager.acquireLifecycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.beginPause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.isClosing() {
		t.Fatal("manager unexpectedly closing")
	}
	pauseDone := make(chan error, 1)
	go func() {
		if err := manager.acquireLifecycle(context.Background()); err != nil {
			pauseDone <- err
			return
		}
		err := fake.Pause(context.Background(), "demo")
		manager.releaseLifecycle()
		manager.endPause()
		pauseDone <- err
	}()
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close(context.Background()) }()
	manager.releaseLifecycle()
	if err := <-pauseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 1 {
		t.Fatalf("Close returned without admitted pause: pause calls = %d", fake.pauseN)
	}
}

func TestPauseClearsCachedEndpoint(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	if _, err := manager.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("observation after pause error = %v, want ErrNotRunning", err)
	}
}

func TestActiveRequestDefersPauseUntilRelease(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	manager := newTestManager(fake, nil, alwaysIdle)
	_, release, err := manager.AcquireRequest(context.Background(), RequestWork)
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

func TestRequestCannotCrossAuthoritativePauseCheck(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	idleStarted := make(chan struct{})
	allowIdle := make(chan struct{})
	manager := newTestManager(fake, nil, func(context.Context, runtime.Endpoint) (bool, error) {
		close(idleStarted)
		<-allowIdle
		return true, nil
	})
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- manager.Pause(context.Background()) }()
	<-idleStarted
	requestDone := make(chan error, 1)
	go func() {
		_, release, err := manager.AcquireRequest(context.Background(), RequestWork)
		release()
		requestDone <- err
	}()
	select {
	case err := <-requestDone:
		t.Fatalf("request crossed pause check: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowIdle)
	if err := <-pauseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 1 || fake.resumeN != 1 {
		t.Fatalf("pause calls=%d resume calls=%d", fake.pauseN, fake.resumeN)
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

func TestPausePreservesNegotiatedProtocolAcrossFreshStatus(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	fake.endpoint.Protocol = runtime.ProtocolV2
	statusEndpoint := fake.endpoint
	statusEndpoint.Protocol = ""
	fake.statusEndpoint = &statusEndpoint
	var checked runtime.Protocol
	manager := NewManager(context.Background(), fake, runtime.Spec{
		Name: "demo", Protocol: runtime.ProtocolAuto,
	}, nil, func(_ context.Context, endpoint runtime.Endpoint) (bool, error) {
		checked = endpoint.Protocol
		return true, nil
	}, nil)
	if _, err := manager.EnsureRunning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checked != runtime.ProtocolV2 {
		t.Fatalf("authoritative check protocol = %q, want v2", checked)
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
	_, release, err := manager.AcquireRequest(context.Background(), RequestRead)
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
	if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); err == nil {
		t.Fatal("manager accepted request after Close")
	}
}

func newTestManager(fake lifecycleRuntime, observe EndpointObserver, idle IdleChecker) *Manager {
	return NewManager(context.Background(), fake, runtime.Spec{Name: "demo"}, observe, idle, nil)
}

func alwaysIdle(context.Context, runtime.Endpoint) (bool, error) { return true, nil }
