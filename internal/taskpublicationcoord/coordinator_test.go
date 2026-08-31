package taskpublicationcoord

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskpublication"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	testBaseSHA   = task.GitOID("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	testResultSHA = task.GitOID("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	testOtherSHA  = task.GitOID("cccccccccccccccccccccccccccccccccccccccc")
)

type memoryStore struct {
	mu                sync.Mutex
	work              taskstore.PublicationWork
	terminal          bool
	stale             bool
	payloads          [][]byte
	findCalls         int
	find              func(int, taskstore.PublicationWork) (taskstore.PublicationWork, error)
	fencer            *fakeFencer
	mutations         int
	mutationsUnfenced int
}

func (store *memoryStore) FindPublicationWork(context.Context, task.WorkspaceID) (taskstore.PublicationWork, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.findCalls++
	if store.terminal {
		return taskstore.PublicationWork{}, taskstore.ErrNotFound
	}
	if store.find != nil {
		return store.find(store.findCalls, store.work)
	}
	return store.work, nil
}

func (store *memoryStore) AdvancePublication(_ context.Context, params taskstore.AdvancePublicationParams) (taskstore.PublicationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.recordMutation()
	if store.stale {
		return taskstore.PublicationRecord{}, taskstore.ErrStaleRevision
	}
	if params.ExpectedRevision != store.work.Publication.Revision || params.ExpectedTaskRevision != store.work.Task.Revision ||
		params.ExpectedAttemptRevision != store.work.Attempt.Revision || params.From != store.work.Publication.EffectPhase {
		return taskstore.PublicationRecord{}, taskstore.ErrStaleRevision
	}
	store.payloads = append(store.payloads, append([]byte(nil), params.EvidencePayload...))
	store.work.Publication.Revision++
	store.work.Publication.State = taskstore.PublicationRunning
	store.work.Publication.EffectPhase = params.To
	if params.ObservedRemoteSHA != "" {
		store.work.Publication.ObservedRemoteSHA = params.ObservedRemoteSHA
	}
	return taskstore.PublicationRecord{Publication: store.work.Publication}, nil
}

func (store *memoryStore) CompletePublication(_ context.Context, params taskstore.CompletePublicationParams) (taskstore.PublicationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.recordMutation()
	if params.ExpectedRevision != store.work.Publication.Revision || params.ExpectedTaskRevision != store.work.Task.Revision || params.ExpectedAttemptRevision != store.work.Attempt.Revision {
		return taskstore.PublicationRecord{}, taskstore.ErrStaleRevision
	}
	store.payloads = append(store.payloads, append([]byte(nil), params.EvidencePayload...))
	store.work.Publication.Revision++
	store.work.Publication.State = taskstore.PublicationPublished
	store.work.Publication.Observation = &params.Observation
	store.terminal = true
	return taskstore.PublicationRecord{Publication: store.work.Publication}, nil
}

func (store *memoryStore) RecoverPublication(_ context.Context, params taskstore.RecoverPublicationParams) (taskstore.PublicationRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.recordMutation()
	if params.ExpectedRevision != store.work.Publication.Revision || params.ExpectedTaskRevision != store.work.Task.Revision || params.ExpectedAttemptRevision != store.work.Attempt.Revision {
		return taskstore.PublicationRecord{}, taskstore.ErrStaleRevision
	}
	store.payloads = append(store.payloads, append([]byte(nil), params.EvidencePayload...))
	store.work.Publication.Revision++
	store.work.Publication.State = params.State
	store.work.Publication.Reason = params.Reason
	store.terminal = params.State != taskstore.PublicationUncertain
	return taskstore.PublicationRecord{Publication: store.work.Publication}, nil
}

func (store *memoryStore) recordMutation() {
	store.mutations++
	if store.fencer != nil && !store.fencer.isHeld() {
		store.mutationsUnfenced++
	}
}

type fakeFencer struct {
	mu        sync.Mutex
	held      bool
	acquires  int
	releases  int
	err       error
	onAcquire func()
}

func (fencer *fakeFencer) AcquirePaused(context.Context) (func(), error) {
	fencer.mu.Lock()
	fencer.acquires++
	if fencer.err != nil {
		err := fencer.err
		fencer.mu.Unlock()
		return nil, err
	}
	fencer.held = true
	onAcquire := fencer.onAcquire
	fencer.mu.Unlock()
	if onAcquire != nil {
		onAcquire()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			fencer.mu.Lock()
			fencer.held = false
			fencer.releases++
			fencer.mu.Unlock()
		})
	}, nil
}

