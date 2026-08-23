package taskresultcoord

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskresult"
	"github.com/nebler/fern/internal/taskstore"
)

type AuthorizationStore interface {
	GetSealPreview(context.Context, task.TaskID) (taskstore.SealPreview, error)
	RequestSeal(context.Context, taskstore.RequestSealParams) (taskstore.SealAdmission, error)
}

type SealSnapshot struct {
	WorkspaceRevision int64
	TaskRevision      int64
	AttemptRevision   int64
	TaskID            task.TaskID
	AttemptID         task.AttemptID
	RepositoryID      task.RepositoryID
	BaseSHA           task.GitOID
	ResultCommit      task.GitOID
	TreeOID           task.GitOID
	Outcome           task.ResultOutcome
	ManifestEntries   int
	ManifestSHA256    [sha256.Size]byte
	WorktreeClean     bool
}

type AuthorizerConfig struct {
	RepositoryPath   string
	PolicyVersion    string
	OperationTimeout time.Duration
}

type Authorizer struct {
	store     AuthorizationStore
	fencer    PauseFencer
	collector Collector
	config    AuthorizerConfig
}

type PauseFencer interface {
	AcquirePaused(context.Context) (func(), error)
}

func NewAuthorizer(store AuthorizationStore, fencer PauseFencer, collector Collector, config AuthorizerConfig) (*Authorizer, error) {
	if store == nil || fencer == nil || collector == nil ||
		len(config.RepositoryPath) < 1 || len(config.RepositoryPath) > MaxRepositoryPathBytes ||
		!filepath.IsAbs(config.RepositoryPath) || filepath.Clean(config.RepositoryPath) != config.RepositoryPath ||
		strings.ContainsAny(config.RepositoryPath, "\x00\r\n") || !validPolicyVersion(config.PolicyVersion) ||
		config.OperationTimeout <= 0 || config.OperationTimeout > MaxOperationTimeout {
		return nil, ErrInvalidConfig
	}
	return &Authorizer{store: store, fencer: fencer, collector: collector, config: config}, nil
}

func (a *Authorizer) Preview(ctx context.Context, taskID task.TaskID) (SealSnapshot, error) {
	operation, cancel := context.WithTimeout(ctx, a.config.OperationTimeout)
	defer cancel()
	var snapshot SealSnapshot
	err := a.withSnapshot(operation, taskID, func(value SealSnapshot) error {
		snapshot = value
		return nil
	})
	return snapshot, err
}

func (a *Authorizer) Request(ctx context.Context, expected SealSnapshot, params taskstore.RequestSealParams) (taskstore.SealAdmission, error) {
	if expected.TaskID != params.TaskID {
		return taskstore.SealAdmission{}, ErrSelectionChanged
	}
	operation, cancel := context.WithTimeout(ctx, a.config.OperationTimeout)
	defer cancel()
	var admission taskstore.SealAdmission
	err := a.withSnapshot(operation, expected.TaskID, func(actual SealSnapshot) error {
		if !sameExpectedSnapshot(actual, expected) {
			return ErrSelectionChanged
		}
		params.ExpectedWorkspaceRevision = actual.WorkspaceRevision
		params.ExpectedTaskRevision = actual.TaskRevision
		params.ExpectedAttemptRevision = actual.AttemptRevision
		params.RepositoryID = actual.RepositoryID
		params.BaseSHA = actual.BaseSHA
		params.ExpectedResultCommit = actual.ResultCommit
		params.ExpectedTreeOID = actual.TreeOID
		params.ExpectedOutcome = actual.Outcome
		params.ExpectedManifestEntries = actual.ManifestEntries
		params.ExpectedManifestSHA256 = actual.ManifestSHA256
		params.ExpectedWorktreeClean = actual.WorktreeClean
		var requestErr error
		admission, requestErr = a.store.RequestSeal(operation, params)
		return requestErr
	})
	return admission, err
}

