package taskenvdocker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nebler/fern/internal/safeio"
	"github.com/nebler/fern/internal/taskstore"
)

// RemoveClone removes only an exactly attested clone after the exact runtime is absent.
func (p *Provider) RemoveClone(ctx context.Context, run taskstore.BackgroundRun, authority WriterFence) (_ Observation, resultErr error) {
	digest, err := p.validateRunForCleanup(run)
	if err != nil {
		return Observation{}, err
	}
	if _, err := validateCleanupAuthority(authority); err != nil {
		return Observation{}, err
	}
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		return Observation{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	if err := p.requireContainerAbsent(ctx, run, digest, authority.ContainerID()); err != nil {
		return Observation{}, err
	}
	if err := p.requireVolumeAbsent(ctx, run, digest); err != nil {
		return Observation{}, err
	}
	removed := func(status string) (Observation, error) {
		e, _ := makeEvidence(evidence{Effect: "clone_remove", Identity: run.CloneIdentity, Spec: digest, Status: status})
		return Observation{Evidence: e}, nil
	}
	path := filepath.Join(p.root, run.CloneIdentity)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(p.cloneMarkerPath(run)); errors.Is(err, os.ErrNotExist) {
			return removed("absent")
		} else if err != nil {
			return Observation{}, err
		}
		recovered, err := p.finishMarkerBoundDeletion(ctx, run, digest)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Observation{}, err
		} else if err != nil {
			return Observation{}, &IdentityError{Resource: "clone marker", Identity: run.CloneIdentity, Reason: err.Error()}
		}
		if recovered {
			return removed("recovered")
		}
		return removed("absent")
	}
	if err != nil {
		return Observation{}, err
	}
	if err := p.attestCloneDeletion(run, digest, path); err != nil {
		return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}
	}
	device, inode, err := safeio.Identity(info)
	if err != nil {
		return Observation{}, err
	}
	quarantine := filepath.Join(p.root, ".clone-quarantine-"+rand.Text())
	if err := safeio.QuarantineRemove(path, quarantine, device, inode); errors.Is(err, safeio.ErrChanged) {
		return Observation{}, &IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "renamed clone is not the attested inode"}
	} else if err != nil {
		return Observation{}, fmt.Errorf("quarantine and remove clone: %w", err)
	}
	if _, err := p.finishMarkerBoundDeletion(ctx, run, digest); err != nil {
		return Observation{}, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return Observation{}, errors.New("canonical clone path was replaced during removal")
	}
	return removed("removed")
}

// finishMarkerBoundDeletion removes the one recoverable location still holding
// the marker's clone inode, if any, and then the marker itself.
func (p *Provider) finishMarkerBoundDeletion(ctx context.Context, run taskstore.BackgroundRun, digest string) (bool, error) {
	marker, err := p.readCloneMarker(run, digest)
	if err != nil {
		return false, err
	}
	locations, unknown, err := p.findRecoverableClones(ctx, marker)
	if err != nil {
		return false, err
	}
	if unknown || len(locations) > 1 {
		return false, errors.New("marker-bound clone inode has unknown or multiple recovery locations")
	}
	recovered := len(locations) == 1
	if recovered {
		location := locations[0]
		if err := p.attestCloneMarker(run, digest, location.path); err != nil {
			return false, err
		}
		if err := safeio.RemoveTree(filepath.Dir(location.path), filepath.Base(location.path)); err != nil {
			return false, err
		}
		if location.kind == cloneRecoveryStage {
			if err := os.Remove(location.parent); err != nil {
				return false, fmt.Errorf("remove recovered staging parent: %w", err)
			}
		}
		locations, unknown, err = p.findRecoverableClones(ctx, marker)
		if err != nil {
			return false, err
		}
	}
	if unknown || len(locations) != 0 {
		return false, errors.New("marker-bound clone inode remains after deletion")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := p.removeCloneMarker(run, digest, marker); err != nil {
		return false, err
	}
	return recovered, nil
}

// removeCreatedTree rolls back a stage this call created, proving by inode
// that it removes exactly that directory. A missing stage is already gone.
func removeCreatedTree(root, path string, created os.FileInfo) error {
	device, inode, err := safeio.Identity(created)
	if err != nil {
		return err
	}
	err = safeio.QuarantineRemove(path, filepath.Join(root, ".clone-quarantine-"+rand.Text()), device, inode)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (p *Provider) attestCloneDeletion(run taskstore.BackgroundRun, digest, path string) error {
	if err := p.attestCloneMarker(run, digest, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("clone deletion target is not an exact directory")
	}
	return nil
}

func (p *Provider) acquireCloneAuthority(ctx context.Context, run taskstore.BackgroundRun, digest string) (func() error, error) {
	unlock, present, err := p.acquireCloneAuthorityIfPresent(ctx, run, digest)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errors.Join(&IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: "private clone authority is absent"}, unlock())
	}
	return unlock, nil
}

func (p *Provider) acquireCloneAuthorityIfPresent(ctx context.Context, run taskstore.BackgroundRun, digest string) (func() error, bool, error) {
	unlock, err := p.acquireCloneLock(ctx, run.CloneIdentity)
	if err != nil {
		return nil, false, err
	}
	if _, err := os.Lstat(p.cloneMarkerPath(run)); errors.Is(err, os.ErrNotExist) {
		return unlock, false, nil
	} else if err != nil {
		return nil, false, errors.Join(err, unlock())
	}
	if err := p.attestCloneDeletion(run, digest, filepath.Join(p.root, run.CloneIdentity)); err != nil {
		return nil, false, errors.Join(&IdentityError{Resource: "clone", Identity: run.CloneIdentity, Reason: err.Error()}, unlock())
	}
	return unlock, true, nil
}
