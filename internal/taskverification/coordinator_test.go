package taskverification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/verification"
)

func TestCoordinatorResultMappingsAndOutputAccounting(t *testing.T) {
	secret := []byte("output-secret")
	hash := sha256.Sum256(secret)
	tests := []struct {
		name    string
		result  verification.Result
		runErr  error
		state   taskstore.VerificationState
		outcome string
	}{
		{"success", verification.Result{Executed: true, ExitCode: 0, Success: true}, nil, taskstore.VerificationSucceeded, "passed"},
		{"exit", verification.Result{Executed: true, ExitCode: 7, Failure: verification.FailureCommand}, nil, taskstore.VerificationFailed, "exit_nonzero"},
		{"timeout", verification.Result{Executed: true, ExitCode: -1, TimedOut: true, Failure: verification.FailureTimeout}, nil, taskstore.VerificationFailed, "timeout"},
		{"canceled", verification.Result{Executed: true, ExitCode: -1, Cancelled: true, Failure: verification.FailureCancelled}, nil, taskstore.VerificationFailed, "canceled"},
		{"integrity", verification.Result{Executed: true, ExitCode: 0, IntegrityError: true, Failure: verification.FailureIntegrity}, verification.ErrIntegrity, taskstore.VerificationRecoveryRequired, "integrity_failure"},
		{"preflight", verification.Result{ExitCode: -1, Failure: verification.FailurePreflight}, verification.ErrPreflight, taskstore.VerificationRecoveryRequired, "runner_failure"},
		{"start", verification.Result{ExitCode: -1, Failure: verification.FailureStart}, verification.ErrExecution, taskstore.VerificationRecoveryRequired, "start_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCoordinatorFixture(t, test.result, test.runErr)
			fixture.runner.result.Stdout = verification.OutputEvidence{ByteCount: int64(len(secret)), SHA256: hash, Prefix: append([]byte(nil), secret...)}
			fixture.runner.result.Stderr = verification.OutputEvidence{SHA256: sha256.Sum256(nil)}
			if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fixture.runner.calls != 1 || fixture.store.verification.State != test.state || fixture.store.verification.Outcome != test.outcome {
				t.Fatalf("calls=%d verification=%+v", fixture.runner.calls, fixture.store.verification)
			}
			if !fixture.runner.sawFence || fixture.fencer.calls != 1 || fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
				t.Fatalf("runner fence=%v calls=%d releases=%d held=%v", fixture.runner.sawFence, fixture.fencer.calls, fixture.fencer.releases, fixture.fencer.isHeld())
			}
			if test.state == taskstore.VerificationRecoveryRequired && !fixture.store.recoverSawFence {
				t.Fatal("recovery store write did not retain fence")
			}
			if test.state != taskstore.VerificationRecoveryRequired && !fixture.store.completeSawFence {
				t.Fatal("completion store write did not retain fence")
			}
			if fixture.store.verification.Stdout == nil || fixture.store.verification.Stdout.ByteCount != int64(len(secret)) ||
				fixture.store.verification.Stdout.RetainedBytes != int64(len(secret)) || fixture.store.verification.Stdout.SHA256 != hash {
				t.Fatalf("stdout accounting = %+v", fixture.store.verification.Stdout)
			}
			if strings.Contains(string(fixture.store.lastEvidence), string(secret)) {
				t.Fatalf("output entered evidence: %s", fixture.store.lastEvidence)
			}
		})
	}
}

func TestCoordinatorRestartNeverReruns(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	fixture.store.installRunning(fixture)
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.calls != 0 || fixture.fencer.calls != 0 || fixture.fencer.releases != 0 ||
		fixture.store.verification.State != taskstore.VerificationRecoveryRequired ||
		fixture.store.verification.Outcome != "runner_failure" || fixture.store.verification.Stdout == nil ||
		fixture.store.verification.Stdout.SHA256 != sha256.Sum256(nil) {
		t.Fatalf("runner calls=%d verification=%+v", fixture.runner.calls, fixture.store.verification)
	}
}

