package taskresultcoord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskresult"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const (
	workspaceID = task.WorkspaceID("wsp_01890a5d-ac00-7000-8000-000000000001")
	taskID      = task.TaskID("tsk_01890a5d-ac00-7000-8000-000000000002")
	attemptID   = task.AttemptID("att_01890a5d-ac00-7000-8000-000000000003")
	resultID    = task.ResultID("res_01890a5d-ac00-7000-8000-000000000004")
	resultEvent = task.EventID("fev_01890a5d-ac00-7000-8000-000000000005")
	taskEvent   = task.EventID("fev_01890a5d-ac00-7000-8000-000000000006")
	sessionID   = task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef")
	messageID   = task.OpenCodeMessageID("msg_0123456789abcdef0123456789abcdef")
	baseSHA     = task.GitOID("1111111111111111111111111111111111111111")
	treeSHA     = task.GitOID("2222222222222222222222222222222222222222")
)

var fixedTime = time.Unix(1_700_000_100, 123_000_000).UTC()

type fakeStore struct {
	mu     sync.Mutex
	find   func(context.Context, int) (taskstore.DeliveryWork, error)
	seal   func(context.Context, taskstore.SealResultParams) error
	finds  int
	seals  int
	params taskstore.SealResultParams
}

type fakeAuthorizedStore struct {
	*fakeStore
	work             taskstore.SealRequestWork
	claimErr         error
	inspect          func(taskstore.SealRequestWork) taskstore.SealRequestWork
	claims           int
	inspects         int
	rejections       int
	authorizedSeals  int
	authorized       taskstore.SealAuthorizedResultParams
	onAuthorizedSeal func()
	authorizedErr    error
}

func (store *fakeAuthorizedStore) ClaimSealRequest(context.Context, taskstore.ClaimSealRequestParams) (taskstore.SealRequestWork, error) {
	store.claims++
	if store.claimErr != nil {
		return taskstore.SealRequestWork{}, store.claimErr
	}
	return store.work, nil
}

func (store *fakeAuthorizedStore) InspectClaimedSealRequest(context.Context, task.SealRequestID, string, int64) (taskstore.SealRequestWork, error) {
	store.inspects++
	if store.inspect != nil {
		return store.inspect(store.work), nil
	}
	return store.work, nil
}

func (store *fakeAuthorizedStore) RejectSealRequest(_ context.Context, p taskstore.RejectSealRequestParams) (taskstore.SealRequest, error) {
	store.rejections++
	request := store.work.Request
	request.State = taskstore.SealRequestRejected
	request.RejectedReason = p.Reason
	return request, nil
}

func (store *fakeAuthorizedStore) SealAuthorizedResult(_ context.Context, p taskstore.SealAuthorizedResultParams) (taskstore.SealedResult, error) {
	store.authorizedSeals++
	store.authorized = p
	if store.onAuthorizedSeal != nil {
		store.onAuthorizedSeal()
	}
	return taskstore.SealedResult{}, store.authorizedErr
}

func (store *fakeStore) FindSucceededUnsealedAttempt(ctx context.Context, _ task.WorkspaceID) (taskstore.DeliveryWork, error) {
	store.mu.Lock()
	store.finds++
	call := store.finds
	find := store.find
	store.mu.Unlock()
	if find != nil {
		return find(ctx, call)
	}
	return succeededWork(), nil
}

func (store *fakeStore) SealResult(ctx context.Context, params taskstore.SealResultParams) (taskstore.SealedResult, error) {
	store.mu.Lock()
	store.seals++
	store.params = params
	seal := store.seal
	store.mu.Unlock()
	if seal != nil {
		return taskstore.SealedResult{}, seal(ctx, params)
	}
	return taskstore.SealedResult{}, nil
}

type fakeFencer struct {
	mu            sync.Mutex
	calls         int
	pausedCalls   int
	quiescedCalls int
	observations  int
	releases      int
	held          bool
	onObserved    func(int)
}

func (fencer *fakeFencer) AcquirePaused(context.Context) (func(), error) {
	fencer.mu.Lock()
	fencer.calls++
	fencer.pausedCalls++
	fencer.held = true
	fencer.mu.Unlock()
	var once sync.Once
	return func() { once.Do(fencer.release) }, nil
}

func (fencer *fakeFencer) AcquireQuiesced(ctx context.Context, observe func(context.Context, workspace.RequestTarget) error) (func(), error) {
	fencer.mu.Lock()
	fencer.calls++
	fencer.quiescedCalls++
	fencer.held = true
	fencer.mu.Unlock()
	for index := 1; index <= 2; index++ {
		if err := observe(ctx, workspace.RequestTarget{ImageID: "sha256:attested", Generation: 7}); err != nil {
			fencer.release()
			return nil, err
		}
		fencer.mu.Lock()
		fencer.observations++
		onObserved := fencer.onObserved
		fencer.mu.Unlock()
		if onObserved != nil {
			onObserved(index)
		}
	}
	var once sync.Once
	return func() { once.Do(fencer.release) }, nil
}

