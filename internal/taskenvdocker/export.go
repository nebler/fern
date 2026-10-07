package taskenvdocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/docker/docker/errdefs"
	"github.com/nebler/fern/internal/taskstore"
)

// ExportSource is an opaque lease on one exact Background Run clone. The
// repository path remains protected by the provider's exclusive clone lock
// until Close. ExportSource must not be copied after first use.
type ExportSource struct {
	noCopy noCopy

	path      string
	unlock    func() error
	closeOnce sync.Once
	closeErr  error
}

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// RepositoryPath returns only the exact clone path authorized for export.
func (source *ExportSource) RepositoryPath() string { return source.path }

// Close releases the exact clone authority. It is safe to call more than once.
func (source *ExportSource) Close() error {
	if source == nil {
		return nil
	}
	source.closeOnce.Do(func() {
		if source.unlock != nil {
			source.closeErr = source.unlock()
		}
	})
	return source.closeErr
}

// AcquireExportSource acquires a filesystem-only export lease after proving
// that the supplied exact writer fence remains inactive. It performs no Git
// reads and grants no provider-root or cleanup authority.
func (p *Provider) AcquireExportSource(ctx context.Context, run taskstore.BackgroundRun, fence WriterFence) (_ *ExportSource, resultErr error) {
	if err := p.requireOpen(); err != nil {
		return nil, err
	}
	digest, err := p.validateRun(run)
	if err != nil {
		return nil, err
	}
	kind, err := validateCleanupAuthority(fence)
	if err != nil {
		return nil, err
	}
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, exportIdentityError(run, "private clone lock is unavailable")
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, unlock())
		}
	}()

	marker, err := p.readExportClone(run, digest)
	if err != nil {
		return nil, exportIdentityError(run, "private clone authority is not exact")
	}
	if err := p.requireExportWriterInactive(ctx, run, digest, kind, fence); err != nil {
		return nil, err
	}

	// This is intentionally the last filesystem operation before publishing the
	// lease: the marker and canonical clone path must still name the exact
	// clone observed before the Docker inactivity proof.
	if current, err := p.readExportClone(run, digest); err != nil || current != marker {
		return nil, exportIdentityError(run, "clone authority changed during export acquisition")
	}

	p.lifecycle.mu.Lock()
	if p.lifecycle.closed {
		p.lifecycle.mu.Unlock()
		return nil, ErrProviderClosed
	}
	source := &ExportSource{path: filepath.Join(p.root, run.CloneIdentity), unlock: unlock}
	p.lifecycle.mu.Unlock()
	return source, nil
}

func (p *Provider) requireOpen() error {
	p.lifecycle.mu.Lock()
	defer p.lifecycle.mu.Unlock()
	if p.lifecycle.closed {
		return ErrProviderClosed
	}
	return nil
}

func (p *Provider) readExportClone(run taskstore.BackgroundRun, digest string) (cloneMarker, error) {
	marker, err := p.readCloneMarker(run, digest)
	if err != nil {
		return cloneMarker{}, err
	}
	info, err := os.Lstat(filepath.Join(p.root, run.CloneIdentity))
	if err != nil || !info.IsDir() || !sameCloneIdentity(info, marker) {
		return cloneMarker{}, errors.New("clone is not the exact directory its private authority names")
	}
	return marker, nil
}

func (p *Provider) requireExportWriterInactive(ctx context.Context, run taskstore.BackgroundRun, digest string, kind WriterFenceKind, fence WriterFence) error {
	operation, cancel := context.WithTimeout(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		var identity *IdentityError
		if err := p.requireNoStrayContainer(operation, run, digest, fence.ContainerID()); errors.As(err, &identity) {
			return exportIdentityError(run, identity.Reason)
		} else if err != nil {
			return exportDockerError(ctx)
		}
		return nil
	}
	if err != nil {
		return exportDockerError(ctx)
	}
	if kind == WriterFenceNeverCreated || info.ID != fence.ContainerID() {
		return exportIdentityError(run, "container does not match the writer fence")
	}
	if info.State == nil || info.State.Running || info.State.Paused || info.State.Restarting {
		return exportIdentityError(run, "writer is active")
	}
	if err := p.attestContainer(run, digest, info, false); err != nil {
		return exportIdentityError(run, "container attestation differs from the writer fence")
	}
	if kind == WriterFenceCreatedNeverStarted {
		if info.State.Status != "created" || info.State.StartedAt != "" {
			return exportIdentityError(run, "created-container fence has a process epoch")
		}
	} else {
		if info.State.Status == "created" {
			return exportIdentityError(run, "runtime fence names a never-started container")
		}
		if err := requireRuntime(info, fence.runtimeIdentity()); err != nil {
			return exportIdentityError(run, "container process epoch differs from the writer fence")
		}
	}
	listed, err := p.listRunContainers(operation, run, digest)
	if err != nil {
		return exportDockerError(ctx)
	}
	if len(listed) != 1 || listed[0].ID != info.ID {
		return exportIdentityError(run, "exact-labeled container set differs from the writer fence")
	}
	return nil
}

func exportIdentityError(run taskstore.BackgroundRun, reason string) error {
	return &IdentityError{Resource: "export source", Identity: run.CloneIdentity, Reason: reason}
}

func exportDockerError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("Docker state is unavailable while proving export writer inactivity")
}
