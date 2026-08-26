// Package taskresultcoord seals exact repository snapshots from either an
// external success authority or an explicit durable user authorization.
package taskresultcoord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskresult"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const (
	MaxPolicyVersionBytes  = 128
	MaxRepositoryPathBytes = 4096
	MaxOperationTimeout    = 5 * time.Minute
)

var (
	ErrInvalidConfig       = errors.New("invalid task result coordinator configuration")
	ErrNoWork              = errors.New("no task result work is available")
	ErrFenceFailed         = errors.New("task result fence acquisition failed")
	ErrObservationFailed   = errors.New("authoritative result observation failed")
	ErrObservationMismatch = errors.New("authoritative result observations differ")
	ErrSelectionChanged    = errors.New("task result selection changed while fenced")
	ErrCollectionFailed    = errors.New("task result collection failed")
)

// Store exposes only durable success discovery and atomic result sealing. The
// discovery method must return success recorded by an external authoritative
// execution projector; the coordinator never creates that projection.
type Store interface {
	FindSucceededUnsealedAttempt(context.Context, task.WorkspaceID) (taskstore.DeliveryWork, error)
	SealResult(context.Context, taskstore.SealResultParams) (taskstore.SealedResult, error)
}

type authorizedStore interface {
	ClaimSealRequest(context.Context, taskstore.ClaimSealRequestParams) (taskstore.SealRequestWork, error)
	InspectClaimedSealRequest(context.Context, task.SealRequestID, string, int64) (taskstore.SealRequestWork, error)
	RejectSealRequest(context.Context, taskstore.RejectSealRequestParams) (taskstore.SealRequest, error)
	SealAuthorizedResult(context.Context, taskstore.SealAuthorizedResultParams) (taskstore.SealedResult, error)
}

type Fencer interface {
	AcquireQuiesced(context.Context, func(context.Context, workspace.RequestTarget) error) (func(), error)
}

type Collector interface {
	Collect(context.Context, taskresult.Request) (taskresult.Result, error)
}

type IDGenerator interface {
	ResultID() (task.ResultID, error)
	EventID() (task.EventID, error)
}

// SuccessIdentity is the complete persisted identity an Observer must
// authoritatively re-prove. Revisions bind the proof to the selected immutable
// task and attempt snapshots.
type SuccessIdentity struct {
	WorkspaceID     task.WorkspaceID
	TaskID          task.TaskID
	AttemptID       task.AttemptID
	TaskRevision    int64
	AttemptRevision int64
	SessionID       task.OpenCodeSessionID
	MessageID       task.OpenCodeMessageID
}

// Observation is sanitized evidence of exact terminal success. Evidence must
// be a bounded JSON object and EvidenceSHA256 must digest its original bytes.
type Observation struct {
	Identity       SuccessIdentity
	Evidence       json.RawMessage
	EvidenceSHA256 [32]byte
	PolicyVersion  string
}

// Observer is an external authoritative proof boundary. Implementations must
// inspect the supplied attested target and must not infer success from an
// inactive, idle, absent, or empty state.
type Observer interface {
	ObserveSucceeded(context.Context, workspace.RequestTarget, SuccessIdentity) (Observation, error)
}

type Config struct {
	WorkspaceID      task.WorkspaceID
	RepositoryPath   string
	PolicyVersion    string
	OperationTimeout time.Duration
	Actor            task.ActorSnapshot
	ClaimOwner       string
	Now              func() time.Time
}

type Coordinator struct {
	store       Store
	fencer      Fencer
	pauseFencer PauseFencer
	collector   Collector
	observer    Observer
	ids         IDGenerator
	config      Config
	runMu       sync.Mutex
}

var (
	_ Store       = (*taskstore.Store)(nil)
	_ Fencer      = (*workspace.Manager)(nil)
	_ PauseFencer = (*workspace.Manager)(nil)
	_ Collector   = (*taskresult.Collector)(nil)
	_ IDGenerator = (*task.Generator)(nil)
)

func New(store Store, fencer Fencer, collector Collector, observer Observer, ids IDGenerator, config Config) (*Coordinator, error) {
	if store == nil || fencer == nil || collector == nil || observer == nil || ids == nil {
		return nil, ErrInvalidConfig
	}
	if err := validateConfig(&config); err != nil {
		return nil, err
	}
	var pauseFencer PauseFencer
	if _, ok := store.(authorizedStore); ok {
		pauseFencer, ok = fencer.(PauseFencer)
		if !ok {
			return nil, ErrInvalidConfig
		}
	}
	return &Coordinator{store: store, fencer: fencer, pauseFencer: pauseFencer, collector: collector, observer: observer, ids: ids, config: config}, nil
}