func (fencer *fakeFencer) release() {
	fencer.mu.Lock()
	fencer.held = false
	fencer.releases++
	fencer.mu.Unlock()
}

func (fencer *fakeFencer) isHeld() bool {
	fencer.mu.Lock()
	defer fencer.mu.Unlock()
	return fencer.held
}

type fakeObserver struct {
	mu      sync.Mutex
	calls   int
	observe func(context.Context, workspace.RequestTarget, SuccessIdentity, int) (Observation, error)
}

func (observer *fakeObserver) ObserveSucceeded(ctx context.Context, target workspace.RequestTarget, identity SuccessIdentity) (Observation, error) {
	observer.mu.Lock()
	observer.calls++
	call := observer.calls
	observe := observer.observe
	observer.mu.Unlock()
	if observe != nil {
		return observe(ctx, target, identity, call)
	}
	return validObservation(identity), nil
}

type fakeCollector struct {
	mu      sync.Mutex
	calls   int
	request taskresult.Request
	collect func(context.Context, taskresult.Request) (taskresult.Result, error)
}

func (collector *fakeCollector) Collect(ctx context.Context, request taskresult.Request) (taskresult.Result, error) {
	collector.mu.Lock()
	collector.calls++
	collector.request = request
	collect := collector.collect
	collector.mu.Unlock()
	if collect != nil {
		return collect(ctx, request)
	}
	return collectedResult(request), nil
}

type fakeIDs struct {
	mu          sync.Mutex
	resultCalls int
	eventCalls  int
	resultErr   error
	eventErr    error
}

func (ids *fakeIDs) ResultID() (task.ResultID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.resultCalls++
	if ids.resultErr != nil {
		return "", ids.resultErr
	}
	return resultID, nil
}

func (ids *fakeIDs) EventID() (task.EventID, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.eventCalls++
	if ids.eventErr != nil {
		return "", ids.eventErr
	}
	if ids.eventCalls == 1 {
		return resultEvent, nil
	}
	return taskEvent, nil
}

func TestRunOnceSuccessUsesExactProofAndRetainsFenceThroughSeal(t *testing.T) {
	store := &fakeStore{}
	fencer := &fakeFencer{}
	observer := &fakeObserver{}
	collector := &fakeCollector{}
	ids := &fakeIDs{}
	store.seal = func(_ context.Context, _ taskstore.SealResultParams) error {
		if !fencer.isHeld() {
			t.Fatal("fence released before SealResult returned")
		}
		return nil
	}
	coordinator := newCoordinator(t, store, fencer, collector, observer, ids)

	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if observer.calls != 2 || fencer.observations != 2 {
		t.Fatalf("observations: observer=%d fencer=%d", observer.calls, fencer.observations)
	}
	if store.finds != 2 || store.seals != 1 || collector.calls != 1 || fencer.releases != 1 || fencer.isHeld() {
		t.Fatalf("calls: finds=%d seals=%d collects=%d releases=%d held=%v", store.finds, store.seals, collector.calls, fencer.releases, fencer.isHeld())
	}
	work := succeededWork()
	params := store.params
	if params.ResultID != resultID || params.ResultEventID != resultEvent || params.TaskEventID != taskEvent ||
		params.TaskID != work.Task.ID || params.AttemptID != work.Attempt.ID ||
		params.ExpectedTaskRevision != work.Task.Revision || params.ExpectedAttemptRevision != work.Attempt.Revision ||
		params.RepositoryID != work.Task.RepositoryID || params.BaseSHA != work.Task.BaseSHA ||
		params.OpenCodeSessionID != sessionID || params.OpenCodeMessageID != messageID ||
		params.PolicyVersion != "result-v1" || params.Actor != testConfig().Actor {
		t.Fatalf("unexpected seal params: %+v", params)
	}
	if !reflect.DeepEqual(params.EvidencePayload, evidence()) || params.EvidenceSHA256 != sha256.Sum256(evidence()) || params.SealedAt != fixedTime {
		t.Fatalf("evidence/time not preserved: %+v", params)
	}
	if collector.request.RepositoryPath != "/srv/fern/repository" || collector.request.Repository != (task.RepositoryTuple{RepositoryID: 42, BaseSHA: baseSHA}) {
		t.Fatalf("unexpected collector request: %+v", collector.request)
	}
}

