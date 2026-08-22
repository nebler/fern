package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

const testImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeRuntime struct {
	mu             sync.Mutex
	state          runtime.State
	createN        int
	ensureN        int
	startupN       int
	resumeN        int
	pauseN         int
	statusN        int
	createWait     time.Duration
	createErr      error
	endpoint       runtime.Endpoint
	imageID        string
	statusEndpoint *runtime.Endpoint
	ensureResult   *runtime.RunningResult
	ensureErr      error
	startupResult  *runtime.StartupResult
	startupErr     error
	pauseCtxErr    error
	createBlock    chan struct{}
	createStart    chan struct{}
	respectCtx     bool
}

func newFakeRuntime(state runtime.State) *fakeRuntime {
	return &fakeRuntime{state: state, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}, imageID: testImageID}
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

func (f *fakeRuntime) EnsureRunningObserved(ctx context.Context, spec runtime.Spec) (runtime.RunningResult, error) {
	f.mu.Lock()
	if f.ensureResult != nil || f.ensureErr != nil {
		f.ensureN++
		result, err := runtime.RunningResult{}, f.ensureErr
		if f.ensureResult != nil {
			result = *f.ensureResult
		}
		f.mu.Unlock()
		return result, err
	}
	f.mu.Unlock()
	ep, transitioned, err := f.EnsureRunning(ctx, spec)
	if err != nil {
		return runtime.RunningResult{}, err
	}
	f.mu.Lock()
	imageID := f.imageID
	f.mu.Unlock()
	return runtime.RunningResult{Observation: runtime.Observation{
		State: runtime.StateRunning, Running: true, Endpoint: ep, HasEndpoint: true, ImageID: imageID,
	}, Transitioned: transitioned}, nil
}

func (f *fakeRuntime) ReconcileStartup(ctx context.Context, spec runtime.Spec) (runtime.StartupResult, error) {
	f.mu.Lock()
	f.startupN++
	if f.startupResult != nil || f.startupErr != nil {
		result, err := runtime.StartupResult{}, f.startupErr
		if f.startupResult != nil {
			result = *f.startupResult
		}
		f.mu.Unlock()
		return result, err
	}
	state := f.state
	f.mu.Unlock()
	switch state {
	case runtime.StateAbsent, runtime.StatePaused:
		return runtime.StartupResult{}, nil
	case runtime.StateRunning:
		return runtime.StartupResult{Endpoint: f.endpoint, ImageID: f.imageID, Running: true}, nil
	case runtime.StateProvisioning:
		ep, err := f.Resume(ctx, spec)
		return runtime.StartupResult{Endpoint: ep, ImageID: f.imageID, Running: err == nil, Transitioned: true}, err
	default:
		return runtime.StartupResult{}, runtime.ErrFailed
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

func TestReconcileStartupPreservesDormantWorkspace(t *testing.T) {
	for _, state := range []runtime.State{runtime.StateAbsent, runtime.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			fake := newFakeRuntime(state)
			observed := 0
			manager := newTestManager(fake, func(context.Context, runtime.Endpoint, bool) error {
				observed++
				return nil
			}, alwaysIdle)
			if err := manager.ReconcileStartup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("dormant observation error = %v, want ErrNotRunning", err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.startupN != 1 || fake.ensureN != 0 || fake.resumeN != 0 || observed != 0 {
				t.Fatalf("startup=%d ensure=%d resume=%d observed=%d", fake.startupN, fake.ensureN, fake.resumeN, observed)
			}
		})
	}
}

func TestReconcileStartupObservesAndPublishesActiveWorkspace(t *testing.T) {
	for _, test := range []struct {
		state runtime.State
		force bool
	}{
		{state: runtime.StateRunning},
		{state: runtime.StateProvisioning, force: true},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			fake := newFakeRuntime(test.state)
			observed := 0
			manager := newTestManager(fake, func(_ context.Context, ep runtime.Endpoint, force bool) error {
				observed++
				if ep != fake.endpoint || force != test.force {
					t.Fatalf("observer endpoint=%+v force=%t", ep, force)
				}
				return nil
			}, alwaysIdle)
			if err := manager.ReconcileStartup(context.Background()); err != nil {
				t.Fatal(err)
			}
			target, _, err := manager.AcquireRequest(context.Background(), RequestObserve)
			if err != nil {
				t.Fatal(err)
			}
			if target.Endpoint != fake.endpoint || target.ImageID != testImageID || target.Generation == 0 || observed != 1 {
				t.Fatalf("target=%+v observed=%d", target, observed)
			}
		})
	}
}

