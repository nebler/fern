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

	path          string
	cloneIdentity string
	device        uint64
	inode         uint64
	unlock        func() error
	closeOnce     sync.Once
	closeErr      error
}

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// RepositoryPath returns only the exact clone path authorized for export.
func (source *ExportSource) RepositoryPath() string { return source.path }

// CloneIdentity returns the immutable provider clone identity.
func (source *ExportSource) CloneIdentity() string { return source.cloneIdentity }

// Device returns the clone directory's stable Unix device identity.
func (source *ExportSource) Device() uint64 { return source.device }

// Inode returns the clone directory's stable Unix inode identity.
func (source *ExportSource) Inode() uint64 { return source.inode }

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
	if _, err := validateCleanupAuthority(fence); err != nil {
		return nil, err
	}
	if err := p.attestExportRoot(); err != nil {
		return nil, exportIdentityError(run, "private provider root changed")
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

	marker, info, device, inode, err := p.readExportClone(run, digest)
	if err != nil {
		return nil, exportIdentityError(run, "private clone authority is not exact")
	}
	if err := p.requireExportWriterInactive(ctx, run, digest, fence); err != nil {
		return nil, err
	}

	// This is intentionally the last filesystem operation before publishing the
	// lease: both authority files and the canonical clone path must still name
	// the exact objects observed before the Docker inactivity proof.
	if err := p.attestExportRoot(); err != nil {
		return nil, exportIdentityError(run, "private provider root changed")
	}
	current, finalInfo, finalDevice, finalInode, err := p.readExportClone(run, digest)
	if err != nil || current != marker || !os.SameFile(info, finalInfo) || finalDevice != device || finalInode != inode {
		return nil, exportIdentityError(run, "clone authority changed during export acquisition")
	}

	p.lifecycle.mu.Lock()
	if p.lifecycle.closed {
		p.lifecycle.mu.Unlock()
		return nil, ErrProviderClosed
	}
	source := &ExportSource{path: filepath.Join(p.root, run.CloneIdentity), cloneIdentity: run.CloneIdentity, device: device, inode: inode, unlock: unlock}
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

func (p *Provider) attestExportRoot() error {
	info, err := os.Lstat(p.root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("private provider root is unsafe")
	}
	device, inode, err := fileIdentity(info)
	if err != nil || device != p.rootDevice || inode != p.rootInode {
		return errors.New("private provider root identity changed")
	}
	resolved, err := filepath.EvalSymlinks(p.root)
	if err != nil || resolved != p.root {
		return errors.New("private provider root path changed")
	}
	return nil
}

func (p *Provider) readExportClone(run taskstore.BackgroundRun, digest string) (cloneMarkerSnapshot, os.FileInfo, uint64, uint64, error) {
	marker, err := p.readCloneMarkerSnapshot(run, digest)
	if err != nil {
		return cloneMarkerSnapshot{}, nil, 0, 0, err
	}
	path := filepath.Join(p.root, run.CloneIdentity)
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return cloneMarkerSnapshot{}, nil, 0, 0, errors.New("clone is not an exact directory")
	}
	device, inode, err := fileIdentity(info)
	if err != nil || marker.marker.Device != device || marker.marker.Inode != inode {
		return cloneMarkerSnapshot{}, nil, 0, 0, errors.New("clone inode differs from private authority")
	}
	return marker, info, device, inode, nil
}

func (p *Provider) requireExportWriterInactive(ctx context.Context, run taskstore.BackgroundRun, digest string, fence WriterFence) error {
	kind, err := validateCleanupAuthority(fence)
	if err != nil {
		return err
	}
	operation, cancel := operationContext(ctx, p.config.DockerTimeout)
	defer cancel()
	info, err := p.docker.ContainerInspect(operation, run.ContainerIdentity)
	if errdefs.IsNotFound(err) {
		if fence.ContainerID != "" {
			if _, idErr := p.docker.ContainerInspect(operation, fence.ContainerID); idErr == nil {
				return exportIdentityError(run, "fenced container exists under another name")
			} else if !errdefs.IsNotFound(idErr) {
				return exportDockerError(ctx)
			}
		}
		listed, listErr := p.listRunContainers(operation, run, digest)
		if listErr != nil {
			return exportDockerError(ctx)
		}
		if len(listed) != 0 {
			return exportIdentityError(run, "an exact-labeled replacement container exists")
		}
		return nil
	}
	if err != nil {
		return exportDockerError(ctx)
	}
	if kind == cleanupNeverCreated || info.ID != fence.ContainerID {
		return exportIdentityError(run, "container does not match the writer fence")
	}
	if info.State == nil || info.State.Running || info.State.Paused || info.State.Restarting {
		return exportIdentityError(run, "writer is active")
	}
	if err := p.attestContainerForCleanup(run, digest, info, false); err != nil {
		return exportIdentityError(run, "container attestation differs from the writer fence")
	}
	if kind == cleanupCreated {
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