// NewAuthorized constructs a coordinator that consumes only explicit durable
// user seal requests. It never discovers or infers execution success.
func NewAuthorized(store Store, fencer PauseFencer, collector Collector, config Config) (*Coordinator, error) {
	if store == nil || fencer == nil || collector == nil {
		return nil, ErrInvalidConfig
	}
	if _, ok := store.(authorizedStore); !ok {
		return nil, ErrInvalidConfig
	}
	if err := validateConfig(&config); err != nil {
		return nil, err
	}
	return &Coordinator{store: store, pauseFencer: fencer, collector: collector, config: config}, nil
}

func validateConfig(config *Config) error {
	if _, err := task.ParseWorkspaceID(string(config.WorkspaceID)); err != nil {
		return ErrInvalidConfig
	}
	if len(config.RepositoryPath) < 1 || len(config.RepositoryPath) > MaxRepositoryPathBytes ||
		!filepath.IsAbs(config.RepositoryPath) || filepath.Clean(config.RepositoryPath) != config.RepositoryPath ||
		strings.ContainsAny(config.RepositoryPath, "\x00\r\n") {
		return ErrInvalidConfig
	}
	if !validPolicyVersion(config.PolicyVersion) || config.OperationTimeout <= 0 || config.OperationTimeout > MaxOperationTimeout {
		return ErrInvalidConfig
	}
	if err := config.Actor.Validate(); err != nil || (config.Actor.Type != task.ActorSystem && config.Actor.Type != task.ActorRecovery) {
		return ErrInvalidConfig
	}
	if config.ClaimOwner == "" {
		config.ClaimOwner = config.Actor.ID
	}
	if len(config.ClaimOwner) > 64 || !validPolicyVersion(config.ClaimOwner) {
		return ErrInvalidConfig
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return nil
}

// RunOnce seals at most one result. It does not poll, infer terminal success,
// record execution state, or retry SealResult.
func (coordinator *Coordinator) RunOnce(ctx context.Context) error {
	coordinator.runMu.Lock()
	defer coordinator.runMu.Unlock()
	if store, ok := coordinator.store.(authorizedStore); ok {
		now := coordinator.config.Now().UTC().Truncate(time.Millisecond)
		if now.IsZero() || now.UnixMilli() < 0 {
			return ErrInvalidConfig
		}
		work, claimErr := store.ClaimSealRequest(ctx, taskstore.ClaimSealRequestParams{
			WorkspaceID: coordinator.config.WorkspaceID, ClaimOwner: coordinator.config.ClaimOwner,
			Now: now, LeaseExpiresAt: now.Add(coordinator.config.OperationTimeout),
		})
		if claimErr == nil {
			return coordinator.runAuthorized(ctx, store, work)
		}
		if !errors.Is(claimErr, taskstore.ErrNotFound) {
			return claimErr
		}
	}
	if coordinator.observer == nil {
		return ErrNoWork
	}

	selected, err := coordinator.store.FindSucceededUnsealedAttempt(ctx, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrNoWork
	}
	if err != nil {
		return err
	}
	identity := identityFor(selected)

	operationContext, cancelOperation := context.WithTimeout(ctx, coordinator.config.OperationTimeout)
	defer cancelOperation()
	authorityDeadline, _ := operationContext.Deadline()

	observations := make([]Observation, 0, 2)
	release, err := coordinator.fencer.AcquireQuiesced(operationContext, func(observeContext context.Context, target workspace.RequestTarget) error {
		if len(observations) >= 2 {
			return ErrObservationMismatch
		}
		observation, observeErr := coordinator.observer.ObserveSucceeded(observeContext, target, identity)
		if observeErr != nil {
			return classifiedError{kind: ErrObservationFailed, cause: observeErr}
		}
		if err := validateObservation(observation, identity, coordinator.config.PolicyVersion); err != nil {
			return err
		}
		observation.Evidence = append(json.RawMessage(nil), observation.Evidence...)
		observations = append(observations, observation)
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrObservationFailed) || errors.Is(err, ErrObservationMismatch) {
			return err
		}
		return classifiedError{kind: ErrFenceFailed, cause: err}
	}
	if release == nil {
		return ErrFenceFailed
	}
	defer release()
	if len(observations) != 2 || !observationsEquivalent(observations[0], observations[1]) {
		return ErrObservationMismatch
	}

	current, err := coordinator.store.FindSucceededUnsealedAttempt(operationContext, coordinator.config.WorkspaceID)
	if errors.Is(err, taskstore.ErrNotFound) {
		return ErrSelectionChanged
	}
	if err != nil {
		return err
	}
	if !sameSelection(selected, current) {
		return ErrSelectionChanged
	}

	proof := observations[1]
	collected, err := coordinator.collector.Collect(operationContext, taskresult.Request{
		RepositoryPath:    coordinator.config.RepositoryPath,
		Repository:        task.RepositoryTuple{RepositoryID: selected.Task.RepositoryID, BaseSHA: selected.Task.BaseSHA},
		OpenCodeSessionID: selected.Attempt.OpenCodeSessionID,
		OpenCodeMessageID: selected.Attempt.OpenCodeMessageID,
		EvidencePayload:   append(json.RawMessage(nil), proof.Evidence...),
		EvidenceSHA256:    proof.EvidenceSHA256,
		PolicyVersion:     proof.PolicyVersion,
	})
	if err != nil {
		return classifiedError{kind: ErrCollectionFailed, cause: err}
	}
	if !validCollectedResult(collected, selected, proof) {
		return ErrCollectionFailed
	}

	resultID, err := coordinator.ids.ResultID()
	if err != nil {
		return err
	}
	resultEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	taskEventID, err := coordinator.ids.EventID()
	if err != nil {
		return err
	}
	sealedAt := coordinator.config.Now().UTC().Truncate(time.Millisecond)
	if sealedAt.IsZero() || sealedAt.UnixMilli() < 0 {
		return ErrInvalidConfig
	}
	if sealedAt.Before(collected.CollectedAt) {
		sealedAt = collected.CollectedAt
	}

	// Collection completed while authority was held. Preserve that authority's
	// original deadline while allowing the atomic seal to survive caller
	// cancellation; no second call is made if the commit result is ambiguous.
	sealContext, cancelSeal := context.WithDeadline(context.WithoutCancel(ctx), authorityDeadline)
	defer cancelSeal()
	_, err = coordinator.store.SealResult(sealContext, taskstore.SealResultParams{
		ResultID: resultID, TaskID: selected.Task.ID, AttemptID: selected.Attempt.ID,
		ExpectedAttemptRevision: selected.Attempt.Revision, ExpectedTaskRevision: selected.Task.Revision,
		ResultEventID: resultEventID, TaskEventID: taskEventID,
		RepositoryID: selected.Task.RepositoryID, BaseSHA: selected.Task.BaseSHA,
		ResultCommit: collected.Tuple.ResultCommit, TreeOID: collected.TreeOID, Outcome: collected.Tuple.Outcome,
		WorktreeClean: collected.Tuple.WorktreeClean, Manifest: append([]taskstore.ManifestEntry(nil), collected.Manifest...),
		ManifestSHA256:    collected.ManifestSHA256,
		OpenCodeSessionID: collected.OpenCodeSessionID, OpenCodeMessageID: collected.OpenCodeMessageID,
		EvidencePayload: append(json.RawMessage(nil), collected.EvidencePayload...), EvidenceSHA256: collected.EvidenceSHA256,
		PolicyVersion: collected.PolicyVersion, CollectedAt: collected.CollectedAt, SealedAt: sealedAt, Actor: coordinator.config.Actor,
	})
	return err
}

func (coordinator *Coordinator) runAuthorized(ctx context.Context, store authorizedStore, selected taskstore.SealRequestWork) error {
	operationContext, cancelOperation := context.WithTimeout(ctx, coordinator.config.OperationTimeout)
	defer cancelOperation()
	authorityDeadline, _ := operationContext.Deadline()

	release, err := coordinator.pauseFencer.AcquirePaused(operationContext)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return classifiedError{kind: ErrFenceFailed, cause: err}
	}
	if release == nil {
		return ErrFenceFailed
	}
	defer release()

	current, err := store.InspectClaimedSealRequest(operationContext, selected.Request.ID, coordinator.config.ClaimOwner, selected.Request.ClaimRevision)
	if err != nil {
		return err
	}
	if !sameAuthorizedSelection(selected, current) {
		coordinator.rejectAuthorized(ctx, store, selected.Request, "authorized_ownership_changed", authorityDeadline)
		return ErrSelectionChanged
	}
	evidence, err := json.Marshal(struct {
		Authority     taskstore.SealCompletionAuthority `json:"authority"`
		SealRequestID task.SealRequestID                `json:"sealRequestId"`
	}{taskstore.SealAuthorityUser, current.Request.ID})
	if err != nil {
		return ErrInvalidConfig
	}
	evidenceHash := sha256.Sum256(evidence)
	expected := &taskresult.SnapshotExpectation{
		ResultCommit: current.Request.ExpectedResultCommit, TreeOID: current.Request.ExpectedTreeOID,
		ManifestEntries: current.Request.ExpectedManifestEntries, ManifestSHA256: current.Request.ExpectedManifestSHA256,
	}
	collected, err := coordinator.collector.Collect(operationContext, taskresult.Request{
		RepositoryPath:    coordinator.config.RepositoryPath,
		Repository:        task.RepositoryTuple{RepositoryID: current.Request.RepositoryID, BaseSHA: current.Request.BaseSHA},
		OpenCodeSessionID: current.Preview.Attempt.OpenCodeSessionID, OpenCodeMessageID: current.Preview.Attempt.OpenCodeMessageID,
		EvidencePayload: evidence, EvidenceSHA256: evidenceHash, PolicyVersion: coordinator.config.PolicyVersion,
		ExpectedSnapshot: expected,
	})
	if err != nil {
		coordinator.rejectAuthorized(ctx, store, current.Request, "authorized_snapshot_changed", authorityDeadline)
		return classifiedError{kind: ErrCollectionFailed, cause: err}
	}
	if !validAuthorizedCollection(collected, current) {
		coordinator.rejectAuthorized(ctx, store, current.Request, "authorized_snapshot_mismatch", authorityDeadline)
		return ErrCollectionFailed
	}
	sealedAt := coordinator.config.Now().UTC().Truncate(time.Millisecond)
	if sealedAt.IsZero() || sealedAt.UnixMilli() < 0 {
		return ErrInvalidConfig
	}
	if sealedAt.Before(collected.CollectedAt) {
		sealedAt = collected.CollectedAt
	}
	authorizer := current.Request.Authorizer
	sealContext, cancelSeal := context.WithDeadline(context.WithoutCancel(ctx), authorityDeadline)
	defer cancelSeal()
	_, err = store.SealAuthorizedResult(sealContext, taskstore.SealAuthorizedResultParams{
		SealRequestID: current.Request.ID, ClaimOwner: coordinator.config.ClaimOwner,
		ExpectedClaimRevision: current.Request.ClaimRevision,
		Result: taskstore.SealResultParams{
			ResultID: current.Request.ResultID, TaskID: current.Request.TaskID, AttemptID: current.Request.AttemptID,
			ExpectedAttemptRevision: current.Request.ExpectedAttemptRevision, ExpectedTaskRevision: current.Request.ExpectedTaskRevision,
			ResultEventID: current.Request.ResultEventID, TaskEventID: current.Request.TaskEventID,
			RepositoryID: current.Request.RepositoryID, BaseSHA: current.Request.BaseSHA,
			ResultCommit: collected.Tuple.ResultCommit, TreeOID: collected.TreeOID, Outcome: collected.Tuple.Outcome,
			WorktreeClean: collected.Tuple.WorktreeClean, Manifest: append([]taskstore.ManifestEntry(nil), collected.Manifest...),
			ManifestSHA256: collected.ManifestSHA256, OpenCodeSessionID: collected.OpenCodeSessionID,
			OpenCodeMessageID: collected.OpenCodeMessageID, EvidencePayload: append(json.RawMessage(nil), collected.EvidencePayload...),
			EvidenceSHA256: collected.EvidenceSHA256, PolicyVersion: collected.PolicyVersion,
			CollectedAt: collected.CollectedAt, SealedAt: sealedAt, Actor: coordinator.config.Actor,
			CompletionAuthority: taskstore.SealAuthorityUser, SealRequestID: current.Request.ID, Authorizer: &authorizer,
		},
	})
	return err
}