func TestRunOnceUserSealUsesDurableAuthorizationWithoutSuccessObservation(t *testing.T) {
	base := &fakeStore{}
	store := &fakeAuthorizedStore{fakeStore: base, work: authorizedWork()}
	fencer := &fakeFencer{}
	observer := &fakeObserver{}
	collector := &fakeCollector{}
	ids := &fakeIDs{}
	coordinator := newCoordinator(t, store, fencer, collector, observer, ids)
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claims != 1 || store.inspects != 1 || store.authorizedSeals != 1 || store.seals != 0 ||
		observer.calls != 0 || ids.resultCalls != 0 || ids.eventCalls != 0 || collector.calls != 1 {
		t.Fatalf("calls claim=%d inspect=%d authorized=%d legacy=%d observe=%d ids=%d/%d collect=%d",
			store.claims, store.inspects, store.authorizedSeals, store.seals, observer.calls, ids.resultCalls, ids.eventCalls, collector.calls)
	}
	if fencer.pausedCalls != 1 || fencer.quiescedCalls != 0 || fencer.releases != 1 {
		t.Fatalf("fences paused=%d quiesced=%d releases=%d", fencer.pausedCalls, fencer.quiescedCalls, fencer.releases)
	}
	params := store.authorized
	if params.SealRequestID != store.work.Request.ID || params.Result.CompletionAuthority != taskstore.SealAuthorityUser ||
		params.Result.ResultID != store.work.Request.ResultID || params.Result.ResultCommit != store.work.Request.ExpectedResultCommit ||
		params.Result.Authorizer == nil || *params.Result.Authorizer != store.work.Request.Authorizer {
		t.Fatalf("authorized params: %+v", params)
	}
	if collector.request.ExpectedSnapshot == nil || collector.request.ExpectedSnapshot.ResultCommit != store.work.Request.ExpectedResultCommit ||
		collector.request.ExpectedSnapshot.ManifestSHA256 != store.work.Request.ExpectedManifestSHA256 {
		t.Fatalf("collector was not constrained: %+v", collector.request)
	}
}

func TestAuthorizedSealLostCommitResponseDoesNotRepeatCompletedWork(t *testing.T) {
	store := &fakeAuthorizedStore{fakeStore: &fakeStore{}, work: authorizedWork(), authorizedErr: errors.New("commit response lost")}
	coordinator, err := NewAuthorized(store, &fakeFencer{}, &fakeCollector{}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(context.Background()); err == nil || err.Error() != "commit response lost" {
		t.Fatalf("lost response error = %v", err)
	}
	if store.authorizedSeals != 1 {
		t.Fatalf("seal calls = %d", store.authorizedSeals)
	}
	// A committed store no longer exposes the request after restart/retry. The
	// coordinator must not fall back to execution-success discovery.
	store.claimErr = taskstore.ErrNotFound
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("retry error = %v", err)
	}
	if store.authorizedSeals != 1 || store.seals != 0 {
		t.Fatalf("authorized seals=%d legacy seals=%d", store.authorizedSeals, store.seals)
	}
}

type managerRuntime struct {
	mu       sync.Mutex
	state    runtime.State
	endpoint runtime.Endpoint
	ensureN  int
	pauseN   int
}