func TestCoordinatorAdvanceAmbiguityRecoversWithoutRun(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	fixture.store.advanceAfterCommitError = errors.New("commit response lost")
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.calls != 0 || fixture.store.verification.State != taskstore.VerificationRecoveryRequired ||
		!fixture.store.recoverSawFence || fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("runner calls=%d verification=%+v", fixture.runner.calls, fixture.store.verification)
	}
}

func TestCoordinatorOwnershipRaceDoesNotRun(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	fixture.store.advanceError = taskstore.ErrStaleRevision
	err := fixture.coordinator.RunOnce(context.Background())
	if !errors.Is(err, taskstore.ErrStaleRevision) || fixture.runner.calls != 0 || fixture.store.verification.State != taskstore.VerificationPrepared ||
		fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("error=%v calls=%d verification=%+v", err, fixture.runner.calls, fixture.store.verification)
	}
}

func TestCoordinatorCompletionAmbiguityPreservesAccounting(t *testing.T) {
	data := []byte("available-output")
	hash := sha256.Sum256(data)
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true,
		Stdout: verification.OutputEvidence{ByteCount: int64(len(data)), SHA256: hash, Prefix: data},
		Stderr: verification.OutputEvidence{SHA256: sha256.Sum256(nil)}}, nil)
	fixture.store.completeError = errors.New("sqlite completion ambiguous")
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.calls != 1 || fixture.store.verification.State != taskstore.VerificationRecoveryRequired ||
		fixture.store.verification.Stdout == nil || fixture.store.verification.Stdout.SHA256 != hash ||
		!fixture.store.completeSawFence || !fixture.store.recoverSawFence || fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("calls=%d verification=%+v", fixture.runner.calls, fixture.store.verification)
	}
}

func TestCoordinatorLostCompletionResponseAcceptsDurableTerminalState(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true,
		Stdout: verification.OutputEvidence{SHA256: sha256.Sum256(nil)},
		Stderr: verification.OutputEvidence{SHA256: sha256.Sum256(nil)}}, nil)
	fixture.store.completeAfterCommitError = errors.New("completion response lost")
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.calls != 1 || fixture.store.completeCalls != 1 || fixture.store.recoverCalls != 0 ||
		fixture.store.verification.State != taskstore.VerificationSucceeded || fixture.fencer.releases != 1 {
		t.Fatalf("runs=%d completes=%d recovers=%d verification=%+v releases=%d", fixture.runner.calls,
			fixture.store.completeCalls, fixture.store.recoverCalls, fixture.store.verification, fixture.fencer.releases)
	}
}

func TestCoordinatorRepeatedAndConcurrentRunOnceRunsCommandOnce(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true,
		Stdout: verification.OutputEvidence{SHA256: sha256.Sum256(nil)}, Stderr: verification.OutputEvidence{SHA256: sha256.Sum256(nil)}}, nil)
	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- fixture.coordinator.RunOnce(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrNoWork) {
			t.Errorf("RunOnce: %v", err)
		}
	}
	if fixture.runner.calls != 1 || fixture.store.prepareCalls != 1 || fixture.fencer.calls != 1 ||
		fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("runner calls=%d preparations=%d fences=%d releases=%d held=%v", fixture.runner.calls,
			fixture.store.prepareCalls, fixture.fencer.calls, fixture.fencer.releases, fixture.fencer.isHeld())
	}
	if err := fixture.coordinator.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) || fixture.runner.calls != 1 {
		t.Fatalf("repeat error=%v calls=%d", err, fixture.runner.calls)
	}
}

func TestCoordinatorPreparedSnapshotMismatchRecovers(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	fixture.store.installPrepared(fixture)
	fixture.store.verification.PolicySHA256 = sha256.Sum256([]byte("other-policy"))
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.calls != 0 || fixture.store.verification.State != taskstore.VerificationRecoveryRequired || fixture.store.verification.EffectAttempt != 0 ||
		!fixture.store.recoverSawFence || fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("calls=%d verification=%+v", fixture.runner.calls, fixture.store.verification)
	}
}