func (fencer *fakeFencer) isHeld() bool {
	fencer.mu.Lock()
	defer fencer.mu.Unlock()
	return fencer.held
}

func (fencer *fakeFencer) counts() (int, int) {
	fencer.mu.Lock()
	defer fencer.mu.Unlock()
	return fencer.acquires, fencer.releases
}

type fakePublisher struct {
	mu                sync.Mutex
	branch            taskpublication.BranchObservation
	branchErr         error
	push              taskpublication.BranchProof
	pushErr           error
	pull              taskpublication.PullRequestProof
	pullErr           error
	create            taskpublication.PullRequestProof
	createErr         error
	reconcileBranches int
	pushes            int
	reconcilePulls    int
	creates           int
	fencer            *fakeFencer
	callWithoutFence  int
	request           taskpublication.Request
}

func (publisher *fakePublisher) ReconcileBranch(_ context.Context, request taskpublication.Request) (taskpublication.BranchObservation, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.recordCall(request)
	publisher.reconcileBranches++
	return publisher.branch, publisher.branchErr
}

func (publisher *fakePublisher) PushOnce(_ context.Context, request taskpublication.Request) (taskpublication.BranchProof, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.recordCall(request)
	publisher.pushes++
	return publisher.push, publisher.pushErr
}

func (publisher *fakePublisher) ReconcilePullRequest(_ context.Context, request taskpublication.Request) (taskpublication.PullRequestProof, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.recordCall(request)
	publisher.reconcilePulls++
	return publisher.pull, publisher.pullErr
}

func (publisher *fakePublisher) CreatePullRequestOnce(_ context.Context, request taskpublication.Request) (taskpublication.PullRequestProof, error) {
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	publisher.recordCall(request)
	publisher.creates++
	return publisher.create, publisher.createErr
}

func (publisher *fakePublisher) recordCall(request taskpublication.Request) {
	publisher.request = request
	if publisher.fencer != nil && !publisher.fencer.isHeld() {
		publisher.callWithoutFence++
	}
}

type trackingPublicationSource struct {
	path     string
	closeErr error
	acquires int
	closes   int
}

func (source *trackingPublicationSource) Acquire(context.Context, taskstore.Result) (string, func() error, error) {
	source.acquires++
	return source.path, func() error {
		source.closes++
		return source.closeErr
	}, nil
}

func TestNoWorkDoesNotAcquireFence(t *testing.T) {
	store, _, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	store.terminal = true
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatalf("error = %v", err)
	}
	acquires, releases := store.fencer.counts()
	if acquires != 0 || releases != 0 {
		t.Fatalf("acquires=%d releases=%d", acquires, releases)
	}
}

func TestCoordinatorConsumesReceiptAdmittedPublication(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	store.work.Publication.AdmissionReceiptID = task.ReceiptID("rcp_0198d34d-5e40-7c5a-8e3f-6bfad471ae19")
	publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.work.Publication.EffectPhase != taskstore.PublicationPhasePushObserved || store.mutations != 2 || publisher.pushes != 1 {
		t.Fatalf("admitted publication was not consumed: publication=%+v mutations=%d pushes=%d",
			store.work.Publication, store.mutations, publisher.pushes)
	}
}

func TestCoordinatorUsesAndClosesSelectedPublicationSource(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	source := &trackingPublicationSource{path: "/private/retained-checkout"}
	coordinator.source = source
	publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.acquires != 1 || source.closes != 1 || publisher.request.RepositoryPath != source.path || store.mutations != 2 {
		t.Fatalf("acquires=%d closes=%d request=%+v mutations=%d", source.acquires, source.closes, publisher.request, store.mutations)
	}
}

func TestCoordinatorClosesPublicationSourceOnMutationAndCleanupFailures(t *testing.T) {
	t.Run("mutation", func(t *testing.T) {
		_, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
		source := &trackingPublicationSource{path: "/private/retained-checkout"}
		coordinator.source = source
		publisher.pushErr = taskpublication.ErrPushFailed
		if err := coordinator.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if source.closes != 1 {
			t.Fatalf("closes=%d", source.closes)
		}
	})
	t.Run("close", func(t *testing.T) {
		_, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
		closeErr := errors.New("retained checkout cleanup failed")
		source := &trackingPublicationSource{path: "/private/retained-checkout", closeErr: closeErr}
		coordinator.source = source
		publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
		if err := coordinator.RunOnce(context.Background()); !errors.Is(err, closeErr) {
			t.Fatalf("error=%v", err)
		}
		if source.closes != 1 {
			t.Fatalf("closes=%d", source.closes)
		}
	})
}