func (runtimeFake *managerRuntime) EnsureRunningObserved(context.Context, runtime.Spec) (runtime.RunningResult, error) {
	runtimeFake.mu.Lock()
	defer runtimeFake.mu.Unlock()
	runtimeFake.ensureN++
	runtimeFake.state = runtime.StateRunning
	return runtime.RunningResult{Observation: runtime.Observation{
		State: runtime.StateRunning, Running: true, Endpoint: runtimeFake.endpoint, HasEndpoint: true,
		ImageID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, Transitioned: true}, nil
}

func (runtimeFake *managerRuntime) ReconcileStartup(context.Context, runtime.Spec) (runtime.StartupResult, error) {
	return runtime.StartupResult{}, nil
}

func (runtimeFake *managerRuntime) Pause(context.Context, string) error {
	runtimeFake.mu.Lock()
	defer runtimeFake.mu.Unlock()
	runtimeFake.pauseN++
	runtimeFake.state = runtime.StatePaused
	return nil
}

func (runtimeFake *managerRuntime) Status(context.Context, string) (runtime.Observation, error) {
	runtimeFake.mu.Lock()
	defer runtimeFake.mu.Unlock()
	return runtime.Observation{
		State: runtimeFake.state, Running: runtimeFake.state == runtime.StateRunning,
		Endpoint: runtimeFake.endpoint, HasEndpoint: runtimeFake.state == runtime.StateRunning,
		ImageID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, nil
}

func TestAuthorizedSealCompletesThroughRealManagerWhileAlreadyPaused(t *testing.T) {
	runtimeFake := &managerRuntime{state: runtime.StateAbsent, endpoint: runtime.Endpoint{Host: "127.0.0.1", Port: 4096}}
	serviceContext, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	manager := workspace.NewManager(serviceContext, runtimeFake, runtime.Spec{Name: "test"}, nil,
		func(context.Context, runtime.Endpoint) (bool, error) { return true, nil }, nil)
	_, releaseRequest, err := manager.AcquireRequest(context.Background(), workspace.RequestRead)
	if err != nil {
		t.Fatal(err)
	}
	releaseRequest()
	if err := manager.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}

	store := &fakeAuthorizedStore{fakeStore: &fakeStore{}, work: authorizedWork()}
	store.onAuthorizedSeal = func() {
		requestContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, _, err := manager.AcquireRequest(requestContext, workspace.RequestRead); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request crossed result fence: %v", err)
		}
	}
	coordinator, err := NewAuthorized(store, manager, &fakeCollector{}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeFake.mu.Lock()
	defer runtimeFake.mu.Unlock()
	if runtimeFake.ensureN != 1 || runtimeFake.pauseN != 1 || runtimeFake.state != runtime.StatePaused || store.authorizedSeals != 1 {
		t.Fatalf("ensure=%d pause=%d state=%s seals=%d", runtimeFake.ensureN, runtimeFake.pauseN, runtimeFake.state, store.authorizedSeals)
	}
}

func TestRunOnceUserSealRejectsChangedRevisionAndSnapshot(t *testing.T) {
	t.Run("revision", func(t *testing.T) {
		store := &fakeAuthorizedStore{fakeStore: &fakeStore{}, work: authorizedWork()}
		store.inspect = func(work taskstore.SealRequestWork) taskstore.SealRequestWork {
			work.Preview.Task.Revision++
			return work
		}
		collector := &fakeCollector{}
		coordinator := newCoordinator(t, store, &fakeFencer{}, collector, &fakeObserver{}, &fakeIDs{})
		if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrSelectionChanged) {
			t.Fatalf("err=%v", err)
		}
		if store.rejections != 1 || store.authorizedSeals != 0 || collector.calls != 0 {
			t.Fatalf("rejections=%d seals=%d collects=%d", store.rejections, store.authorizedSeals, collector.calls)
		}
	})
	t.Run("head or manifest", func(t *testing.T) {
		store := &fakeAuthorizedStore{fakeStore: &fakeStore{}, work: authorizedWork()}
		collector := &fakeCollector{collect: func(_ context.Context, request taskresult.Request) (taskresult.Result, error) {
			result := collectedResult(request)
			result.Tuple.ResultCommit = task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			return result, nil
		}}
		coordinator := newCoordinator(t, store, &fakeFencer{}, collector, &fakeObserver{}, &fakeIDs{})
		if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrCollectionFailed) {
			t.Fatalf("err=%v", err)
		}
		if store.rejections != 1 || store.authorizedSeals != 0 {
			t.Fatalf("rejections=%d seals=%d", store.rejections, store.authorizedSeals)
		}
	})
}

func TestRunOnceCleanIdleAndRestartDoNotCreateResultWithoutAuthorization(t *testing.T) {
	base := &fakeStore{find: func(context.Context, int) (taskstore.DeliveryWork, error) {
		return taskstore.DeliveryWork{}, taskstore.ErrNotFound
	}}
	store := &fakeAuthorizedStore{fakeStore: base, claimErr: taskstore.ErrNotFound}
	collector := &fakeCollector{}
	fencer := &fakeFencer{}
	coordinator := newCoordinator(t, store, fencer, collector, &fakeObserver{}, &fakeIDs{})
	for range 2 {
		if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
			t.Fatalf("err=%v", err)
		}
	}
	if store.authorizedSeals != 0 || store.seals != 0 || collector.calls != 0 || fencer.calls != 0 {
		t.Fatalf("seals=%d/%d collects=%d fences=%d", store.authorizedSeals, store.seals, collector.calls, fencer.calls)
	}
}

func TestAuthorizedCoordinatorNeverFallsBackToSuccessDiscovery(t *testing.T) {
	base := &fakeStore{find: func(context.Context, int) (taskstore.DeliveryWork, error) {
		t.Fatal("authorized coordinator queried execution success")
		return taskstore.DeliveryWork{}, nil
	}}
	store := &fakeAuthorizedStore{fakeStore: base, claimErr: taskstore.ErrNotFound}
	coordinator, err := NewAuthorized(store, &fakeFencer{}, &fakeCollector{}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("RunOnce error = %v", err)
	}
}

func TestNewAuthorizedRequiresAuthorizedStore(t *testing.T) {
	if _, err := NewAuthorized(&fakeStore{}, &fakeFencer{}, &fakeCollector{}, testConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v", err)
	}
}

type pausedOnlyFencer struct{ calls int }

func (fencer *pausedOnlyFencer) AcquirePaused(context.Context) (func(), error) {
	fencer.calls++
	return func() {}, nil
}