func (coordinator *Coordinator) rejectAuthorized(ctx context.Context, store authorizedStore, request taskstore.SealRequest, reason string, deadline time.Time) {
	rejectContext, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	rejectedAt := coordinator.config.Now().UTC().Truncate(time.Millisecond)
	if rejectedAt.Before(request.AcceptedAt) {
		rejectedAt = request.AcceptedAt
	}
	_, _ = store.RejectSealRequest(rejectContext, taskstore.RejectSealRequestParams{
		SealRequestID: request.ID, ClaimOwner: coordinator.config.ClaimOwner, ExpectedClaimRevision: request.ClaimRevision,
		Reason: reason, RejectedAt: rejectedAt,
	})
}

func sameAuthorizedSelection(first, second taskstore.SealRequestWork) bool {
	return first.Request.ID == second.Request.ID && first.Request.State == taskstore.SealRequestClaimed &&
		second.Request.State == taskstore.SealRequestClaimed && first.Request.ClaimRevision == second.Request.ClaimRevision &&
		sameSealRequest(first.Request, second.Request) && samePreview(first.Preview, second.Preview) && authorizedOwnershipCurrent(second)
}

func sameSealRequest(first, second taskstore.SealRequest) bool {
	return first.ID == second.ID && first.ReceiptID == second.ReceiptID && first.WorkspaceID == second.WorkspaceID &&
		first.TaskID == second.TaskID && first.AttemptID == second.AttemptID && first.State == second.State &&
		first.CompletionAuthority == second.CompletionAuthority && first.ExpectedWorkspaceRevision == second.ExpectedWorkspaceRevision &&
		first.ExpectedTaskRevision == second.ExpectedTaskRevision && first.ExpectedAttemptRevision == second.ExpectedAttemptRevision &&
		first.RepositoryID == second.RepositoryID && first.BaseSHA == second.BaseSHA && first.ExpectedResultCommit == second.ExpectedResultCommit &&
		first.ExpectedTreeOID == second.ExpectedTreeOID && first.ExpectedOutcome == second.ExpectedOutcome &&
		first.ExpectedManifestEntries == second.ExpectedManifestEntries && first.ExpectedManifestSHA256 == second.ExpectedManifestSHA256 &&
		first.ExpectedWorktreeClean == second.ExpectedWorktreeClean &&
		first.IdempotencyKey == second.IdempotencyKey && first.RequestHash == second.RequestHash && first.Authorizer == second.Authorizer &&
		first.ResultID == second.ResultID && first.ResultEventID == second.ResultEventID && first.TaskEventID == second.TaskEventID &&
		first.ClaimOwner == second.ClaimOwner && first.ClaimRevision == second.ClaimRevision && first.AcceptedAt.Equal(second.AcceptedAt) &&
		first.ClaimExpiresAt != nil && second.ClaimExpiresAt != nil && first.ClaimExpiresAt.Equal(*second.ClaimExpiresAt)
}

