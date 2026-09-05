// Package taskresultsource resolves immutable result Git state without ever
// treating a disposable Background Run clone as post-result authority.
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
	Materialize(context.Context, taskartifact.Locator) (*taskartifact.Checkout, error)
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
	locator, err := r.verify(ctx, result)
	if err != nil {
		return "", nil, err
	}
	checkout, err := r.artifact.Materialize(ctx, locator)
	if err != nil {
		return "", nil, err
	}
	path := checkout.Path()
	if path == "" {
		_ = checkout.Close()
		return "", nil, taskstore.ErrCorruptStore
	}
	return path, checkout.Close, nil
}

// Verify freshly proves that the retained artifact is present, intact, and
// bound to the complete durable result tuple.
func (r *Resolver) Verify(ctx context.Context, result taskstore.Result) error {
	_, err := r.verify(ctx, result)
	return err
}

func (r *Resolver) verify(ctx context.Context, result taskstore.Result) (taskartifact.Locator, error) {
	if result.SourceKind != taskstore.ResultSourceRetainedArtifact {
		return taskartifact.Locator{}, taskstore.ErrCorruptStore
	}
	artifact, err := r.store.GetRetainedArtifact(ctx, result.RetainedArtifactID)
	if err != nil {
		return taskartifact.Locator{}, err
	}
	locator, err := taskartifact.ParseLocator(artifact.CASLocator)
	if err != nil {
		return taskartifact.Locator{}, err
	}
	snapshot, err := r.artifact.Inspect(ctx, locator)
	if err != nil {
		return taskartifact.Locator{}, err
	}
	if artifact.ID != result.RetainedArtifactID || artifact.ResultID != result.ID || artifact.ExportID != result.ArtifactExportID ||
		artifact.MaterializationID != result.MaterializationID ||
		artifact.WorkspaceID != result.WorkspaceID || artifact.TaskID != result.TaskID || artifact.AttemptID != result.AttemptID ||
		artifact.BaseSHA != result.BaseSHA || artifact.ResultCommit != result.ResultCommit || artifact.TreeOID != result.TreeOID ||
		artifact.OpenCodeSessionID != result.OpenCodeSessionID || artifact.OpenCodeMessageID != result.OpenCodeMessageID ||
		artifact.ChangesSHA256 != result.ManifestSHA256 || artifact.ManifestSHA256 != locator.Digest().Bytes() ||
		snapshot.RepositoryID != result.RepositoryID || snapshot.WorkspaceID != artifact.WorkspaceID || snapshot.TaskID != artifact.TaskID ||
		snapshot.AttemptID != artifact.AttemptID || snapshot.Generation != artifact.Generation || snapshot.SealRequestID != artifact.SealRequestID ||
		snapshot.Base != result.BaseSHA || snapshot.Result != result.ResultCommit || snapshot.Tree != result.TreeOID ||
		snapshot.OpenCodeSessionID != artifact.OpenCodeSessionID || snapshot.OpenCodeMessageID != artifact.OpenCodeMessageID ||
		snapshot.ChangesSHA256.Bytes() != result.ManifestSHA256 || snapshot.ManifestSHA256.Bytes() != artifact.ManifestSHA256 ||
		snapshot.BundleSHA256.Bytes() != artifact.BundleSHA256 || snapshot.BundleBytes != artifact.BundleBytes {
		return taskartifact.Locator{}, taskstore.ErrCorruptStore
	}
	return locator, nil
}