func TestCoordinatorReleasesFenceWhenRecoveryFails(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{}, nil)
	fixture.store.installPrepared(fixture)
	fixture.store.verification.PolicySHA256 = sha256.Sum256([]byte("other-policy"))
	recoverErr := errors.New("recovery write failed")
	fixture.store.recoverError = recoverErr

	err := fixture.coordinator.RunOnce(context.Background())
	if !errors.Is(err, recoverErr) || fixture.runner.calls != 0 || !fixture.store.recoverSawFence ||
		fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("error=%v runs=%d recovery fence=%v releases=%d held=%v", err, fixture.runner.calls,
			fixture.store.recoverSawFence, fixture.fencer.releases, fixture.fencer.isHeld())
	}
}

func TestCoordinatorFenceAcquisitionFailureLeavesPreparedRetryable(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	acquireErr := errors.New("pause failed")
	fixture.fencer.acquireErr = acquireErr

	err := fixture.coordinator.RunOnce(context.Background())
	if !errors.Is(err, acquireErr) || fixture.store.verification.State != taskstore.VerificationPrepared ||
		fixture.store.advanceCalls != 0 || fixture.runner.calls != 0 || fixture.fencer.releases != 0 || fixture.fencer.isHeld() {
		t.Fatalf("error=%v verification=%+v advances=%d runs=%d releases=%d held=%v", err,
			fixture.store.verification, fixture.store.advanceCalls, fixture.runner.calls, fixture.fencer.releases, fixture.fencer.isHeld())
	}

	fixture.fencer.acquireErr = nil
	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.store.prepareCalls != 1 || fixture.store.advanceCalls != 1 || fixture.runner.calls != 1 ||
		fixture.store.verification.State != taskstore.VerificationSucceeded || fixture.fencer.releases != 1 || fixture.fencer.isHeld() {
		t.Fatalf("preparations=%d advances=%d runs=%d verification=%+v releases=%d held=%v",
			fixture.store.prepareCalls, fixture.store.advanceCalls, fixture.runner.calls, fixture.store.verification,
			fixture.fencer.releases, fixture.fencer.isHeld())
	}
}

func TestCoordinatorRereadsOwnershipAfterFenceAcquisition(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{Executed: true, ExitCode: 0, Success: true}, nil)
	fixture.fencer.onAcquire = func() {
		fixture.store.mu.Lock()
		fixture.store.source.Task.Revision++
		fixture.store.source.Attempt.Revision++
		fixture.store.mu.Unlock()
	}

	if err := fixture.coordinator.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.store.lastAdvance.ExpectedTaskRevision != 6 || fixture.store.lastAdvance.ExpectedAttemptRevision != 8 ||
		!fixture.store.taskReadSawFence || !fixture.store.attemptReadSawFence || !fixture.store.advanceSawFence || !fixture.runner.sawFence {
		t.Fatalf("advance=%+v reads fenced=%v/%v advance fence=%v runner fence=%v", fixture.store.lastAdvance,
			fixture.store.taskReadSawFence, fixture.store.attemptReadSawFence, fixture.store.advanceSawFence, fixture.runner.sawFence)
	}
}

func TestCoordinatorRequiresFencer(t *testing.T) {
	fixture := newCoordinatorFixture(t, verification.Result{}, nil)
	_, err := New(fixture.store, nil, fixture.runner, fixture.policy, fixture.coordinator.ids, fixture.coordinator.config)
	if err == nil {
		t.Fatal("New accepted a nil fencer")
	}
}

type coordinatorFixture struct {
	coordinator *Coordinator
	store       *fakeStore
	fencer      *fakeFencer
	runner      *fakeRunner
	policy      verification.Policy
	now         time.Time
}