func samePreview(first, second taskstore.SealPreview) bool {
	return first.Workspace.ID == second.Workspace.ID && first.Workspace.Revision == second.Workspace.Revision &&
		first.Task.ID == second.Task.ID && first.Task.Revision == second.Task.Revision && first.Task.State == second.Task.State &&
		first.Task.CancelEpoch == second.Task.CancelEpoch && first.Task.CurrentAttemptID == second.Task.CurrentAttemptID &&
		first.Attempt.ID == second.Attempt.ID && first.Attempt.Revision == second.Attempt.Revision && first.Attempt.State == second.Attempt.State
}

func authorizedOwnershipCurrent(work taskstore.SealRequestWork) bool {
	request, preview := work.Request, work.Preview
	return preview.Workspace.State == taskstore.WorkspaceActive && request.WorkspaceID == preview.Workspace.ID &&
		request.ExpectedWorkspaceRevision == preview.Workspace.Revision && request.TaskID == preview.Task.ID &&
		request.ExpectedTaskRevision == preview.Task.Revision && request.AttemptID == preview.Attempt.ID &&
		request.ExpectedAttemptRevision == preview.Attempt.Revision && preview.Task.CurrentAttemptID == preview.Attempt.ID &&
		preview.Task.CancelEpoch == 0 && preview.Task.SealedResultID == "" && preview.Attempt.SealedResultID == "" &&
		(preview.Task.State == task.TaskRunning || preview.Task.State == task.TaskInputRequired) &&
		(preview.Attempt.State == task.AttemptAdmitted || preview.Attempt.State == task.AttemptRunning || preview.Attempt.State == task.AttemptInputRequired) &&
		request.RepositoryID == preview.Task.RepositoryID && request.BaseSHA == preview.Task.BaseSHA && request.BaseSHA == preview.Attempt.BaseSHA
}