func TestNewAuthorizedRequiresOnlyPausedFence(t *testing.T) {
	store := &fakeAuthorizedStore{fakeStore: &fakeStore{}, claimErr: taskstore.ErrNotFound}
	fencer := &pausedOnlyFencer{}
	coordinator, err := NewAuthorized(store, fencer, &fakeCollector{}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) || fencer.calls != 0 {
		t.Fatalf("RunOnce error=%v pause calls=%d", err, fencer.calls)
	}
}

func TestRunOnceNoWorkAvoidsFence(t *testing.T) {
	store := &fakeStore{find: func(context.Context, int) (taskstore.DeliveryWork, error) {
		return taskstore.DeliveryWork{}, taskstore.ErrNotFound
	}}
	fencer := &fakeFencer{}
	coordinator := newCoordinator(t, store, fencer, &fakeCollector{}, &fakeObserver{}, &fakeIDs{})
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("err=%v", err)
	}
	if fencer.calls != 0 {
		t.Fatalf("fencer called %d times", fencer.calls)
	}
}

func TestRunOnceObservationMismatchBlocksCollectionAndSeal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Observation)
	}{
		{"identity", func(observation *Observation) {
			observation.Identity.MessageID = task.OpenCodeMessageID("msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		}},
		{"evidence", func(observation *Observation) {
			observation.Evidence = json.RawMessage(`{"terminal":"different"}`)
			observation.EvidenceSHA256 = sha256.Sum256(observation.Evidence)
		}},
		{"digest", func(observation *Observation) { observation.EvidenceSHA256 = [32]byte{} }},
		{"policy", func(observation *Observation) { observation.PolicyVersion = "other-v1" }},
		{"invalid-json", func(observation *Observation) {
			observation.Evidence = json.RawMessage(`[]`)
			observation.EvidenceSHA256 = sha256.Sum256(observation.Evidence)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{}
			fencer := &fakeFencer{}
			collector := &fakeCollector{}
			observer := &fakeObserver{observe: func(_ context.Context, _ workspace.RequestTarget, identity SuccessIdentity, call int) (Observation, error) {
				observation := validObservation(identity)
				if call == 2 || test.name != "evidence" {
					test.mutate(&observation)
				}
				return observation, nil
			}}
			coordinator := newCoordinator(t, store, fencer, collector, observer, &fakeIDs{})
			if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrObservationMismatch) {
				t.Fatalf("err=%v", err)
			}
			if collector.calls != 0 || store.seals != 0 || fencer.releases != 1 || fencer.isHeld() {
				t.Fatalf("collects=%d seals=%d releases=%d held=%v", collector.calls, store.seals, fencer.releases, fencer.isHeld())
			}
		})
	}
}

func TestRunOnceSelectedTupleChangeUnderFenceBlocksCollection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*taskstore.DeliveryWork)
	}{
		{"attempt-revision", func(work *taskstore.DeliveryWork) { work.Attempt.Revision++ }},
		{"task-revision", func(work *taskstore.DeliveryWork) { work.Task.Revision++ }},
		{"repository", func(work *taskstore.DeliveryWork) { work.Task.RepositoryID++ }},
		{"base", func(work *taskstore.DeliveryWork) { work.Task.BaseSHA = treeSHA }},
		{"session", func(work *taskstore.DeliveryWork) {
			work.Attempt.OpenCodeSessionID = "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{find: func(_ context.Context, call int) (taskstore.DeliveryWork, error) {
				work := succeededWork()
				if call == 2 {
					test.mutate(&work)
				}
				return work, nil
			}}
			fencer := &fakeFencer{}
			collector := &fakeCollector{}
			coordinator := newCoordinator(t, store, fencer, collector, &fakeObserver{}, &fakeIDs{})
			if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrSelectionChanged) {
				t.Fatalf("err=%v", err)
			}
			if collector.calls != 0 || store.seals != 0 || fencer.releases != 1 {
				t.Fatalf("collects=%d seals=%d releases=%d", collector.calls, store.seals, fencer.releases)
			}
		})
	}
}

