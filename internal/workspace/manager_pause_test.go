package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

type pauseRecoveryRuntime struct {
	mu       sync.Mutex
	state    runtime.State
	endpoint runtime.Endpoint
	pauseErr error
	running  bool
	onPause  func()
	ensureN  int
	pauseN   int
	prepareN int
}

func (f *pauseRecoveryRuntime) EnsureRunningObserved(ctx context.Context, spec runtime.Spec) (runtime.RunningResult, error) {
	ep, transitioned, err := f.EnsureRunning(ctx, spec)
	return runtime.RunningResult{Observation: runtime.Observation{
		State: runtime.StateRunning, Running: true, Endpoint: ep, HasEndpoint: true, ImageID: testImageID,
	}, Transitioned: transitioned}, err
}

func (f *pauseRecoveryRuntime) EnsureRunning(context.Context, runtime.Spec) (runtime.Endpoint, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureN++
	f.state = runtime.StateRunning
	return f.endpoint, true, nil
}

func (f *pauseRecoveryRuntime) ReconcileStartup(context.Context, runtime.Spec) (runtime.StartupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == runtime.StateAbsent || f.state == runtime.StatePaused {
		return runtime.StartupResult{}, nil
	}
	return runtime.StartupResult{Endpoint: f.endpoint, ImageID: testImageID, Running: true, Transitioned: f.state == runtime.StateProvisioning}, nil
}

func (f *pauseRecoveryRuntime) Pause(context.Context, string) error {
	f.mu.Lock()
	f.pauseN++
	onPause := f.onPause
	err := f.pauseErr
	f.mu.Unlock()
	if onPause != nil {
		onPause()
	}
	return err
}

func (f *pauseRecoveryRuntime) Status(context.Context, string) (runtime.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runtime.Observation{
		State:       f.state,
		ImageID:     testImageID,
		Running:     f.running,
		Endpoint:    f.endpoint,
		HasEndpoint: f.state == runtime.StateRunning,
	}, nil
}

func (f *pauseRecoveryRuntime) PrepareShutdown(context.Context, string) error {
	f.mu.Lock()
	f.prepareN++
	f.mu.Unlock()
	return nil
}