func validAuthorizedCollection(result taskresult.Result, work taskstore.SealRequestWork) bool {
	request := work.Request
	repository := task.RepositoryTuple{RepositoryID: request.RepositoryID, BaseSHA: request.BaseSHA}
	return result.Tuple.ValidateAgainst(repository) == nil && result.Tuple.ResultCommit == request.ExpectedResultCommit &&
		result.TreeOID == request.ExpectedTreeOID && result.Tuple.Outcome == request.ExpectedOutcome &&
		result.Tuple.WorktreeClean == request.ExpectedWorktreeClean &&
		result.Tuple.ManifestEntries == request.ExpectedManifestEntries && len(result.Manifest) == request.ExpectedManifestEntries &&
		result.ManifestSHA256 == request.ExpectedManifestSHA256 && result.OpenCodeSessionID == work.Preview.Attempt.OpenCodeSessionID &&
		result.OpenCodeMessageID == work.Preview.Attempt.OpenCodeMessageID && result.PolicyVersion != "" &&
		!result.CollectedAt.IsZero() && result.CollectedAt.UnixMilli() >= request.AcceptedAt.UnixMilli()
}

func identityFor(work taskstore.DeliveryWork) SuccessIdentity {
	return SuccessIdentity{
		WorkspaceID: work.Task.WorkspaceID, TaskID: work.Task.ID, AttemptID: work.Attempt.ID,
		TaskRevision: work.Task.Revision, AttemptRevision: work.Attempt.Revision,
		SessionID: work.Attempt.OpenCodeSessionID, MessageID: work.Attempt.OpenCodeMessageID,
	}
}

