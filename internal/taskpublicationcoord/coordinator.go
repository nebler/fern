// Package taskpublicationcoord coordinates durable publication journal phases
// with the stateless GitHub publication broker.
package taskpublicationcoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskpublication"
	"github.com/nebler/fern/internal/taskstore"
)

var (
	ErrNoWork                = errors.New("no task publication work is available")
	ErrReconciliationPending = errors.New("task publication reconciliation is not conclusive")
	ErrFenceFailed           = errors.New("task publication fence acquisition failed")
	ErrSelectionChanged      = errors.New("task publication selection changed while fenced")
)

type Store interface {
	FindPublicationWork(context.Context, task.WorkspaceID) (taskstore.PublicationWork, error)
	AdvancePublication(context.Context, taskstore.AdvancePublicationParams) (taskstore.PublicationRecord, error)
	CompletePublication(context.Context, taskstore.CompletePublicationParams) (taskstore.PublicationRecord, error)
	RecoverPublication(context.Context, taskstore.RecoverPublicationParams) (taskstore.PublicationRecord, error)
}

type Fencer interface {
	AcquirePaused(context.Context) (func(), error)
}

// Publisher exposes one durable-effect boundary per call. Reconcile methods
// are read-only and Once methods issue no more than one mutation.
type Publisher interface {
	ReconcileBranch(context.Context, taskpublication.Request) (taskpublication.BranchObservation, error)
	PushOnce(context.Context, taskpublication.Request) (taskpublication.BranchProof, error)
	ReconcilePullRequest(context.Context, taskpublication.Request) (taskpublication.PullRequestProof, error)
	CreatePullRequestOnce(context.Context, taskpublication.Request) (taskpublication.PullRequestProof, error)
}

type Config struct {
	WorkspaceID      task.WorkspaceID
	PullRequestBody  string
	OperationTimeout time.Duration
	PollInterval     time.Duration
	Actor            task.ActorSnapshot
	RecoveryActor    task.ActorSnapshot
	Now              func() time.Time
	OnError          func(error)
}

type Coordinator struct {
	store     Store
	fencer    Fencer
	publisher Publisher
	ids       *task.Generator
	config    Config
	wake      chan struct{}
	runMu     sync.Mutex
}

func New(store Store, fencer Fencer, publisher Publisher, ids *task.Generator, config Config) (*Coordinator, error) {
	if store == nil || fencer == nil || publisher == nil || ids == nil {
		return nil, errors.New("task publication dependencies are required")
	}
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return nil, errors.New("valid task publication workspace is required")
	}
	if config.OperationTimeout <= 0 || config.OperationTimeout > 5*time.Minute {
		return nil, errors.New("task publication operation timeout must be positive and at most five minutes")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if len(config.PullRequestBody) > 60<<10 || !utf8.ValidString(config.PullRequestBody) || strings.ContainsRune(config.PullRequestBody, 0) {
		return nil, errors.New("valid task publication pull request body is required")
	}
	if err := config.Actor.Validate(); err != nil || config.Actor.Type != task.ActorSystem {
		return nil, errors.New("valid system publication actor is required")
	}
	if err := config.RecoveryActor.Validate(); err != nil || config.RecoveryActor.Type != task.ActorRecovery {
		return nil, errors.New("valid recovery publication actor is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.OnError == nil {
		config.OnError = func(error) {}
	}
	return &Coordinator{store: store, fencer: fencer, publisher: publisher, ids: ids, config: config, wake: make(chan struct{}, 1)}, nil
}

// Wake requests a publication pass without blocking the caller.
func (coordinator *Coordinator) Wake() {
	select {
	case coordinator.wake <- struct{}{}:
	default:
	}
}

func (coordinator *Coordinator) Run(ctx context.Context) error {
	retry := observability.NewRetry(coordinator.config.PollInterval, 30*time.Second)
	var delay time.Duration
	for {
		if err := observability.Wait(ctx, coordinator.wake, delay); err != nil {
			return err
		}
		failed := false
		for {
			err := coordinator.RunOnce(ctx)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrNoWork) && !errors.Is(err, ErrReconciliationPending) {
				if errors.Is(err, taskstore.ErrCorruptStore) {
					return err
				}
				coordinator.config.OnError(err)
				failed = true
			}
			break
		}
		if failed {
			delay = retry.Next()
		} else {
			retry.Reset()
			delay = coordinator.config.PollInterval
		}
	}
}