func TestRunOnceReleasesFenceOnEveryPostAcquisitionFailure(t *testing.T) {
	want := errors.New("failure")
	tests := []struct {
		name      string
		configure func(*fakeStore, *fakeCollector, *fakeIDs, *Config)
	}{
		{"reread", func(store *fakeStore, _ *fakeCollector, _ *fakeIDs, _ *Config) {
			store.find = func(_ context.Context, call int) (taskstore.DeliveryWork, error) {
				if call == 2 {
					return taskstore.DeliveryWork{}, want
				}
				return succeededWork(), nil
			}
		}},
		{"invalid-collection", func(_ *fakeStore, collector *fakeCollector, _ *fakeIDs, _ *Config) {
			collector.collect = func(_ context.Context, request taskresult.Request) (taskresult.Result, error) {
				result := collectedResult(request)
				result.OpenCodeMessageID = "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				return result, nil
			}
		}},
		{"result-id", func(_ *fakeStore, _ *fakeCollector, ids *fakeIDs, _ *Config) { ids.resultErr = want }},
		{"event-id", func(_ *fakeStore, _ *fakeCollector, ids *fakeIDs, _ *Config) { ids.eventErr = want }},
		{"clock", func(_ *fakeStore, _ *fakeCollector, _ *fakeIDs, config *Config) {
			config.Now = func() time.Time { return time.Time{} }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeStore{}
			fencer := &fakeFencer{}
			collector := &fakeCollector{}
			ids := &fakeIDs{}
			config := testConfig()
			test.configure(store, collector, ids, &config)
			coordinator, err := New(store, fencer, collector, &fakeObserver{}, ids, config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := coordinator.RunOnce(context.Background()); err == nil {
				t.Fatal("RunOnce unexpectedly succeeded")
			}
			if store.seals != 0 || fencer.releases != 1 || fencer.isHeld() {
				t.Fatalf("seals=%d releases=%d held=%v", store.seals, fencer.releases, fencer.isHeld())
			}
		})
	}
}

func TestRunOnceCollectionFailureReleasesWithoutIDsOrSeal(t *testing.T) {
	want := errors.New("repository output that must not escape")
	store := &fakeStore{}
	fencer := &fakeFencer{}
	collector := &fakeCollector{collect: func(context.Context, taskresult.Request) (taskresult.Result, error) { return taskresult.Result{}, want }}
	ids := &fakeIDs{}
	coordinator := newCoordinator(t, store, fencer, collector, &fakeObserver{}, ids)
	err := coordinator.RunOnce(context.Background())
	if !errors.Is(err, ErrCollectionFailed) || !errors.Is(err, want) || err.Error() != ErrCollectionFailed.Error() {
		t.Fatalf("err=%q", err)
	}
	if store.seals != 0 || ids.resultCalls != 0 || ids.eventCalls != 0 || fencer.releases != 1 || fencer.isHeld() {
		t.Fatalf("seals=%d ids=%d/%d releases=%d held=%v", store.seals, ids.resultCalls, ids.eventCalls, fencer.releases, fencer.isHeld())
	}
}

func TestRunOnceSensitiveEvidenceIsDelegatedToCollector(t *testing.T) {
	payload := json.RawMessage(`{"prompt":"secret"}`)
	observer := &fakeObserver{observe: func(_ context.Context, _ workspace.RequestTarget, identity SuccessIdentity, _ int) (Observation, error) {
		return Observation{Identity: identity, Evidence: payload, EvidenceSHA256: sha256.Sum256(payload), PolicyVersion: "result-v1"}, nil
	}}
	collector := &fakeCollector{collect: func(_ context.Context, request taskresult.Request) (taskresult.Result, error) {
		if !reflect.DeepEqual(request.EvidencePayload, payload) {
			t.Fatal("coordinator changed evidence before collector validation")
		}
		return taskresult.Result{}, taskresult.ErrInvalidRequest
	}}
	store := &fakeStore{}
	coordinator := newCoordinator(t, store, &fakeFencer{}, collector, observer, &fakeIDs{})
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrCollectionFailed) || !errors.Is(err, taskresult.ErrInvalidRequest) {
		t.Fatalf("err=%v", err)
	}
	if collector.calls != 1 || store.seals != 0 {
		t.Fatalf("collects=%d seals=%d", collector.calls, store.seals)
	}
}

func TestRunOnceSealErrorsAreNotRetried(t *testing.T) {
	for _, want := range []error{taskstore.ErrStaleRevision, context.Canceled, errors.New("ambiguous commit")} {
		t.Run(want.Error(), func(t *testing.T) {
			store := &fakeStore{seal: func(context.Context, taskstore.SealResultParams) error { return want }}
			fencer := &fakeFencer{}
			ids := &fakeIDs{}
			coordinator := newCoordinator(t, store, fencer, &fakeCollector{}, &fakeObserver{}, ids)
			err := coordinator.RunOnce(context.Background())
			if !errors.Is(err, want) {
				t.Fatalf("err=%v want=%v", err, want)
			}
			if store.seals != 1 || ids.resultCalls != 1 || ids.eventCalls != 2 || fencer.releases != 1 {
				t.Fatalf("seals=%d ids=%d/%d releases=%d", store.seals, ids.resultCalls, ids.eventCalls, fencer.releases)
			}
		})
	}
}

func TestRunOnceCancellationAfterCollectionUsesBoundedSealContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	collector := &fakeCollector{collect: func(_ context.Context, request taskresult.Request) (taskresult.Result, error) {
		cancel()
		return collectedResult(request), nil
	}}
	store := &fakeStore{seal: func(sealContext context.Context, _ taskstore.SealResultParams) error {
		if err := sealContext.Err(); err != nil {
			t.Fatalf("seal context inherited caller cancellation: %v", err)
		}
		deadline, ok := sealContext.Deadline()
		if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) <= 0 {
			t.Fatalf("seal deadline is not bounded: %v %v", deadline, ok)
		}
		return nil
	}}
	fencer := &fakeFencer{}
	coordinator := newCoordinator(t, store, fencer, collector, &fakeObserver{}, &fakeIDs{})
	if err := coordinator.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if store.seals != 1 || fencer.releases != 1 {
		t.Fatalf("seals=%d releases=%d", store.seals, fencer.releases)
	}
}