func TestPauseRefusesRunningProvisioningWorkspace(t *testing.T) {
	t.Parallel()
	fake := &pauseRecoveryRuntime{
		state: runtime.StateProvisioning, running: true,
		endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
	}
	manager := newTestManager(fake, nil, alwaysIdle)
	manager.publishEndpoint(fake.endpoint, testImageID)
	if err := manager.Pause(context.Background()); err == nil {
		t.Fatal("Pause stopped a running provisioning workspace without an idle snapshot")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.pauseN != 0 {
		t.Fatalf("runtime pause calls = %d, want 0", fake.pauseN)
	}
}

func TestPauseAttemptInvalidatesEndpointAndReopensAdmission(t *testing.T) {
	pauseErr := errors.New("pause failed")
	tests := []struct {
		name  string
		state runtime.State
		err   error
	}{
		{name: "running success", state: runtime.StateRunning},
		{name: "running error", state: runtime.StateRunning, err: pauseErr},
		{name: "provisioning error", state: runtime.StateProvisioning, err: pauseErr},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &pauseRecoveryRuntime{
				state:    test.state,
				endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
				pauseErr: test.err,
			}
			manager := newTestManager(fake, nil, alwaysIdle)
			manager.publishEndpoint(fake.endpoint, testImageID)

			err := manager.Pause(context.Background())
			if err != test.err {
				t.Fatalf("Pause error = %v, want original error %v", err, test.err)
			}
			if _, _, err := manager.AcquireRequest(context.Background(), RequestObserve); !errors.Is(err, ErrNotRunning) {
				t.Fatalf("cached endpoint after pause attempt error = %v, want ErrNotRunning", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			target, release, err := manager.AcquireRequest(ctx, RequestRead)
			if err != nil {
				t.Fatalf("request after pause attempt failed: %v", err)
			}
			release()
			if target.Endpoint != fake.endpoint {
				t.Fatalf("fresh target endpoint = %+v, want %+v", target.Endpoint, fake.endpoint)
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.pauseN != 1 || fake.ensureN != 1 {
				t.Fatalf("runtime calls: pause=%d ensure=%d, want 1 each", fake.pauseN, fake.ensureN)
			}
		})
	}
}

func TestPauseInvalidationDoesNotEraseNewerGeneration(t *testing.T) {
	oldEndpoint := runtime.Endpoint{Host: "127.0.0.1", Port: 4096}
	newEndpoint := runtime.Endpoint{Host: "127.0.0.1", Port: 4097}
	pauseErr := errors.New("pause result unknown")
	fake := &pauseRecoveryRuntime{
		state:    runtime.StateRunning,
		endpoint: oldEndpoint,
		pauseErr: pauseErr,
	}
	manager := newTestManager(fake, nil, alwaysIdle)
	oldTarget := manager.publishEndpoint(oldEndpoint, testImageID)
	fake.onPause = func() {
		manager.publishEndpoint(newEndpoint, "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	}

	if err := manager.Pause(context.Background()); err != pauseErr {
		t.Fatalf("Pause error = %v, want original error %v", err, pauseErr)
	}
	current, _, err := manager.AcquireRequest(context.Background(), RequestObserve)
	if err != nil {
		t.Fatalf("new endpoint was invalidated: %v", err)
	}
	if current.Endpoint != newEndpoint || current.ImageID == oldTarget.ImageID || current.Generation <= oldTarget.Generation {
		t.Fatalf("current target = %+v, want newer endpoint %+v after generation %d", current, newEndpoint, oldTarget.Generation)
	}
}

func TestAcquirePausedHoldsAdmissionUntilRelease(t *testing.T) {
	fake := &pauseRecoveryRuntime{
		state: runtime.StateRunning, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
	}
	manager := newTestManager(fake, nil, alwaysIdle)
	manager.publishEndpoint(fake.endpoint, testImageID)
	releaseFence, err := manager.AcquirePaused(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, _, err := manager.AcquireRequest(ctx, RequestRead); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request crossed held pause fence: %v", err)
	}
	releaseFence()
	releaseFence()

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, releaseRequest, err := manager.AcquireRequest(ctx, RequestRead)
	if err != nil {
		t.Fatalf("request after fence release failed: %v", err)
	}
	releaseRequest()
}

func TestAcquireQuiescedObservesTwiceThenRetainsFence(t *testing.T) {
	fake := &pauseRecoveryRuntime{
		state: runtime.StateRunning, running: true, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
	}
	manager := newTestManager(fake, nil, alwaysIdle)
	want := manager.publishEndpoint(fake.endpoint, testImageID)
	observations := 0
	releaseFence, err := manager.AcquireQuiesced(context.Background(), func(_ context.Context, target RequestTarget) error {
		observations++
		if target != want {
			t.Fatalf("target = %+v, want %+v", target, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if observations != 2 || fake.pauseN != 1 {
		t.Fatalf("observations=%d pauses=%d", observations, fake.pauseN)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, _, err := manager.AcquireRequest(ctx, RequestRead); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request crossed quiesced fence: %v", err)
	}
	releaseFence()
	releaseFence()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, releaseRequest, err := manager.AcquireRequest(ctx, RequestRead)
	if err != nil {
		t.Fatalf("request after quiesced fence release: %v", err)
	}
	releaseRequest()
}

func TestAcquireQuiescedFailureDoesNotStopAndReopensAdmission(t *testing.T) {
	fake := &pauseRecoveryRuntime{
		state: runtime.StateRunning, running: true, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096},
	}
	manager := newTestManager(fake, nil, alwaysIdle)
	manager.publishEndpoint(fake.endpoint, testImageID)
	wantErr := errors.New("terminal observation inconclusive")
	if _, err := manager.AcquireQuiesced(context.Background(), func(context.Context, RequestTarget) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("observation error = %v", err)
	}
	if fake.pauseN != 0 {
		t.Fatalf("pause calls = %d", fake.pauseN)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := manager.AcquireRequest(ctx, RequestRead)
	if err != nil {
		t.Fatalf("request after failed observation: %v", err)
	}
	release()
}

func TestClosePreparesOrderlyShutdownRecovery(t *testing.T) {
	fake := &pauseRecoveryRuntime{state: runtime.StateRunning}
	manager := newTestManager(fake, nil, alwaysIdle)
	if err := manager.PrepareShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.prepareN != 1 {
		t.Fatalf("shutdown preparations = %d, want 1", fake.prepareN)
	}
}
