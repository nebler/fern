package artifact

import (
	"context"
	"errors"

	"github.com/nebler/fern/internal/store"
)

type ArtifactReader interface {
	Inspect(context.Context, Locator) (Snapshot, error)
	Acquire(context.Context, Locator) (Snapshot, *Checkout, error)
}

type Resolver struct {
	artifact ArtifactReader
}

func NewResolver(engine ArtifactReader) (*Resolver, error) {
	if engine == nil {
		return nil, errors.New("valid result source configuration is required")
	}
	return &Resolver{artifact: engine}, nil
}

// Acquire returns a fresh repository and an idempotent mandatory cleanup.
func (r *Resolver) Acquire(ctx context.Context, projection store.RunResult) (string, func() error, error) {
	locator, err := ParseLocator(projection.Result.CASLocator())
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
func (r *Resolver) Verify(ctx context.Context, projection store.RunResult) error {
	locator, err := ParseLocator(projection.Result.CASLocator())
	if err != nil {
		return err
	}
	snapshot, err := r.artifact.Inspect(ctx, locator)
	if err != nil {
		return err
	}
	return verifyTuple(projection, snapshot)
}

func verifyTuple(projection store.RunResult, snapshot Snapshot) error {
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