func sameExpectedSnapshot(actual, expected SealSnapshot) bool {
	return actual.WorkspaceRevision == expected.WorkspaceRevision && actual.TaskRevision == expected.TaskRevision &&
		actual.AttemptRevision == expected.AttemptRevision && actual.TaskID == expected.TaskID && actual.AttemptID == expected.AttemptID &&
		actual.ResultCommit == expected.ResultCommit && actual.TreeOID == expected.TreeOID && actual.Outcome == expected.Outcome &&
		actual.ManifestEntries == expected.ManifestEntries && actual.ManifestSHA256 == expected.ManifestSHA256 &&
		actual.WorktreeClean == expected.WorktreeClean
}

func (a *Authorizer) withSnapshot(ctx context.Context, taskID task.TaskID, consume func(SealSnapshot) error) error {
	if consume == nil {
		return ErrInvalidConfig
	}
	selected, err := a.store.GetSealPreview(ctx, taskID)
	if err != nil {
		return err
	}
	release, err := a.fencer.AcquirePaused(ctx)
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
	current, err := a.store.GetSealPreview(ctx, taskID)
	if err != nil {
		return err
	}
	if !sameSealPreview(selected, current) {
		return ErrSelectionChanged
	}
	evidence := json.RawMessage(`{"authority":"user_seal_preview"}`)
	collected, err := a.collector.Collect(ctx, taskresult.Request{
		RepositoryPath:    a.config.RepositoryPath,
		Repository:        task.RepositoryTuple{RepositoryID: current.Task.RepositoryID, BaseSHA: current.Task.BaseSHA},
		OpenCodeSessionID: current.Attempt.OpenCodeSessionID, OpenCodeMessageID: current.Attempt.OpenCodeMessageID,
		EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), PolicyVersion: a.config.PolicyVersion,
	})
	if err != nil {
		return classifiedError{kind: ErrCollectionFailed, cause: err}
	}
	if !collected.Tuple.WorktreeClean || collected.Tuple.RepositoryID != current.Task.RepositoryID ||
		collected.Tuple.BaseSHA != current.Task.BaseSHA || collected.OpenCodeSessionID != current.Attempt.OpenCodeSessionID ||
		collected.OpenCodeMessageID != current.Attempt.OpenCodeMessageID {
		return ErrCollectionFailed
	}
	return consume(SealSnapshot{
		WorkspaceRevision: current.Workspace.Revision, TaskRevision: current.Task.Revision, AttemptRevision: current.Attempt.Revision,
		TaskID: current.Task.ID, AttemptID: current.Attempt.ID, RepositoryID: current.Task.RepositoryID, BaseSHA: current.Task.BaseSHA,
		ResultCommit: collected.Tuple.ResultCommit, TreeOID: collected.TreeOID, Outcome: collected.Tuple.Outcome,
		ManifestEntries: collected.Tuple.ManifestEntries, ManifestSHA256: collected.ManifestSHA256, WorktreeClean: true,
	})
}

func sameSealPreview(first, second taskstore.SealPreview) bool {
	return first.Workspace.ID == second.Workspace.ID && first.Workspace.Revision == second.Workspace.Revision &&
		first.Task.ID == second.Task.ID && first.Task.Revision == second.Task.Revision && first.Task.State == second.Task.State &&
		first.Task.CancelEpoch == second.Task.CancelEpoch && first.Task.CurrentAttemptID == second.Task.CurrentAttemptID &&
		first.Task.RepositoryID == second.Task.RepositoryID && first.Task.BaseSHA == second.Task.BaseSHA &&
		first.Attempt.ID == second.Attempt.ID && first.Attempt.Revision == second.Attempt.Revision && first.Attempt.State == second.Attempt.State &&
		first.Attempt.OpenCodeSessionID == second.Attempt.OpenCodeSessionID && first.Attempt.OpenCodeMessageID == second.Attempt.OpenCodeMessageID
}
