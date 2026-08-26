package taskverification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/verification"
)

var ErrNoWork = errors.New("no task verification work is available")

type Store interface {
	FindResultAwaitingVerification(context.Context, task.WorkspaceID) (taskstore.VerificationSource, error)
	FindPreparedVerification(context.Context, task.WorkspaceID) (taskstore.VerificationRecord, error)
	FindRunningVerification(context.Context, task.WorkspaceID) (taskstore.VerificationRecord, error)
	InspectVerification(context.Context, task.VerificationID) (taskstore.VerificationRecord, error)
	GetResult(context.Context, task.ResultID) (taskstore.Result, error)
	GetTask(context.Context, task.TaskID) (taskstore.Task, error)
	GetAttempt(context.Context, task.AttemptID) (taskstore.Attempt, error)
	PrepareVerification(context.Context, taskstore.PrepareVerificationParams) (taskstore.VerificationRecord, error)
	AdvanceVerification(context.Context, taskstore.AdvanceVerificationParams) (taskstore.VerificationRecord, error)
	CompleteVerification(context.Context, taskstore.CompleteVerificationParams) (taskstore.VerificationRecord, error)
	RecoverVerification(context.Context, taskstore.RecoverVerificationParams) (taskstore.VerificationRecord, error)
}

// Runner permits isolated testing while verification.Runner remains the
// production implementation.
type Runner interface {
	Snapshot(verification.Policy) (verification.RunnerSnapshot, error)
	Run(context.Context, verification.Policy, verification.Request) (verification.Result, error)
}

// Fencer pauses workspace compute and excludes repository writers until the
// returned release function is called.
type Fencer interface {
	AcquirePaused(context.Context) (func(), error)
}

type Config struct {
	WorkspaceID    task.WorkspaceID
	RepositoryPath string
	PollInterval   time.Duration
	Deadline       time.Duration
	Actor          task.ActorSnapshot
	RecoveryActor  task.ActorSnapshot
	Now            func() time.Time
	OnError        func(error)
	OnSuccess      func()
}

type Coordinator struct {
	store          Store
	fencer         Fencer
	runner         Runner
	policy         verification.Policy
	policySnapshot verification.PolicySnapshot
	runnerSnapshot verification.RunnerSnapshot
	ids            *task.Generator
	config         Config
	runMu          sync.Mutex
}