// RunOnce advances at most one selected publication. A phase found on entry is
// reconciled read-only; a mutation occurs only immediately after this call has
// durably committed its corresponding started phase.
func (coordinator *Coordinator) RunOnce(ctx context.Context) error {
	coordinator.runMu.Lock()
	defer coordinator.runMu.Unlock()
	work, err := coordinator.store.FindPublicationWork(ctx, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	operationContext, cancel := coordinator.operationContext(ctx)
	release, err := coordinator.fencer.AcquirePaused(operationContext)
	cancel()
	if err != nil {
		return classifiedError{kind: ErrFenceFailed, cause: err}
	}
	if release == nil {
		return ErrFenceFailed
	}
	defer release()

	current, err := coordinator.store.FindPublicationWork(ctx, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return classifiedError{kind: ErrSelectionChanged, cause: err}
	}
	if err != nil {
		return err
	}
	if !sameSelection(work, current) {
		return ErrSelectionChanged
	}
	work = current
	request := publicationRequest(work, coordinator.config.PullRequestBody)

	switch work.Publication.EffectPhase {
	case taskstore.PublicationPhaseNone:
		advanced, err := coordinator.advance(ctx, work, taskstore.PublicationPhaseNone, taskstore.PublicationPhasePushStarted, "push_authorized", "unobserved", "unobserved", "")
		if err != nil {
			return err
		}
		work.Publication = advanced.Publication
		operationContext, cancel := coordinator.operationContext(ctx)
		proof, pushErr := coordinator.publisher.PushOnce(operationContext, request)
		cancel()
		if proof.Observation.Exists && proof.Observation.SHA == work.Publication.Tuple.ResultCommit {
			return coordinator.observePush(ctx, work, proofEvidence("push", proof))
		}
		return coordinator.effectError(ctx, work, "push", pushErr, proof.Push.Attempted, proofEvidenceState("push", proof, stateForError(pushErr)))
	case taskstore.PublicationPhasePushStarted:
		operationContext, cancel := coordinator.operationContext(ctx)
		observation, readErr := coordinator.publisher.ReconcileBranch(operationContext, request)
		cancel()
		if readErr != nil {
			return coordinator.readError(ctx, work, "push_reconcile", readErr)
		}
		if observation.Exists && observation.SHA == work.Publication.Tuple.ResultCommit {
			return coordinator.observePush(ctx, work, branchEvidence("push_reconcile", observation, "exact"))
		}
		if observation.Exists {
			return coordinator.recover(ctx, work, taskstore.PublicationConflict, "remote_branch_conflict", branchEvidence("push_reconcile", observation, "conflict"))
		}
		return coordinator.recover(ctx, work, taskstore.PublicationRecoveryRequired, "push_not_observed", branchEvidence("push_reconcile", observation, "absent"))
	case taskstore.PublicationPhasePushObserved:
		operationContext, cancel := coordinator.operationContext(ctx)
		proof, readErr := coordinator.publisher.ReconcilePullRequest(operationContext, request)
		cancel()
		if readErr != nil {
			return coordinator.readError(ctx, work, "pr_reconcile", readErr)
		}
		if proof.Found {
			return coordinator.complete(ctx, work, proof.Observation, pullEvidence("pr_reconcile", proof, "exact"))
		}
		advanced, err := coordinator.advance(ctx, work, taskstore.PublicationPhasePushObserved, taskstore.PublicationPhasePRCreateStarted, "pr_create_authorized", "exact", "absent", work.Publication.Tuple.ResultCommit)
		if err != nil {
			return err
		}
		work.Publication = advanced.Publication
		operationContext, cancel = coordinator.operationContext(ctx)
		created, createErr := coordinator.publisher.CreatePullRequestOnce(operationContext, request)
		cancel()
		if created.Found {
			return coordinator.complete(ctx, work, created.Observation, pullEvidence("pr_create", created, "exact"))
		}
		return coordinator.effectError(ctx, work, "pr_create", createErr, created.CreateAttempted, pullEvidence("pr_create", created, stateForError(createErr)))
	case taskstore.PublicationPhasePRCreateStarted:
		operationContext, cancel := coordinator.operationContext(ctx)
		proof, readErr := coordinator.publisher.ReconcilePullRequest(operationContext, request)
		cancel()
		if readErr != nil {
			return coordinator.readError(ctx, work, "pr_reconcile", readErr)
		}
		if proof.Found {
			return coordinator.complete(ctx, work, proof.Observation, pullEvidence("pr_reconcile", proof, "exact"))
		}
		return coordinator.recover(ctx, work, taskstore.PublicationRecoveryRequired, "pull_request_not_observed", pullEvidence("pr_reconcile", proof, "absent"))
	default:
		return coordinator.recover(ctx, work, taskstore.PublicationRecoveryRequired, "invalid_publication_phase", staticEvidence("phase", "conflict", "unobserved", "unobserved", ""))
	}
}

func sameSelection(first, second taskstore.PublicationWork) bool {
	return first.Publication.ID == second.Publication.ID && first.Publication.Revision == second.Publication.Revision &&
		first.Task.ID == second.Task.ID && first.Task.Revision == second.Task.Revision &&
		first.Attempt.ID == second.Attempt.ID && first.Attempt.Revision == second.Attempt.Revision &&
		first.Result.ID == second.Result.ID && first.Result.Revision == second.Result.Revision &&
		first.Verification.ID == second.Verification.ID && first.Verification.Revision == second.Verification.Revision
}

func publicationRequest(work taskstore.PublicationWork, body string) taskpublication.Request {
	return taskpublication.Request{
		WorkspaceRepository: work.Task.RepositoryID,
		Task:                task.RepositoryTuple{RepositoryID: work.Task.RepositoryID, BaseSHA: work.Task.BaseSHA},
		Result: task.ResultTuple{RepositoryTuple: task.RepositoryTuple{RepositoryID: work.Result.RepositoryID, BaseSHA: work.Result.BaseSHA},
			ResultCommit: work.Result.ResultCommit, Outcome: work.Result.Outcome, ManifestEntries: work.Result.ManifestEntries, WorktreeClean: work.Result.WorktreeClean},
		Verification: task.VerificationTuple{State: task.VerificationState(work.Verification.State), VerifiedCommit: work.Verification.VerifiedCommit},
		Publication:  work.Publication.Tuple, Title: work.Task.Title, Body: body,
	}
}

func (coordinator *Coordinator) observePush(ctx context.Context, work taskstore.PublicationWork, payload json.RawMessage) error {
	_, err := coordinator.advanceWithSHA(ctx, work, taskstore.PublicationPhasePushStarted, taskstore.PublicationPhasePushObserved, work.Publication.Tuple.ResultCommit, payload)
	return err
}

func (coordinator *Coordinator) advance(ctx context.Context, work taskstore.PublicationWork, from, to taskstore.PublicationPhase, stage, branch, pull string, sha task.GitOID) (taskstore.PublicationRecord, error) {
	return coordinator.advanceWithSHA(ctx, work, from, to, sha, staticEvidence(stage, branch, pull, "unobserved", sha))
}

func (coordinator *Coordinator) advanceWithSHA(ctx context.Context, work taskstore.PublicationWork, from, to taskstore.PublicationPhase, sha task.GitOID, payload json.RawMessage) (taskstore.PublicationRecord, error) {
	eventID, err := coordinator.ids.EventID()
	if err != nil {
		return taskstore.PublicationRecord{}, err
	}
	return coordinator.store.AdvancePublication(ctx, taskstore.AdvancePublicationParams{
		PublicationID: work.Publication.ID, ExpectedRevision: work.Publication.Revision,
		ExpectedTaskRevision: work.Task.Revision, ExpectedAttemptRevision: work.Attempt.Revision,
		From: from, To: to, ObservedRemoteSHA: sha, EventID: eventID, AdvancedAt: coordinator.now(),
		EvidencePayload: payload, EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
}

func (coordinator *Coordinator) complete(ctx context.Context, work taskstore.PublicationWork, observation task.PublicationObservation, payload json.RawMessage) error {
	eventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	_, err = coordinator.store.CompletePublication(ctx, taskstore.CompletePublicationParams{
		PublicationID: work.Publication.ID, ExpectedRevision: work.Publication.Revision,
		ExpectedTaskRevision: work.Task.Revision, ExpectedAttemptRevision: work.Attempt.Revision,
		EventID: eventID, Observation: observation, CompletedAt: coordinator.now(), EvidencePayload: payload,
		EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) recover(ctx context.Context, work taskstore.PublicationWork, state taskstore.PublicationState, reason string, payload json.RawMessage) error {
	if state == taskstore.PublicationUncertain && work.Publication.State == taskstore.PublicationUncertain {
		return ErrReconciliationPending
	}
	eventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	_, err = coordinator.store.RecoverPublication(ctx, taskstore.RecoverPublicationParams{
		PublicationID: work.Publication.ID, ExpectedRevision: work.Publication.Revision,
		ExpectedTaskRevision: work.Task.Revision, ExpectedAttemptRevision: work.Attempt.Revision,
		EventID: eventID, State: state, Reason: reason, RecoveredAt: coordinator.now(), EvidencePayload: payload,
		EvidenceSHA256: sha256.Sum256(payload), Actor: coordinator.config.RecoveryActor,
	})
	return err
}

func (coordinator *Coordinator) readError(ctx context.Context, work taskstore.PublicationWork, stage string, err error) error {
	payload := staticEvidence(stage, stateForError(err), "unobserved", "unobserved", "")
	if permanentConflict(err) {
		return coordinator.recover(ctx, work, taskstore.PublicationConflict, stage+"_conflict", payload)
	}
	if permanentFailure(err) {
		return coordinator.recover(ctx, work, taskstore.PublicationRecoveryRequired, stage+"_invalid", payload)
	}
	return coordinator.recover(ctx, work, taskstore.PublicationUncertain, stage+"_unavailable", payload)
}

func (coordinator *Coordinator) effectError(ctx context.Context, work taskstore.PublicationWork, stage string, err error, attempted bool, payload json.RawMessage) error {
	if permanentConflict(err) {
		return coordinator.recover(ctx, work, taskstore.PublicationConflict, stage+"_conflict", payload)
	}
	if !attempted || permanentFailure(err) {
		return coordinator.recover(ctx, work, taskstore.PublicationRecoveryRequired, stage+"_failed_before_observation", payload)
	}
	return coordinator.recover(ctx, work, taskstore.PublicationUncertain, stage+"_result_unknown", payload)
}

func permanentConflict(err error) bool {
	return errors.Is(err, taskpublication.ErrRepositoryConflict) || errors.Is(err, taskpublication.ErrBaseMoved) ||
		errors.Is(err, taskpublication.ErrBranchConflict) || errors.Is(err, taskpublication.ErrPullRequestConflict) ||
		errors.Is(err, githubapp.ErrAmbiguousPullRequests) || errors.Is(err, githubapp.ErrPullRequestConflict)
}

func permanentFailure(err error) bool {
	return errors.Is(err, taskpublication.ErrInvalidConfiguration) || errors.Is(err, taskpublication.ErrInvalidRequest) ||
		errors.Is(err, taskpublication.ErrGitFailed)
}

// stateForError maps publication errors into the publication recovery-state
// vocabulary ("inconclusive", "conflict", "invalid", "unavailable"); it is
// distinct from taskdelivery's deliveryStateForError, which only distinguishes
// "conflict" and "unavailable".
func stateForError(err error) string {
	if err == nil {
		return "inconclusive"
	}
	if permanentConflict(err) {
		return "conflict"
	}
	if permanentFailure(err) {
		return "invalid"
	}
	return "unavailable"
}

// classifiedError keeps dependency output out of rendered errors while
// retaining errors.Is classification for callers and tests.
type classifiedError struct {
	kind  error
	cause error
}

func (err classifiedError) Error() string   { return err.kind.Error() }
func (err classifiedError) Unwrap() []error { return []error{err.kind, err.cause} }

type evidence struct {
	Stage           string `json:"stage"`
	Branch          string `json:"branch"`
	PullRequest     string `json:"pullRequest"`
	Mutation        string `json:"mutation"`
	RemoteSHA       string `json:"remoteSha,omitempty"`
	ExitCode        int    `json:"exitCode,omitempty"`
	TimedOut        bool   `json:"timedOut,omitempty"`
	StdoutBytes     int64  `json:"stdoutBytes,omitempty"`
	StdoutSHA256    string `json:"stdoutSha256,omitempty"`
	StderrBytes     int64  `json:"stderrBytes,omitempty"`
	StderrSHA256    string `json:"stderrSha256,omitempty"`
	PRNumber        int64  `json:"prNumber,omitempty"`
	CreateAttempted bool   `json:"createAttempted,omitempty"`
	CreateConfirmed bool   `json:"createConfirmed,omitempty"`
}

func staticEvidence(stage, branch, pull, mutation string, sha task.GitOID) json.RawMessage {
	payload, _ := json.Marshal(evidence{Stage: stage, Branch: branch, PullRequest: pull, Mutation: mutation, RemoteSHA: string(sha)})
	return payload
}

func branchEvidence(stage string, observation taskpublication.BranchObservation, state string) json.RawMessage {
	return staticEvidence(stage, state, "unobserved", "none", observation.SHA)
}

func proofEvidence(stage string, proof taskpublication.BranchProof) json.RawMessage {
	return proofEvidenceState(stage, proof, "exact")
}

func proofEvidenceState(stage string, proof taskpublication.BranchProof, state string) json.RawMessage {
	value := evidence{Stage: stage, Branch: state, PullRequest: "unobserved", Mutation: "push_once", RemoteSHA: string(proof.Observation.SHA),
		ExitCode: proof.Push.ExitCode, TimedOut: proof.Push.TimedOut, StdoutBytes: proof.Push.Stdout.Bytes,
		StdoutSHA256: hex.EncodeToString(proof.Push.Stdout.SHA256[:]), StderrBytes: proof.Push.Stderr.Bytes,
		StderrSHA256: hex.EncodeToString(proof.Push.Stderr.SHA256[:])}
	payload, _ := json.Marshal(value)
	return payload
}

func pullEvidence(stage string, proof taskpublication.PullRequestProof, state string) json.RawMessage {
	value := evidence{Stage: stage, Branch: "exact", PullRequest: state, Mutation: "none", CreateAttempted: proof.CreateAttempted, CreateConfirmed: proof.CreateConfirmed}
	if stage == "pr_create" {
		value.Mutation = "create_once"
	}
	if proof.Found {
		value.RemoteSHA = string(proof.Observation.RemoteSHA)
		value.PRNumber = int64(proof.Observation.PullRequest.Number)
	}
	payload, _ := json.Marshal(value)
	return payload
}

func (coordinator *Coordinator) now() time.Time {
	return coordinator.config.Now().UTC().Truncate(time.Millisecond)
}

func (coordinator *Coordinator) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, coordinator.config.OperationTimeout)
}