func TestRunOnceContextBeforeCollectionDoesNotSeal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &fakeStore{find: func(ctx context.Context, _ int) (taskstore.DeliveryWork, error) {
		return taskstore.DeliveryWork{}, ctx.Err()
	}}
	fencer := &fakeFencer{}
	coordinator := newCoordinator(t, store, fencer, &fakeCollector{}, &fakeObserver{}, &fakeIDs{})
	if err := coordinator.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if store.seals != 0 || fencer.calls != 0 {
		t.Fatalf("seals=%d fences=%d", store.seals, fencer.calls)
	}
}

func TestConcurrentRunOnceProducesOneSeal(t *testing.T) {
	store := &fakeStore{}
	store.find = func(_ context.Context, _ int) (taskstore.DeliveryWork, error) {
		store.mu.Lock()
		sealed := store.seals > 0
		store.mu.Unlock()
		if sealed {
			return taskstore.DeliveryWork{}, taskstore.ErrNotFound
		}
		return succeededWork(), nil
	}
	coordinator := newCoordinator(t, store, &fakeFencer{}, &fakeCollector{}, &fakeObserver{}, &fakeIDs{})
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errorsOut <- coordinator.RunOnce(context.Background())
		}()
	}
	close(start)
	first, second := <-errorsOut, <-errorsOut
	if first != nil && !errors.Is(first, ErrNoWork) || second != nil && !errors.Is(second, ErrNoWork) {
		t.Fatalf("errors: %v, %v", first, second)
	}
	if (first == nil) == (second == nil) || store.seals != 1 {
		t.Fatalf("errors: %v, %v; seals=%d", first, second, store.seals)
	}
}

func TestObserverFailureIsSanitizedAndFenceReleased(t *testing.T) {
	want := errors.New("raw OpenCode response")
	observer := &fakeObserver{observe: func(context.Context, workspace.RequestTarget, SuccessIdentity, int) (Observation, error) {
		return Observation{}, want
	}}
	fencer := &fakeFencer{}
	store := &fakeStore{}
	coordinator := newCoordinator(t, store, fencer, &fakeCollector{}, observer, &fakeIDs{})
	err := coordinator.RunOnce(context.Background())
	if !errors.Is(err, ErrObservationFailed) || !errors.Is(err, want) || err.Error() != ErrObservationFailed.Error() {
		t.Fatalf("err=%q", err)
	}
	if store.seals != 0 || fencer.releases != 1 || fencer.isHeld() {
		t.Fatalf("seals=%d releases=%d held=%v", store.seals, fencer.releases, fencer.isHeld())
	}
}

func TestCoordinatorHasNoPollingOrSuccessProjectionEntryPoint(t *testing.T) {
	typeOfCoordinator := reflect.TypeOf((*Coordinator)(nil))
	if typeOfCoordinator.NumMethod() != 1 {
		t.Fatalf("exported methods=%d, want only RunOnce", typeOfCoordinator.NumMethod())
	}
	if method, ok := typeOfCoordinator.MethodByName("RunOnce"); !ok || method.Name != "RunOnce" {
		t.Fatal("RunOnce is not the sole explicit entry point")
	}
}