func New(store Store, fencer Fencer, runner Runner, policy verification.Policy, ids *task.Generator, config Config) (*Coordinator, error) {
	if store == nil || fencer == nil || runner == nil || ids == nil {
		return nil, errors.New("task verification dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid task verification workspace is required")
	}
	if !filepath.IsAbs(config.RepositoryPath) || filepath.Clean(config.RepositoryPath) != config.RepositoryPath || strings.IndexByte(config.RepositoryPath, 0) >= 0 {
		return nil, errors.New("absolute clean verification repository path is required")
	}
	if config.Deadline <= 0 || config.Deadline > 2*time.Hour {
		return nil, errors.New("task verification deadline must be positive and at most two hours")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.OnError == nil {
		config.OnError = func(error) {}
	}
	if config.OnSuccess == nil {
		config.OnSuccess = func() {}
	}
	if err := config.Actor.Validate(); err != nil || config.Actor.Type != task.ActorSystem {
		return nil, errors.New("valid system verification actor is required")
	}
	if err := config.RecoveryActor.Validate(); err != nil || config.RecoveryActor.Type != task.ActorRecovery {
		return nil, errors.New("valid recovery verification actor is required")
	}
	policySnapshot := policy.Snapshot()
	if policySnapshot.CheckName == "" || policySnapshot.Timeout <= 0 || policySnapshot.OutputBytes <= 0 {
		return nil, verification.ErrInvalidPolicy
	}
	runnerSnapshot, err := runner.Snapshot(policy)
	if err != nil {
		return nil, err
	}
	runnerSnapshot.Version = durableRunnerVersion(runnerSnapshot)
	return &Coordinator{store: store, fencer: fencer, runner: runner, policy: policy, policySnapshot: policySnapshot,
		runnerSnapshot: runnerSnapshot, ids: ids, config: config}, nil
}

func durableRunnerVersion(snapshot verification.RunnerSnapshot) string {
	digest := "sha256:" + hex.EncodeToString(snapshot.SHA256[:])
	combined := snapshot.Version + "+" + digest
	if len(combined) <= 128 {
		return combined
	}
	return digest
}

func (c *Coordinator) Run(ctx context.Context) error {
	retry := observability.NewRetry(c.config.PollInterval, 30*time.Second)
	var delay time.Duration
	for {
		if err := observability.Wait(ctx, nil, delay); err != nil {
			return err
		}
		failed := false
		if err := c.RunOnce(ctx); err != nil && !errors.Is(err, ErrNoWork) {
			if errors.Is(err, taskstore.ErrCorruptStore) {
				return err
			}
			c.config.OnError(err)
			failed = true
		}
		if failed {
			delay = retry.Next()
		} else {
			c.config.OnSuccess()
			retry.Reset()
			delay = c.config.PollInterval
		}
	}
}

func (c *Coordinator) RunOnce(ctx context.Context) error {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	operation, cancel := context.WithTimeout(ctx, c.config.Deadline)
	defer cancel()
	if running, err := c.store.FindRunningVerification(operation, c.config.WorkspaceID); err == nil {
		return c.recoverRestart(operation, running)
	} else if !errors.Is(err, taskstore.ErrNotFound) {
		return err
	}
	if prepared, err := c.store.FindPreparedVerification(operation, c.config.WorkspaceID); err == nil {
		return c.executePrepared(operation, prepared)
	} else if !errors.Is(err, taskstore.ErrNotFound) {
		return err
	}
	source, err := c.store.FindResultAwaitingVerification(operation, c.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	prepared, err := c.prepare(operation, source)
	if err != nil {
		return err
	}
	return c.executePrepared(operation, prepared)
}

func (c *Coordinator) prepare(ctx context.Context, source taskstore.VerificationSource) (taskstore.VerificationRecord, error) {
	verificationID, err := c.ids.VerificationID()
	if err != nil {
		return taskstore.VerificationRecord{}, err
	}
	eventID, err := c.ids.EventID()
	if err != nil {
		return taskstore.VerificationRecord{}, err
	}
	evidence := evidencePayload("prepared", "")
	return c.store.PrepareVerification(ctx, taskstore.PrepareVerificationParams{
		VerificationID: verificationID, ResultID: source.Result.ID, ExpectedTaskRevision: source.Task.Revision,
		ExpectedAttemptRevision: source.Attempt.Revision, EventID: eventID, PolicyName: c.policySnapshot.CheckName,
		PolicySHA256: c.policySnapshot.SHA256, VerifiedCommit: source.Result.ResultCommit,
		WorkingDirectory: c.policySnapshot.WorkingDirectory, Timeout: c.policySnapshot.Timeout,
		OutputLimitBytes: int64(c.policySnapshot.OutputBytes), RunnerName: c.runnerSnapshot.Name,
		RunnerVersion: c.runnerSnapshot.Version, ImageDigest: c.runnerSnapshot.ImageDigest,
		EnvironmentSHA256: c.runnerSnapshot.EnvironmentSHA256, PreparedAt: c.nowAfter(source.Result.UpdatedAt),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: c.config.Actor,
	})
}

func (c *Coordinator) executePrepared(ctx context.Context, prepared taskstore.VerificationRecord) error {
	release, err := c.fencer.AcquirePaused(ctx)
	if err != nil {
		return err
	}
	if release == nil {
		return errors.New("task verification fence did not provide a release function")
	}
	defer release()

	v := prepared.Verification
	taskRow, attempt, err := c.owners(ctx, v)
	if err != nil {
		return err
	}
	if !c.matchesSnapshot(v) {
		return c.recover(ctx, v, taskRow.Revision, attempt.Revision, "verification_snapshot_mismatch", "", nil)
	}
	eventID, err := c.ids.EventID()
	if err != nil {
		return err
	}
	evidence := evidencePayload("started", "")
	running, err := c.store.AdvanceVerification(ctx, taskstore.AdvanceVerificationParams{
		VerificationID: v.ID, ExpectedRevision: v.Revision, ExpectedTaskRevision: taskRow.Revision,
		ExpectedAttemptRevision: attempt.Revision, EventID: eventID, StartedAt: c.nowAfter(v.UpdatedAt),
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: c.config.Actor,
	})
	if err != nil {
		return c.resolveAdvanceError(ctx, v.ID, err)
	}
	resultRow, err := c.store.GetResult(ctx, running.Verification.ResultID)
	if err != nil {
		return c.recover(ctx, running.Verification, taskRow.Revision, attempt.Revision, "verification_result_read_failed", "runner_failure", nil)
	}
	if resultRow.ID != running.Verification.ResultID || resultRow.TaskID != running.Verification.TaskID ||
		resultRow.AttemptID != running.Verification.AttemptID || resultRow.WorkspaceID != running.Verification.WorkspaceID ||
		resultRow.State != task.ResultSealed || resultRow.ResultCommit != running.Verification.VerifiedCommit ||
		resultRow.RepositoryID != taskRow.RepositoryID || resultRow.BaseSHA != taskRow.BaseSHA || resultRow.BaseSHA != attempt.BaseSHA ||
		taskRow.SealedResultID != resultRow.ID || attempt.SealedResultID != resultRow.ID {
		return c.recover(ctx, running.Verification, taskRow.Revision, attempt.Revision, "verification_result_integrity_failed", "integrity_failure", nil)
	}
	result, runErr := c.runner.Run(ctx, c.policy, verification.Request{RepositoryID: resultRow.RepositoryID,
		BaseSHA: resultRow.BaseSHA, ResultCommit: resultRow.ResultCommit, RepositoryPath: c.config.RepositoryPath})
	return c.recordResult(ctx, running.Verification, taskRow.Revision, attempt.Revision, result, runErr)
}

func (c *Coordinator) recordResult(ctx context.Context, v taskstore.Verification, taskRevision, attemptRevision int64, result verification.Result, runErr error) error {
	if runErr != nil || result.Failure == verification.FailureIntegrity || result.Failure == verification.FailureStart || result.Failure == verification.FailurePreflight {
		outcome, reason := "runner_failure", "verification_runner_failed"
		switch result.Failure {
		case verification.FailureIntegrity:
			outcome, reason = "integrity_failure", "verification_integrity_failed"
		case verification.FailureStart:
			outcome, reason = "start_failed", "verification_start_failed"
		case verification.FailurePreflight:
			reason = "verification_preflight_failed"
		}
		return c.recover(ctx, v, taskRevision, attemptRevision, reason, outcome, &result)
	}
	state, outcome, reason := taskstore.VerificationFailed, "runner_failure", "verification_runner_failed"
	var exitCode *int
	signal := result.Signal
	switch {
	case result.Success && result.Executed && result.ExitCode == 0 && result.Signal == "":
		state, outcome, reason = taskstore.VerificationSucceeded, "passed", ""
		exit := 0
		exitCode = &exit
	case result.TimedOut:
		outcome, reason, signal = "timeout", "verification_timeout", ""
	case result.Cancelled:
		outcome, reason, signal = "canceled", "verification_canceled", ""
	case result.Signal != "":
		outcome, reason = "signaled", "verification_signaled"
	case result.ExitCode > 0 && result.ExitCode <= 255:
		outcome, reason = "exit_nonzero", "verification_exit_nonzero"
		exit := result.ExitCode
		exitCode = &exit
	default:
		return c.recover(ctx, v, taskRevision, attemptRevision, reason, outcome, &result)
	}
	stdout, stderr := output(result.Stdout), output(result.Stderr)
	eventID, err := c.ids.EventID()
	if err != nil {
		return c.recover(ctx, v, taskRevision, attemptRevision, "verification_completion_id_failed", "runner_failure", &result)
	}
	evidence := evidencePayload("completed", outcome)
	commitCtx, cancel := c.commitContext(ctx)
	defer cancel()
	_, err = c.store.CompleteVerification(commitCtx, taskstore.CompleteVerificationParams{
		VerificationID: v.ID, ExpectedRevision: v.Revision, ExpectedTaskRevision: taskRevision,
		ExpectedAttemptRevision: attemptRevision, EventID: eventID, State: state, Outcome: outcome,
		ExitCode: exitCode, Signal: signal, Stdout: stdout, Stderr: stderr, Reason: reason,
		EndedAt: c.nowAfter(v.UpdatedAt), EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), Actor: c.config.Actor,
	})
	if err == nil {
		return nil
	}
	return c.resolveCompletionError(commitCtx, v, taskRevision, attemptRevision, result, err)
}

func (c *Coordinator) recoverRestart(ctx context.Context, record taskstore.VerificationRecord) error {
	taskRow, attempt, err := c.owners(ctx, record.Verification)
	if err != nil {
		return err
	}
	return c.recover(ctx, record.Verification, taskRow.Revision, attempt.Revision,
		"verification_process_restarted", "runner_failure", nil)
}

func (c *Coordinator) recover(ctx context.Context, v taskstore.Verification, taskRevision, attemptRevision int64, reason, outcome string, result *verification.Result) error {
	eventID, err := c.ids.EventID()
	if err != nil {
		return err
	}
	var stdout, stderr *taskstore.VerificationOutput
	if v.State == taskstore.VerificationRunning {
		zero := taskstore.VerificationOutput{SHA256: sha256.Sum256(nil)}
		stdout, stderr = &zero, &zero
		if result != nil {
			out, errOut := checkedOutput(result.Stdout, v.OutputLimitBytes)
			errOutValue, errErr := checkedOutput(result.Stderr, v.OutputLimitBytes)
			if errOut == nil && errErr == nil {
				stdout, stderr = &out, &errOutValue
			}
		}
		if outcome == "" || outcome == "passed" {
			outcome = "runner_failure"
		}
	} else {
		outcome = ""
	}
	evidence := evidencePayload("recovery_required", outcome)
	commitCtx, cancel := c.commitContext(ctx)
	defer cancel()
	_, err = c.store.RecoverVerification(commitCtx, taskstore.RecoverVerificationParams{
		VerificationID: v.ID, ExpectedRevision: v.Revision, ExpectedTaskRevision: taskRevision,
		ExpectedAttemptRevision: attemptRevision, EventID: eventID, Reason: reason, Outcome: outcome,
		Stdout: stdout, Stderr: stderr, RecoveredAt: c.nowAfter(v.UpdatedAt), EvidencePayload: evidence,
		EvidenceSHA256: sha256.Sum256(evidence), Actor: c.config.RecoveryActor,
	})
	if errors.Is(err, taskstore.ErrInvalidState) || errors.Is(err, taskstore.ErrStaleRevision) {
		current, inspectErr := c.store.InspectVerification(commitCtx, v.ID)
		if inspectErr == nil && terminal(current.Verification.State) {
			return nil
		}
	}
	return err
}

func (c *Coordinator) resolveAdvanceError(ctx context.Context, id task.VerificationID, original error) error {
	inspectCtx, cancel := c.commitContext(ctx)
	defer cancel()
	current, err := c.store.InspectVerification(inspectCtx, id)
	if err != nil {
		return original
	}
	if current.Verification.State != taskstore.VerificationRunning {
		if terminal(current.Verification.State) {
			return nil
		}
		return original
	}
	taskRow, attempt, err := c.owners(inspectCtx, current.Verification)
	if err != nil {
		return err
	}
	return c.recover(inspectCtx, current.Verification, taskRow.Revision, attempt.Revision,
		"verification_start_ambiguous", "runner_failure", nil)
}

func (c *Coordinator) resolveCompletionError(ctx context.Context, v taskstore.Verification, taskRevision, attemptRevision int64, result verification.Result, original error) error {
	current, err := c.store.InspectVerification(ctx, v.ID)
	if err == nil && terminal(current.Verification.State) {
		return nil
	}
	if err == nil && current.Verification.State == taskstore.VerificationRunning {
		return c.recover(ctx, current.Verification, taskRevision, attemptRevision,
			"verification_completion_ambiguous", "runner_failure", &result)
	}
	return original
}

func (c *Coordinator) owners(ctx context.Context, v taskstore.Verification) (taskstore.Task, taskstore.Attempt, error) {
	taskRow, err := c.store.GetTask(ctx, v.TaskID)
	if err != nil {
		return taskstore.Task{}, taskstore.Attempt{}, err
	}
	attempt, err := c.store.GetAttempt(ctx, v.AttemptID)
	return taskRow, attempt, err
}

func (c *Coordinator) matchesSnapshot(v taskstore.Verification) bool {
	return v.PolicyName == c.policySnapshot.CheckName && v.PolicySHA256 == c.policySnapshot.SHA256 &&
		v.WorkingDirectory == c.policySnapshot.WorkingDirectory && v.Timeout == c.policySnapshot.Timeout &&
		v.OutputLimitBytes == int64(c.policySnapshot.OutputBytes) && v.RunnerName == c.runnerSnapshot.Name &&
		v.RunnerVersion == c.runnerSnapshot.Version && v.ImageDigest == c.runnerSnapshot.ImageDigest &&
		v.EnvironmentSHA256 == c.runnerSnapshot.EnvironmentSHA256
}

func checkedOutput(value verification.OutputEvidence, limit int64) (taskstore.VerificationOutput, error) {
	retained := int64(len(value.Prefix))
	if value.ByteCount < 0 || retained > value.ByteCount || retained > limit || value.Truncated != (value.ByteCount > retained) ||
		(value.Truncated && retained != limit) {
		return taskstore.VerificationOutput{}, errors.New("invalid verification output accounting")
	}
	return taskstore.VerificationOutput{ByteCount: value.ByteCount, RetainedBytes: retained,
		SHA256: value.SHA256, Truncated: value.Truncated}, nil
}

func output(value verification.OutputEvidence) taskstore.VerificationOutput {
	return taskstore.VerificationOutput{ByteCount: value.ByteCount, RetainedBytes: int64(len(value.Prefix)),
		SHA256: value.SHA256, Truncated: value.Truncated}
}

func evidencePayload(stage, outcome string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Schema  string `json:"schema"`
		Stage   string `json:"stage"`
		Outcome string `json:"outcome,omitempty"`
	}{"fern.taskverification.v1", stage, outcome})
	return payload
}

func terminal(state taskstore.VerificationState) bool {
	return state == taskstore.VerificationSucceeded || state == taskstore.VerificationFailed || state == taskstore.VerificationRecoveryRequired
}

func (c *Coordinator) nowAfter(previous time.Time) time.Time {
	now := c.config.Now().UTC().Truncate(time.Millisecond)
	previous = previous.UTC().Truncate(time.Millisecond)
	if now.Before(previous) {
		return previous
	}
	return now
}

func (c *Coordinator) commitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
