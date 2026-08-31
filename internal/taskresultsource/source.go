// Package taskresultsource resolves immutable result Git state without ever
// treating a disposable Background Run clone as post-result authority.
package taskresultsource

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

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
	store      Store
	artifact   Artifact
	persistent string
}

func New(store Store, artifact Artifact, persistent string) (*Resolver, error) {
	if store == nil || artifact == nil || !filepath.IsAbs(persistent) || filepath.Clean(persistent) != persistent || strings.IndexByte(persistent, 0) >= 0 {
		return nil, errors.New("valid result source configuration is required")
	}
	return &Resolver{store: store, artifact: artifact, persistent: persistent}, nil
}

// Acquire returns a fresh repository and an idempotent mandatory cleanup.
func (r *Resolver) Acquire(ctx context.Context, result taskstore.Result) (string, func() error, error) {
	if result.SourceKind == taskstore.ResultSourcePersistentWorkspace {
		return r.persistent, func() error { return nil }, nil
	}
	if result.SourceKind != taskstore.ResultSourceRetainedArtifact {
		return "", nil, taskstore.ErrCorruptStore
	}
	artifact, err := r.store.GetRetainedArtifact(ctx, result.RetainedArtifactID)
	if err != nil {
		return "", nil, err
	}
	locator, err := taskartifact.ParseLocator(artifact.CASLocator)
	if err != nil {
		return "", nil, err
	}
	snapshot, err := r.artifact.Inspect(ctx, locator)
	if err != nil {
		return "", nil, err
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
		return "", nil, taskstore.ErrCorruptStore
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
