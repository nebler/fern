package taskresultsource

import (
	"context"
	"errors"

	"github.com/nebler/fern/internal/store"
	"github.com/nebler/fern/internal/taskartifact"
)

type Artifact interface {
	Inspect(context.Context, taskartifact.Locator) (taskartifact.Snapshot, error)
	Acquire(context.Context, taskartifact.Locator) (taskartifact.Snapshot, *taskartifact.Checkout, error)
}

type Resolver struct {
	artifact Artifact
}

func New(artifact Artifact) (*Resolver, error) {
	if artifact == nil {
		return nil, errors.New("valid result source configuration is required")
	}
	return &Resolver{artifact: artifact}, nil
}

// Acquire returns a fresh repository and an idempotent mandatory cleanup.
func (r *Resolver) Acquire(ctx context.Context, projection store.BackgroundRunResultProjection) (string, func() error, error) {
	locator, err := taskartifact.ParseLocator(projection.Result.CASLocator())
	if err != nil {
		return "", nil, err
	}
	snapshot, checkout, err := r.artifact.Acquire(ctx, locator)
	if err != nil {
		return "", nil, err
	}
	if err := verifyTuple(projection, snapshot); err != nil {
		if closeErr := checkout.Close(); closeErr != nil {
			return "", nil, errors.Join(err, closeErr)
		}
		return "", nil, err
	}
	path := checkout.Path()
	if path == "" {
		return "", nil, errors.Join(store.ErrCorruptStore, checkout.Close())
	}
	return path, checkout.Close, nil
}

// Verify freshly proves that the retained artifact is present, intact, and
// bound to the complete durable run and result.
func (r *Resolver) Verify(ctx context.Context, projection store.BackgroundRunResultProjection) error {
	locator, err := taskartifact.ParseLocator(projection.Result.CASLocator())
	if err != nil {
		return err
	}
	snapshot, err := r.artifact.Inspect(ctx, locator)
	if err != nil {
		return err
	}
	return verifyTuple(projection, snapshot)
}

func verifyTuple(projection store.BackgroundRunResultProjection, snapshot taskartifact.Snapshot) error {
	run, result := projection.Run, projection.Result
	if result.State != store.ResultSealed || result.RunID != run.RunID || run.Seal == nil || run.Seal.ResultID != result.ID ||
		snapshot.RepositoryID != run.RepositoryID || snapshot.WorkspaceID != run.WorkspaceID || snapshot.RunID != run.RunID ||
		snapshot.ResultID != result.ID || snapshot.OpenCodeSessionID != run.OpenCodeSessionID || snapshot.OpenCodeMessageID != run.OpenCodeMessageID ||
		snapshot.Base != result.BaseSHA || snapshot.Base != run.BaseOID || snapshot.Result != result.ResultCommit || snapshot.Tree != result.TreeOID ||
		snapshot.ChangesSHA256.Bytes() != result.ChangesSHA256 || len(snapshot.Changes) != result.ChangeCount ||
		snapshot.ManifestSHA256.Bytes() != result.ManifestSHA256 ||
		snapshot.BundleSHA256.Bytes() != result.BundleSHA256 || snapshot.BundleBytes != result.BundleBytes {
		return store.ErrCorruptStore
	}
	return nil
}