func validateObservation(observation Observation, expected SuccessIdentity, policy string) error {
	if observation.Identity != expected || observation.PolicyVersion != policy || !validPolicyVersion(observation.PolicyVersion) {
		return ErrObservationMismatch
	}
	trimmed := bytes.TrimSpace(observation.Evidence)
	if len(observation.Evidence) < 2 || len(observation.Evidence) > taskresult.MaxEvidenceBytes || len(trimmed) < 2 ||
		trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(observation.Evidence) ||
		sha256.Sum256(observation.Evidence) != observation.EvidenceSHA256 {
		return ErrObservationMismatch
	}
	return nil
}

func observationsEquivalent(first, second Observation) bool {
	if first.Identity != second.Identity {
		return false
	}
	return bytes.Equal(first.Evidence, second.Evidence) ||
		first.EvidenceSHA256 == second.EvidenceSHA256 && first.PolicyVersion == second.PolicyVersion
}

func sameSelection(first, second taskstore.DeliveryWork) bool {
	return identityFor(first) == identityFor(second) &&
		first.Task.CurrentAttemptID == second.Task.CurrentAttemptID &&
		first.Task.RepositoryID == second.Task.RepositoryID && first.Task.BaseSHA == second.Task.BaseSHA &&
		first.Attempt.TaskID == second.Attempt.TaskID && first.Attempt.WorkspaceID == second.Attempt.WorkspaceID &&
		first.Attempt.BaseSHA == second.Attempt.BaseSHA
}

func validCollectedResult(result taskresult.Result, selected taskstore.DeliveryWork, proof Observation) bool {
	repository := task.RepositoryTuple{RepositoryID: selected.Task.RepositoryID, BaseSHA: selected.Task.BaseSHA}
	return result.Tuple.ValidateAgainst(repository) == nil && result.OpenCodeSessionID == selected.Attempt.OpenCodeSessionID &&
		result.OpenCodeMessageID == selected.Attempt.OpenCodeMessageID && result.PolicyVersion == proof.PolicyVersion &&
		result.EvidenceSHA256 == proof.EvidenceSHA256 && bytes.Equal(result.EvidencePayload, proof.Evidence) &&
		len(result.Manifest) == result.Tuple.ManifestEntries && !result.CollectedAt.IsZero() && result.CollectedAt.UnixMilli() >= 0
}

func validPolicyVersion(value string) bool {
	if len(value) < 1 || len(value) > MaxPolicyVersionBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// classifiedError keeps dependency output out of rendered errors while
// retaining errors.Is classification for callers and tests.
type classifiedError struct {
	kind  error
	cause error
}

func (err classifiedError) Error() string   { return err.kind.Error() }
func (err classifiedError) Unwrap() []error { return []error{err.kind, err.cause} }