func TestReconcileStartupRefusesUnattestedTarget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		result runtime.StartupResult
		err    error
	}{
		{name: "inspection failure", err: errors.New("inspect unavailable")},
		{name: "missing image", result: runtime.StartupResult{Endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}, Running: true}},
		{name: "malformed image", result: runtime.StartupResult{Endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}, ImageID: "image:test", Running: true}},
		{name: "missing endpoint", result: runtime.StartupResult{ImageID: testImageID, Running: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeRuntime(runtime.StateRunning)
			fake.startupResult = &test.result
			fake.startupErr = test.err
			observed := 0
			manager := newTestManager(fake, func(context.Context, runtime.Endpoint, bool) error {
				observed++
				return nil
			}, alwaysIdle)
			if err := manager.ReconcileStartup(context.Background()); err == nil {
				t.Fatal("startup unexpectedly published an unattested target")
			}
			if observed != 0 {
				t.Fatalf("observer called %d times", observed)
			}
			if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("unattested startup target was cached: %v", err)
			}
		})
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
		ImageID:     f.imageID,
		Running:     f.state == runtime.StateRunning,
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

func TestCoalescedRequestsCarryOneStableAttestedTarget(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateAbsent)
	fake.createBlock = make(chan struct{})
	fake.createStart = make(chan struct{})
	manager := newTestManager(fake, nil, alwaysIdle)
	const callers = 12
	targets := make(chan RequestTarget, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			target, release, err := manager.AcquireRequest(context.Background(), RequestRead)
			release()
			targets <- target
			errs <- err
		}()
	}
	<-fake.createStart
	close(fake.createBlock)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		target := <-targets
		if target.Endpoint != fake.endpoint || target.ImageID != testImageID || target.Generation != 1 {
			t.Fatalf("coalesced target = %+v", target)
		}
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
		if target.Endpoint != fake.endpoint || target.ImageID != testImageID || target.Generation == 0 {
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
	fake.mu.Lock()
	fake.imageID = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	fake.mu.Unlock()
	second, release, err := manager.AcquireRequest(context.Background(), RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if second.Generation == first.Generation {
		t.Fatalf("generation was not advanced: first=%+v second=%+v", first, second)
	}
	if second.ImageID == first.ImageID {
		t.Fatalf("replacement did not update image coherently: first=%+v second=%+v", first, second)
	}
	stale := first
	stale.ImageID = second.ImageID
	manager.InvalidateEndpoint(stale)
	current, _, err := manager.AcquireRequest(context.Background(), RequestObserve)
	if err != nil {
		t.Fatal(err)
	}
	if current != second {
		t.Fatalf("stale failure invalidated new generation: current=%+v second=%+v", current, second)
	}
}

func TestMutableConfiguredImageDoesNotReplaceActualImageAttestation(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	spec := runtime.Spec{Name: "demo", Image: "registry.example/workspace:latest"}
	manager := NewManager(context.Background(), fake, spec, nil, alwaysIdle, nil)
	target, release, err := manager.AcquireRequest(context.Background(), RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if target.ImageID != fake.imageID || target.ImageID == spec.Image {
		t.Fatalf("request target image = %q, actual=%q configured=%q", target.ImageID, fake.imageID, spec.Image)
	}
}

func TestWakeRefusesUnattestedTargets(t *testing.T) {
	t.Parallel()
	ep := runtime.Endpoint{Host: "127.0.0.1", Port: 4096}
	tests := []struct {
		name        string
		observation runtime.Observation
		err         error
	}{
		{name: "status failure", err: errors.New("inspect unavailable")},
		{name: "wrong state", observation: runtime.Observation{State: runtime.StatePaused, Running: true, Endpoint: ep, HasEndpoint: true, ImageID: testImageID}},
		{name: "not running", observation: runtime.Observation{State: runtime.StateRunning, Endpoint: ep, HasEndpoint: true, ImageID: testImageID}},
		{name: "endpoint mismatch", observation: runtime.Observation{State: runtime.StateRunning, Running: true, Endpoint: ep, ImageID: testImageID}},
		{name: "missing endpoint", observation: runtime.Observation{State: runtime.StateRunning, Running: true, HasEndpoint: true, ImageID: testImageID}},
		{name: "missing image", observation: runtime.Observation{State: runtime.StateRunning, Running: true, Endpoint: ep, HasEndpoint: true}},
		{name: "malformed image", observation: runtime.Observation{State: runtime.StateRunning, Running: true, Endpoint: ep, HasEndpoint: true, ImageID: "image:test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeRuntime(runtime.StateRunning)
			fake.ensureResult = &runtime.RunningResult{Observation: test.observation}
			fake.ensureErr = test.err
			observed := 0
			manager := newTestManager(fake, func(context.Context, runtime.Endpoint, bool) error {
				observed++
				return nil
			}, alwaysIdle)
			if _, _, err := manager.AcquireRequest(context.Background(), RequestRead); err == nil {
				t.Fatal("wake unexpectedly published an unattested target")
			}
			if observed != 0 {
				t.Fatalf("observer called %d times for unattested target", observed)
			}
			if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("unattested target was cached: %v", err)
			}
		})
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
	manager.wakeMu.Lock()
	defer manager.wakeMu.Unlock()
	if manager.endpoint != (runtime.Endpoint{}) || manager.imageID != "" || manager.endpointGeneration != 0 || manager.hasEndpoint {
		t.Fatalf("pause left cached target endpoint=%+v image=%q generation=%d", manager.endpoint, manager.imageID, manager.endpointGeneration)
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
	var firstIdle sync.Once
	manager := newTestManager(fake, nil, func(context.Context, runtime.Endpoint) (bool, error) {
		firstIdle.Do(func() {
			close(idleStarted)
			<-allowIdle
		})
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

func TestActivityStartingBetweenSnapshotsDefersPause(t *testing.T) {
	t.Parallel()
	fake := newFakeRuntime(runtime.StateRunning)
	checks := 0
	manager := newTestManager(fake, nil, func(context.Context, runtime.Endpoint) (bool, error) {
		checks++
		return checks == 1, nil
	})
	if err := manager.Pause(context.Background()); !errors.Is(err, ErrSessionsActive) {
		t.Fatalf("Pause error = %v, want %v", err, ErrSessionsActive)
	}
	if checks != 2 {
		t.Fatalf("idle checks = %d, want 2", checks)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 0 {
		t.Fatalf("runtime pause calls = %d, want 0", fake.pauseN)
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
	manager.wakeMu.Lock()
	defer manager.wakeMu.Unlock()
	if manager.endpoint != (runtime.Endpoint{}) || manager.imageID != "" || manager.endpointGeneration != 0 || manager.hasEndpoint {
		t.Fatalf("close left cached target endpoint=%+v image=%q generation=%d", manager.endpoint, manager.imageID, manager.endpointGeneration)
	}
}

func newTestManager(fake lifecycleRuntime, observe EndpointObserver, idle IdleChecker) *Manager {
	return NewManager(context.Background(), fake, runtime.Spec{Name: "demo"}, observe, idle, nil)
}

func alwaysIdle(context.Context, runtime.Endpoint) (bool, error) { return true, nil }