func TestAcquireFailureIsSanitizedAndBlocksAllWork(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	cause := errors.New("pause failed secret-output-marker")
	store.fencer.err = cause
	err := coordinator.RunOnce(context.Background())
	if !errors.Is(err, ErrFenceFailed) || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "secret-output-marker") {
		t.Fatalf("fence error leaked dependency output: %v", err)
	}
	if publisher.pushes != 0 || publisher.reconcileBranches != 0 || publisher.reconcilePulls != 0 || publisher.creates != 0 || store.mutations != 0 {
		t.Fatal("work crossed a failed fence")
	}
	acquires, releases := store.fencer.counts()
	if acquires != 1 || releases != 0 {
		t.Fatalf("acquires=%d releases=%d", acquires, releases)
	}
}

func TestPublisherAndTransitionsRunWhileFenceHeld(t *testing.T) {
	tests := []struct {
		name      string
		phase     taskstore.PublicationPhase
		configure func(*fakePublisher)
	}{
		{"push", taskstore.PublicationPhaseNone, func(publisher *fakePublisher) {
			publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
		}},
		{"push_reconcile_advance", taskstore.PublicationPhasePushStarted, func(publisher *fakePublisher) {
			publisher.branch = exactBranch()
		}},
		{"push_reconcile_recover", taskstore.PublicationPhasePushStarted, func(publisher *fakePublisher) {
			publisher.branch = taskpublication.BranchObservation{Exists: true, SHA: testOtherSHA}
		}},
		{"pull_reconcile_complete", taskstore.PublicationPhasePushObserved, func(publisher *fakePublisher) {
			publisher.pull = taskpublication.PullRequestProof{Found: true, Observation: exactPull()}
		}},
		{"pull_create", taskstore.PublicationPhasePushObserved, func(publisher *fakePublisher) {
			publisher.pull = taskpublication.PullRequestProof{Found: false}
			publisher.create = taskpublication.PullRequestProof{Found: true, Observation: exactPull(), CreateAttempted: true, CreateConfirmed: true}
		}},
		{"pull_reconcile_started", taskstore.PublicationPhasePRCreateStarted, func(publisher *fakePublisher) {
			publisher.pull = taskpublication.PullRequestProof{Found: true, Observation: exactPull()}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, publisher, coordinator := testCoordinator(t, test.phase)
			test.configure(publisher)
			if err := coordinator.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if publisher.callWithoutFence != 0 || store.mutationsUnfenced != 0 || store.mutations == 0 {
				t.Fatalf("publisher unfenced=%d store unfenced=%d mutations=%d", publisher.callWithoutFence, store.mutationsUnfenced, store.mutations)
			}
			acquires, releases := store.fencer.counts()
			if acquires != 1 || releases != 1 || store.fencer.isHeld() {
				t.Fatalf("acquires=%d releases=%d held=%v", acquires, releases, store.fencer.isHeld())
			}
		})
	}
}

