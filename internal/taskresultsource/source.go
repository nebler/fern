package taskresultsource

import (
	"context"
	"errors"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskstore"
)

type Store interface {
	GetRetainedArtifact(context.Context, task.RetainedArtifactID) (taskstore.RetainedArtifact, error)
}

type Artifact interface {
	Inspect(context.Context, taskartifact.Locator) (taskartifact.Snapshot, error)
	Acquire(context.Context, taskartifact.Locator) (taskartifact.Snapshot, *taskartifact.Checkout, error)
}

type Resolver struct {
	store    Store
	artifact Artifact
}

func New(store Store, artifact Artifact) (*Resolver, error) {
	if store == nil || artifact == nil {
		return nil, errors.New("valid result source configuration is required")
	}
	return &Resolver{store: store, artifact: artifact}, nil
}

// Acquire returns a fresh repository and an idempotent mandatory cleanup.
func (r *Resolver) Acquire(ctx context.Context, result taskstore.Result) (string, func() error, error) {
	artifact, locator, err := r.load(ctx, result)
	if err != nil {
		return "", nil, err
	}
	snapshot, checkout, err := r.artifact.Acquire(ctx, locator)
	if err != nil {
		return "", nil, err
	}
	if err := verifyTuple(result, artifact, locator, snapshot); err != nil {
		if closeErr := checkout.Close(); closeErr != nil {
			return "", nil, errors.Join(err, closeErr)
		}
		return "", nil, err
	}
	path := checkout.Path()
	if path == "" {
		return "", nil, errors.Join(taskstore.ErrCorruptStore, checkout.Close())
	}
	return path, checkout.Close, nil
}

// Verify freshly proves that the retained artifact is present, intact, and
// bound to the complete durable result tuple.
func (r *Resolver) Verify(ctx context.Context, result taskstore.Result) error {
	artifact, locator, err := r.load(ctx, result)
	if err != nil {
		return err
	}
	snapshot, err := r.artifact.Inspect(ctx, locator)
	if err != nil {
		return err
	}
	return verifyTuple(result, artifact, locator, snapshot)
}

func (r *Resolver) load(ctx context.Context, result taskstore.Result) (taskstore.RetainedArtifact, taskartifact.Locator, error) {
	if result.SourceKind != taskstore.ResultSourceRetainedArtifact {
		return taskstore.RetainedArtifact{}, taskartifact.Locator{}, taskstore.ErrCorruptStore
	}
	artifact, err := r.store.GetRetainedArtifact(ctx, result.RetainedArtifactID)
	if err != nil {
		return taskstore.RetainedArtifact{}, taskartifact.Locator{}, err
	}
	locator, err := taskartifact.ParseLocator(artifact.CASLocator)
	return artifact, locator, err
}

func verifyTuple(result taskstore.Result, artifact taskstore.RetainedArtifact, locator taskartifact.Locator, snapshot taskartifact.Snapshot) error {
	if artifact.ID != result.RetainedArtifactID || artifact.ResultID != result.ID || artifact.ExportID != result.ArtifactExportID ||
		artifact.MaterializationID != result.MaterializationID ||
		artifact.WorkspaceID != result.WorkspaceID || artifact.TaskID != result.TaskID || artifact.AttemptID != result.AttemptID ||
		artifact.BaseSHA != result.BaseSHA || artifact.ResultCommit != result.ResultCommit || artifact.TreeOID != result.TreeOID ||
		artifact.OpenCodeSessionID != result.OpenCodeSessionID || artifact.OpenCodeMessageID != result.OpenCodeMessageID ||
		artifact.ChangesSHA256 != result.ManifestSHA256 || artifact.ManifestSHA256 != locator.Digest().Bytes() ||
		snapshot.RepositoryID != result.RepositoryID || snapshot.WorkspaceID != artifact.WorkspaceID || snapshot.TaskID != artifact.TaskID ||
		snapshot.ResultID != artifact.ResultID ||
		snapshot.Base != result.BaseSHA || snapshot.Result != result.ResultCommit || snapshot.Tree != result.TreeOID ||
		snapshot.OpenCodeSessionID != artifact.OpenCodeSessionID || snapshot.OpenCodeMessageID != artifact.OpenCodeMessageID ||
		snapshot.ChangesSHA256.Bytes() != result.ManifestSHA256 || snapshot.ManifestSHA256.Bytes() != artifact.ManifestSHA256 ||
		snapshot.BundleSHA256.Bytes() != artifact.BundleSHA256 || snapshot.BundleBytes != artifact.BundleBytes {
		return taskstore.ErrCorruptStore
	}
	return nil
}