func TestNewRejectsUnboundedHostConfiguration(t *testing.T) {
	valid := testConfig()
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"workspace", func(config *Config) { config.WorkspaceID = "bad" }},
		{"repository-relative", func(config *Config) { config.RepositoryPath = "repository" }},
		{"repository-unclean", func(config *Config) { config.RepositoryPath = "/srv/../repository" }},
		{"policy-empty", func(config *Config) { config.PolicyVersion = "" }},
		{"policy-control", func(config *Config) { config.PolicyVersion = "bad\n" }},
		{"timeout-zero", func(config *Config) { config.OperationTimeout = 0 }},
		{"timeout-large", func(config *Config) { config.OperationTimeout = MaxOperationTimeout + time.Nanosecond }},
		{"actor", func(config *Config) { config.Actor.ID = "" }},
		{"actor-type", func(config *Config) { config.Actor.Type = task.ActorDevice }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := New(&fakeStore{}, &fakeFencer{}, &fakeCollector{}, &fakeObserver{}, &fakeIDs{}, config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	if _, err := New(nil, &fakeFencer{}, &fakeCollector{}, &fakeObserver{}, &fakeIDs{}, valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil dependency err=%v", err)
	}
}

func newCoordinator(t *testing.T, store Store, fencer Fencer, collector Collector, observer Observer, ids IDGenerator) *Coordinator {
	t.Helper()
	coordinator, err := New(store, fencer, collector, observer, ids, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return coordinator
}

func testConfig() Config {
	return Config{
		WorkspaceID: workspaceID, RepositoryPath: "/srv/fern/repository", PolicyVersion: "result-v1",
		OperationTimeout: time.Minute,
		Actor:            task.ActorSnapshot{Type: task.ActorSystem, ID: "result-coordinator", DisplayName: "Result coordinator", CredentialID: "host-v1", Authentication: "internal", RequestID: "worker-1"},
		Now:              func() time.Time { return fixedTime },
	}
}

func succeededWork() taskstore.DeliveryWork {
	return taskstore.DeliveryWork{
		Task: taskstore.Task{
			ID: taskID, WorkspaceID: workspaceID, RepositoryID: 42, BaseSHA: baseSHA,
			State: task.TaskRunning, CurrentAttemptID: attemptID, Revision: 11,
		},
		Attempt: taskstore.Attempt{
			ID: attemptID, TaskID: taskID, WorkspaceID: workspaceID, State: task.AttemptSucceeded,
			OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID, BaseSHA: baseSHA, Revision: 13,
		},
	}
}

func authorizedWork() taskstore.SealRequestWork {
	expires := fixedTime.Add(time.Minute)
	manifestHash := sha256.Sum256([]byte("[]"))
	authorizer := task.ActorSnapshot{Type: task.ActorDevice, ID: "device-1", DisplayName: "Phone", CredentialID: "credential-1", Authentication: "cookie", RequestID: "seal-1"}
	return taskstore.SealRequestWork{
		Request: taskstore.SealRequest{
			ID: task.SealRequestID("slr_01890a5d-ac00-7000-8000-000000000007"), ReceiptID: task.ReceiptID("rcp_01890a5d-ac00-7000-8000-000000000008"),
			WorkspaceID: workspaceID, TaskID: taskID, AttemptID: attemptID, State: taskstore.SealRequestClaimed,
			CompletionAuthority: taskstore.SealAuthorityUser, ExpectedWorkspaceRevision: 3, ExpectedTaskRevision: 11,
			ExpectedAttemptRevision: 13, RepositoryID: 42, BaseSHA: baseSHA, ExpectedResultCommit: baseSHA,
			ExpectedTreeOID: treeSHA, ExpectedOutcome: task.ResultNoChanges, ExpectedManifestEntries: 0,
			ExpectedManifestSHA256: manifestHash, IdempotencyKey: "seal-once", Authorizer: authorizer,
			ExpectedWorktreeClean: true,
			ResultID:              resultID, ResultEventID: resultEvent, TaskEventID: taskEvent, ClaimOwner: "result-coordinator",
			ClaimExpiresAt: &expires, ClaimRevision: 1, AcceptedAt: fixedTime.Add(-2 * time.Second),
		},
		Preview: taskstore.SealPreview{
			Workspace: taskstore.Workspace{ID: workspaceID, State: taskstore.WorkspaceActive, RepositoryID: 42, Revision: 3},
			Task: taskstore.Task{ID: taskID, WorkspaceID: workspaceID, RepositoryID: 42, BaseSHA: baseSHA, State: task.TaskRunning,
				CurrentAttemptID: attemptID, Revision: 11},
			Attempt: taskstore.Attempt{ID: attemptID, TaskID: taskID, WorkspaceID: workspaceID, State: task.AttemptRunning,
				OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID, BaseSHA: baseSHA, Revision: 13},
		},
	}
}

func evidence() json.RawMessage { return json.RawMessage(`{"terminal":"succeeded"}`) }

func validObservation(identity SuccessIdentity) Observation {
	payload := evidence()
	return Observation{Identity: identity, Evidence: payload, EvidenceSHA256: sha256.Sum256(payload), PolicyVersion: "result-v1"}
}

func collectedResult(request taskresult.Request) taskresult.Result {
	return taskresult.Result{
		Tuple: task.ResultTuple{
			RepositoryTuple: request.Repository, ResultCommit: request.Repository.BaseSHA,
			Outcome: task.ResultNoChanges, ManifestEntries: 0, WorktreeClean: true,
		},
		TreeOID: treeSHA, Manifest: []taskstore.ManifestEntry{}, ManifestSHA256: sha256.Sum256([]byte("[]")),
		OpenCodeSessionID: request.OpenCodeSessionID, OpenCodeMessageID: request.OpenCodeMessageID,
		EvidencePayload: append(json.RawMessage(nil), request.EvidencePayload...), EvidenceSHA256: request.EvidenceSHA256,
		PolicyVersion: request.PolicyVersion, CollectedAt: fixedTime.Add(-time.Second),
	}
}