func TestSelectionChangedUnderFenceBlocksPublisherAndStoreMutation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*taskstore.PublicationWork)
	}{
		{"publication_id", func(work *taskstore.PublicationWork) { work.Publication.ID = "pub_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"publication_revision", func(work *taskstore.PublicationWork) { work.Publication.Revision++ }},
		{"task_id", func(work *taskstore.PublicationWork) { work.Task.ID = "tsk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"task_revision", func(work *taskstore.PublicationWork) { work.Task.Revision++ }},
		{"attempt_id", func(work *taskstore.PublicationWork) { work.Attempt.ID = "att_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"attempt_revision", func(work *taskstore.PublicationWork) { work.Attempt.Revision++ }},
		{"result_id", func(work *taskstore.PublicationWork) { work.Result.ID = "res_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"result_revision", func(work *taskstore.PublicationWork) { work.Result.Revision++ }},
		{"verification_id", func(work *taskstore.PublicationWork) { work.Verification.ID = "ver_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }},
		{"verification_revision", func(work *taskstore.PublicationWork) { work.Verification.Revision++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
			store.find = func(call int, work taskstore.PublicationWork) (taskstore.PublicationWork, error) {
				if call == 2 {
					test.mutate(&work)
				}
				return work, nil
			}
			err := coordinator.RunOnce(context.Background())
			if !errors.Is(err, ErrSelectionChanged) {
				t.Fatalf("error = %v", err)
			}
			if publisher.pushes != 0 || publisher.reconcileBranches != 0 || publisher.reconcilePulls != 0 || publisher.creates != 0 || store.mutations != 0 {
				t.Fatal("selection change permitted work")
			}
			acquires, releases := store.fencer.counts()
			if acquires != 1 || releases != 1 {
				t.Fatalf("acquires=%d releases=%d", acquires, releases)
			}
		})
	}
}

func TestSelectionDisappearingUnderFenceIsSanitized(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	store.find = func(call int, work taskstore.PublicationWork) (taskstore.PublicationWork, error) {
		if call == 2 {
			return taskstore.PublicationWork{}, taskstore.ErrNotFound
		}
		return work, nil
	}
	err := coordinator.RunOnce(context.Background())
	if !errors.Is(err, ErrSelectionChanged) || strings.Contains(err.Error(), string(store.work.Publication.ID)) {
		t.Fatalf("error = %v", err)
	}
	if publisher.pushes != 0 || store.mutations != 0 {
		t.Fatal("disappearing selection permitted mutation")
	}
}

func TestFenceReleasedWhenPostAcquisitionReadFails(t *testing.T) {
	store, _, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	want := errors.New("reread failed")
	store.find = func(call int, work taskstore.PublicationWork) (taskstore.PublicationWork, error) {
		if call == 2 {
			return taskstore.PublicationWork{}, want
		}
		return work, nil
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	acquires, releases := store.fencer.counts()
	if acquires != 1 || releases != 1 || store.fencer.isHeld() {
		t.Fatalf("acquires=%d releases=%d held=%v", acquires, releases, store.fencer.isHeld())
	}
}

func TestSelectionRaceAtFenceBlocksMutation(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	store.fencer.onAcquire = func() {
		store.mu.Lock()
		store.work.Result.Revision++
		store.mu.Unlock()
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrSelectionChanged) {
		t.Fatalf("error = %v", err)
	}
	if publisher.pushes != 0 || store.mutations != 0 {
		t.Fatal("selection race permitted mutation")
	}
}

func TestNewRequiresFencer(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	if _, err := New(store, nil, publisher, coordinator.ids, coordinator.config); err == nil {
		t.Fatal("New accepted a nil fencer")
	}
}

func TestPhaseCommitsFenceExactlyOneMutation(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.pushes != 1 || publisher.creates != 0 || store.work.Publication.EffectPhase != taskstore.PublicationPhasePushObserved {
		t.Fatalf("after push: pushes=%d creates=%d phase=%s", publisher.pushes, publisher.creates, store.work.Publication.EffectPhase)
	}
	publisher.pull = taskpublication.PullRequestProof{Found: false}
	publisher.create = taskpublication.PullRequestProof{Found: true, Observation: exactPull(), CreateAttempted: true, CreateConfirmed: true}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.pushes != 1 || publisher.creates != 1 || !store.terminal || store.work.Publication.State != taskstore.PublicationPublished {
		t.Fatalf("publication did not complete exactly once: %+v", store.work.Publication)
	}
	acquires, releases := store.fencer.counts()
	if acquires != 2 || releases != 2 || store.fencer.isHeld() {
		t.Fatalf("acquires=%d releases=%d held=%v", acquires, releases, store.fencer.isHeld())
	}
}

func TestRestartAtStartedPhasesIsReadOnly(t *testing.T) {
	tests := []struct {
		name  string
		phase taskstore.PublicationPhase
		set   func(*fakePublisher)
	}{
		{"push", taskstore.PublicationPhasePushStarted, func(p *fakePublisher) { p.branch = exactBranch() }},
		{"pull_request", taskstore.PublicationPhasePRCreateStarted, func(p *fakePublisher) {
			p.pull = taskpublication.PullRequestProof{Found: true, Observation: exactPull()}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, publisher, coordinator := testCoordinator(t, test.phase)
			test.set(publisher)
			if err := coordinator.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if publisher.pushes != 0 || publisher.creates != 0 {
				t.Fatalf("restart mutated: pushes=%d creates=%d", publisher.pushes, publisher.creates)
			}
			if test.phase == taskstore.PublicationPhasePushStarted && store.work.Publication.EffectPhase != taskstore.PublicationPhasePushObserved {
				t.Fatalf("phase = %s", store.work.Publication.EffectPhase)
			}
			if test.phase == taskstore.PublicationPhasePRCreateStarted && !store.terminal {
				t.Fatal("exact pull request was not completed")
			}
		})
	}
}

func TestLostMutationResponsesUseAuthoritativeObservation(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true, ExitCode: 1}}
	publisher.pushErr = taskpublication.ErrPushFailed
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.work.Publication.EffectPhase != taskstore.PublicationPhasePushObserved || store.work.Publication.State != taskstore.PublicationRunning {
		t.Fatalf("lost push was not reconciled: %+v", store.work.Publication)
	}
	publisher.pull = taskpublication.PullRequestProof{Found: false}
	publisher.create = taskpublication.PullRequestProof{Found: true, Observation: exactPull(), CreateAttempted: true, CreateConfirmed: false}
	publisher.createErr = errors.New("lost response")
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.terminal || publisher.creates != 1 {
		t.Fatalf("lost create was not reconciled: terminal=%v creates=%d", store.terminal, publisher.creates)
	}
}

func TestConflictAndAbsentStartedEffectsAreTerminalWithoutRetry(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhasePushStarted)
		publisher.branch = taskpublication.BranchObservation{Exists: true, SHA: testOtherSHA}
		if err := coordinator.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if store.work.Publication.State != taskstore.PublicationConflict || publisher.pushes != 0 {
			t.Fatalf("state=%s pushes=%d", store.work.Publication.State, publisher.pushes)
		}
	})
	t.Run("absent_create", func(t *testing.T) {
		store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhasePRCreateStarted)
		publisher.pull = taskpublication.PullRequestProof{Found: false}
		if err := coordinator.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if store.work.Publication.State != taskstore.PublicationRecoveryRequired || publisher.creates != 0 {
			t.Fatalf("state=%s creates=%d", store.work.Publication.State, publisher.creates)
		}
	})
}

func TestStaleFencePreventsMutation(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	store.stale = true
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, taskstore.ErrStaleRevision) {
		t.Fatalf("error = %v", err)
	}
	if publisher.pushes != 0 || publisher.creates != 0 {
		t.Fatal("mutation crossed a stale revision fence")
	}
	acquires, releases := store.fencer.counts()
	if acquires != 1 || releases != 1 || store.fencer.isHeld() {
		t.Fatalf("acquires=%d releases=%d held=%v", acquires, releases, store.fencer.isHeld())
	}
}

func TestUncertainReconciliationRepeatsReadsOnlyAndEvidenceIsSanitized(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhasePushStarted)
	publisher.branchErr = errors.New("transport secret-output-marker")
	if err := coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.work.Publication.State != taskstore.PublicationUncertain {
		t.Fatalf("state = %s", store.work.Publication.State)
	}
	if err := coordinator.RunOnce(context.Background()); !errors.Is(err, ErrReconciliationPending) {
		t.Fatalf("second error = %v", err)
	}
	if publisher.pushes != 0 || publisher.reconcileBranches != 2 {
		t.Fatalf("pushes=%d reads=%d", publisher.pushes, publisher.reconcileBranches)
	}
	for _, payload := range store.payloads {
		if bytes.Contains(payload, []byte("secret-output-marker")) {
			t.Fatalf("error/output leaked to evidence: %s", payload)
		}
	}
}

func TestRunImmediatelyTracksPendingReconciliationAsDegraded(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhasePushStarted)
	publisher.branchErr = errors.New("GitHub unavailable")
	status := observability.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	coordinator.config.OnError = func(err error) {
		status.Degraded(observability.ComponentTaskPublication, err)
		cancel()
	}
	coordinator.config.OnSuccess = func() { status.Healthy(observability.ComponentTaskPublication) }

	err := coordinator.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v", err)
	}
	snapshot := status.Snapshot()
	publicationState := observability.StateDisabled
	for _, component := range snapshot.Components {
		if component.Component == observability.ComponentTaskPublication {
			publicationState = component.State
			break
		}
	}
	if !snapshot.Ready || publicationState != observability.StateDegraded {
		t.Fatalf("transient publication snapshot = %+v", snapshot)
	}
	if publisher.reconcileBranches != 2 || store.work.Publication.State != taskstore.PublicationUncertain {
		t.Fatalf("reads=%d publication=%+v", publisher.reconcileBranches, store.work.Publication)
	}
}