func newCoordinatorFixture(t *testing.T, result verification.Result, runErr error) *coordinatorFixture {
	t.Helper()
	now := time.UnixMilli(1_800_000_000_000)
	ids, err := task.NewGenerator(bytes.NewReader(bytes.Repeat([]byte{0x45}, 4096)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, _ := ids.WorkspaceID()
	taskID, _ := ids.TaskID()
	attemptID, _ := ids.AttemptID()
	resultID, _ := ids.ResultID()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	immutableDirectory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	immutableExecutable := filepath.Join(immutableDirectory, "coordinator.test")
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(immutableExecutable, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0500)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	inputCloseErr := input.Close()
	outputCloseErr := output.Close()
	if copyErr != nil || inputCloseErr != nil || outputCloseErr != nil {
		t.Fatalf("copy immutable test executable: %v %v %v", copyErr, inputCloseErr, outputCloseErr)
	}
	executable = immutableExecutable
	policy, err := verification.NewPolicy(verification.PolicyConfig{CheckName: "go-test", Argv: []string{filepath.Clean(executable)},
		Timeout: time.Second, OutputBytes: 1024, Environment: map[string]string{"POLICY": "test"}})
	if err != nil {
		t.Fatal(err)
	}
	fencer := &fakeFencer{}
	runner := &fakeRunner{fencer: fencer, result: result, runErr: runErr, snapshot: verification.RunnerSnapshot{Name: "fern-verifier", Version: "v1",
		ImageDigest: "sha256:image", EnvironmentSHA256: sha256.Sum256([]byte("exact-environment"))}}
	store := &fakeStore{source: taskstore.VerificationSource{
		Result: taskstore.Result{ID: resultID, TaskID: taskID, AttemptID: attemptID, WorkspaceID: workspaceID,
			State: task.ResultSealed, RepositoryID: 42, BaseSHA: task.GitOID(strings.Repeat("1", 40)),
			ResultCommit: task.GitOID(strings.Repeat("2", 40)), UpdatedAt: now},
		Task: taskstore.Task{ID: taskID, WorkspaceID: workspaceID, State: task.TaskCompleted, RepositoryID: 42,
			BaseSHA: task.GitOID(strings.Repeat("1", 40)), CurrentAttemptID: attemptID,
			SealedResultID: resultID, Revision: 5, UpdatedAt: now},
		Attempt: taskstore.Attempt{ID: attemptID, TaskID: taskID, WorkspaceID: workspaceID, State: task.AttemptSucceeded,
			BaseSHA: task.GitOID(strings.Repeat("1", 40)), SealedResultID: resultID, Revision: 7, UpdatedAt: now},
	}, fencer: fencer}
	actor := task.ActorSnapshot{Type: task.ActorSystem, ID: "verification", DisplayName: "Verification coordinator",
		CredentialID: "service-v1", Authentication: "internal", RequestID: "worker-1"}
	recovery := actor
	recovery.Type, recovery.ID = task.ActorRecovery, "verification-recovery"
	coordinator, err := New(store, fencer, runner, policy, ids, Config{WorkspaceID: workspaceID, RepositoryPath: filepath.Clean(t.TempDir()),
		PollInterval: time.Millisecond, Deadline: time.Minute, Actor: actor, RecoveryActor: recovery, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return &coordinatorFixture{coordinator: coordinator, store: store, fencer: fencer, runner: runner, policy: policy, now: now}
}

type fakeFencer struct {
	mu         sync.Mutex
	calls      int
	releases   int
	held       bool
	acquireErr error
	onAcquire  func()
}

func (f *fakeFencer) AcquirePaused(context.Context) (func(), error) {
	f.mu.Lock()
	f.calls++
	if f.acquireErr != nil {
		err := f.acquireErr
		f.mu.Unlock()
		return nil, err
	}
	f.held = true
	onAcquire := f.onAcquire
	f.mu.Unlock()
	if onAcquire != nil {
		onAcquire()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.held = false
			f.releases++
			f.mu.Unlock()
		})
	}, nil
}

func (f *fakeFencer) isHeld() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

type fakeRunner struct {
	mu       sync.Mutex
	fencer   *fakeFencer
	snapshot verification.RunnerSnapshot
	result   verification.Result
	runErr   error
	calls    int
	sawFence bool
}

func (r *fakeRunner) Snapshot(verification.Policy) (verification.RunnerSnapshot, error) {
	return r.snapshot, nil
}
func (r *fakeRunner) Run(_ context.Context, _ verification.Policy, _ verification.Request) (verification.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.sawFence = r.fencer.isHeld()
	return r.result, r.runErr
}

type fakeStore struct {
	mu                       sync.Mutex
	source                   taskstore.VerificationSource
	verification             taskstore.Verification
	prepareCalls             int
	advanceCalls             int
	completeCalls            int
	recoverCalls             int
	advanceError             error
	advanceAfterCommitError  error
	completeError            error
	completeAfterCommitError error
	recoverError             error
	fencer                   *fakeFencer
	advanceSawFence          bool
	completeSawFence         bool
	recoverSawFence          bool
	taskReadSawFence         bool
	attemptReadSawFence      bool
	lastAdvance              taskstore.AdvanceVerificationParams
	lastEvidence             json.RawMessage
}

func (s *fakeStore) FindResultAwaitingVerification(context.Context, task.WorkspaceID) (taskstore.VerificationSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verification.ID != "" {
		return taskstore.VerificationSource{}, taskstore.ErrNotFound
	}
	return s.source, nil
}
func (s *fakeStore) FindPreparedVerification(context.Context, task.WorkspaceID) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verification.State != taskstore.VerificationPrepared {
		return taskstore.VerificationRecord{}, taskstore.ErrNotFound
	}
	return taskstore.VerificationRecord{Verification: s.verification}, nil
}
func (s *fakeStore) FindRunningVerification(context.Context, task.WorkspaceID) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verification.State != taskstore.VerificationRunning {
		return taskstore.VerificationRecord{}, taskstore.ErrNotFound
	}
	return taskstore.VerificationRecord{Verification: s.verification}, nil
}
func (s *fakeStore) InspectVerification(_ context.Context, id task.VerificationID) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verification.ID != id {
		return taskstore.VerificationRecord{}, taskstore.ErrNotFound
	}
	return taskstore.VerificationRecord{Verification: s.verification}, nil
}
func (s *fakeStore) GetResult(_ context.Context, id task.ResultID) (taskstore.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.source.Result.ID != id {
		return taskstore.Result{}, taskstore.ErrNotFound
	}
	return s.source.Result, nil
}
func (s *fakeStore) GetTask(_ context.Context, id task.TaskID) (taskstore.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taskReadSawFence = s.fencer.isHeld()
	if s.source.Task.ID != id {
		return taskstore.Task{}, taskstore.ErrNotFound
	}
	return s.source.Task, nil
}
func (s *fakeStore) GetAttempt(_ context.Context, id task.AttemptID) (taskstore.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attemptReadSawFence = s.fencer.isHeld()
	if s.source.Attempt.ID != id {
		return taskstore.Attempt{}, taskstore.ErrNotFound
	}
	return s.source.Attempt, nil
}
func (s *fakeStore) PrepareVerification(_ context.Context, p taskstore.PrepareVerificationParams) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareCalls++
	s.verification = taskstore.Verification{ID: p.VerificationID, ResultID: p.ResultID, TaskID: s.source.Task.ID,
		AttemptID: s.source.Attempt.ID, WorkspaceID: s.source.Result.WorkspaceID, State: taskstore.VerificationPrepared,
		PolicyName: p.PolicyName, PolicySHA256: p.PolicySHA256, VerifiedCommit: p.VerifiedCommit,
		WorkingDirectory: p.WorkingDirectory, Timeout: p.Timeout, OutputLimitBytes: p.OutputLimitBytes,
		RunnerName: p.RunnerName, RunnerVersion: p.RunnerVersion, ImageDigest: p.ImageDigest,
		EnvironmentSHA256: p.EnvironmentSHA256, Revision: 1, CreatedAt: p.PreparedAt, UpdatedAt: p.PreparedAt}
	s.lastEvidence = append(s.lastEvidence[:0], p.EvidencePayload...)
	return taskstore.VerificationRecord{Verification: s.verification}, nil
}
func (s *fakeStore) AdvanceVerification(_ context.Context, p taskstore.AdvanceVerificationParams) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advanceCalls++
	s.advanceSawFence = s.fencer.isHeld()
	s.lastAdvance = p
	if s.advanceError != nil {
		return taskstore.VerificationRecord{}, s.advanceError
	}
	if p.ExpectedTaskRevision != s.source.Task.Revision || p.ExpectedAttemptRevision != s.source.Attempt.Revision {
		return taskstore.VerificationRecord{}, taskstore.ErrStaleRevision
	}
	s.verification.State, s.verification.EffectAttempt, s.verification.Revision = taskstore.VerificationRunning, 1, s.verification.Revision+1
	s.verification.StartedAt, s.verification.UpdatedAt = &p.StartedAt, p.StartedAt
	s.lastEvidence = append(s.lastEvidence[:0], p.EvidencePayload...)
	record := taskstore.VerificationRecord{Verification: s.verification}
	return record, s.advanceAfterCommitError
}
func (s *fakeStore) CompleteVerification(_ context.Context, p taskstore.CompleteVerificationParams) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	s.completeSawFence = s.fencer.isHeld()
	if s.completeError != nil {
		return taskstore.VerificationRecord{}, s.completeError
	}
	s.verification.State, s.verification.Outcome, s.verification.ExitCode, s.verification.Signal = p.State, p.Outcome, p.ExitCode, p.Signal
	s.verification.Stdout, s.verification.Stderr, s.verification.Reason = &p.Stdout, &p.Stderr, p.Reason
	s.verification.EndedAt, s.verification.UpdatedAt, s.verification.Revision = &p.EndedAt, p.EndedAt, s.verification.Revision+1
	s.lastEvidence = append(s.lastEvidence[:0], p.EvidencePayload...)
	return taskstore.VerificationRecord{Verification: s.verification}, s.completeAfterCommitError
}
func (s *fakeStore) RecoverVerification(_ context.Context, p taskstore.RecoverVerificationParams) (taskstore.VerificationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverCalls++
	s.recoverSawFence = s.fencer.isHeld()
	if s.recoverError != nil {
		return taskstore.VerificationRecord{}, s.recoverError
	}
	s.verification.State, s.verification.Outcome, s.verification.Reason = taskstore.VerificationRecoveryRequired, p.Outcome, p.Reason
	s.verification.Stdout, s.verification.Stderr = p.Stdout, p.Stderr
	s.verification.EndedAt, s.verification.UpdatedAt, s.verification.Revision = &p.RecoveredAt, p.RecoveredAt, s.verification.Revision+1
	if s.verification.EffectAttempt == 0 {
		s.verification.EndedAt = nil
	}
	s.lastEvidence = append(s.lastEvidence[:0], p.EvidencePayload...)
	return taskstore.VerificationRecord{Verification: s.verification}, nil
}