func TestConcurrentPassesSerializeMutation(t *testing.T) {
	store, publisher, coordinator := testCoordinator(t, taskstore.PublicationPhaseNone)
	publisher.push = taskpublication.BranchProof{Observation: exactBranch(), Push: taskpublication.GitEvidence{Attempted: true}}
	publisher.pull = taskpublication.PullRequestProof{Found: false}
	publisher.create = taskpublication.PullRequestProof{Found: true, Observation: exactPull(), CreateAttempted: true, CreateConfirmed: true}
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			_ = coordinator.RunOnce(context.Background())
		}()
	}
	wait.Wait()
	if publisher.pushes != 1 || publisher.creates != 1 || !store.terminal {
		t.Fatalf("pushes=%d creates=%d terminal=%v", publisher.pushes, publisher.creates, store.terminal)
	}
}

func testCoordinator(t *testing.T, phase taskstore.PublicationPhase) (*memoryStore, *fakePublisher, *Coordinator) {
	t.Helper()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ids, err := task.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{0x42}, 4096)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, _ := ids.WorkspaceID()
	taskID, _ := ids.TaskID()
	attemptID, _ := ids.AttemptID()
	resultID, _ := ids.ResultID()
	verificationID, _ := ids.VerificationID()
	// Generator.PublicationID was removed as dead production API; fabricate a
	// canonical ID directly (version/variant nibbles must satisfy ParsePublicationID).
	publicationID := task.PublicationID("pub_0198d34d-6a50-75fb-b1f2-000000000001")
	operationID, _ := ids.PublicationOperationID()
	state := taskstore.PublicationRunning
	if phase == taskstore.PublicationPhaseNone {
		state = taskstore.PublicationPrepared
	}
	tuple := task.PublicationTuple{OperationID: operationID, InstallationID: 7, RepositoryID: 123, RepositoryFullName: "owner/repo",
		WorkspaceName: "workspace", BaseRef: "main", BaseSHA: testBaseSHA, ResultCommit: testResultSHA,
		Branch: task.PublicationBranch("workspace", operationID)}
	work := taskstore.PublicationWork{
		Publication: taskstore.Publication{ID: publicationID, TaskID: taskID, AttemptID: attemptID, ResultID: resultID,
			VerificationID: verificationID, WorkspaceID: workspaceID, State: state, EffectPhase: phase, Tuple: tuple, Revision: 1},
		Task:    taskstore.Task{ID: taskID, WorkspaceID: workspaceID, Title: "Verified change", RepositoryID: 123, BaseRef: "main", BaseSHA: testBaseSHA, Revision: 4},
		Attempt: taskstore.Attempt{ID: attemptID, TaskID: taskID, WorkspaceID: workspaceID, Revision: 5},
		Result: taskstore.Result{ID: resultID, TaskID: taskID, AttemptID: attemptID, WorkspaceID: workspaceID, State: task.ResultSealed,
			Outcome: task.ResultChanged, RepositoryID: 123, BaseSHA: testBaseSHA, ResultCommit: testResultSHA, ManifestEntries: 1, WorktreeClean: true},
		Verification: taskstore.Verification{ID: verificationID, ResultID: resultID, TaskID: taskID, AttemptID: attemptID,
			WorkspaceID: workspaceID, State: taskstore.VerificationSucceeded, VerifiedCommit: testResultSHA},
	}
	fencer := &fakeFencer{}
	store := &memoryStore{work: work, fencer: fencer}
	publisher := &fakePublisher{fencer: fencer}
	actor := task.ActorSnapshot{Type: task.ActorSystem, ID: "publication-worker", CredentialID: "host", Authentication: "internal", RequestID: "test"}
	recovery := actor
	recovery.Type = task.ActorRecovery
	coordinator, err := New(store, fencer, publisher, ids, Config{WorkspaceID: workspaceID, OperationTimeout: time.Second,
		PollInterval: time.Millisecond, Actor: actor, RecoveryActor: recovery, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return store, publisher, coordinator
}

func exactBranch() taskpublication.BranchObservation {
	return taskpublication.BranchObservation{Exists: true, SHA: testResultSHA}
}

func exactPull() task.PublicationObservation {
	return task.PublicationObservation{RemoteSHA: testResultSHA, PullRequest: task.PullRequestObservation{Number: 1}}
}