func (s *fakeStore) installPrepared(f *coordinatorFixture) {
	p := f.policy.Snapshot()
	s.verification = taskstore.Verification{ID: mustVerificationID(f), ResultID: s.source.Result.ID, TaskID: s.source.Task.ID,
		AttemptID: s.source.Attempt.ID, WorkspaceID: s.source.Result.WorkspaceID, State: taskstore.VerificationPrepared,
		PolicyName: p.CheckName, PolicySHA256: p.SHA256, VerifiedCommit: s.source.Result.ResultCommit,
		WorkingDirectory: p.WorkingDirectory, Timeout: p.Timeout, OutputLimitBytes: int64(p.OutputBytes),
		RunnerName: f.runner.snapshot.Name, RunnerVersion: f.runner.snapshot.Version, ImageDigest: f.runner.snapshot.ImageDigest,
		EnvironmentSHA256: f.runner.snapshot.EnvironmentSHA256, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now}
	s.verification.RunnerVersion = durableRunnerVersion(f.runner.snapshot)
}
func (s *fakeStore) installRunning(f *coordinatorFixture) {
	s.installPrepared(f)
	s.verification.State, s.verification.EffectAttempt, s.verification.Revision = taskstore.VerificationRunning, 1, 2
	s.verification.StartedAt = &f.now
}
func mustVerificationID(f *coordinatorFixture) task.VerificationID {
	id, err := f.coordinator.ids.VerificationID()
	if err != nil {
		panic(err)
	}
	return id
}
