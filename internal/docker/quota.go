package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/volume"
	"github.com/nebler/fern/internal/safeio"
	"github.com/nebler/fern/internal/store"
)

func (p *Provider) attestExecutionTree(ctx context.Context, path string) error {
	if p.config.quotaCheck != nil {
		return p.attestQuotaChild(path)
	}
	want, err := p.checkQuota(p.root)
	if err != nil {
		return err
	}
	return inspectProjectTree(ctx, path, want)
}

type quotaIdentity struct {
	Device         uint64
	Project        uint32
	Blocks, Inodes uint64
}

func validateQuotaKernelState(project, xflags uint32, version byte, flags uint16, quotaVersion, quotaFlags byte, quotaProject uint32, blocks, inodes uint64) error {
	if xflags&0x101 != 0 {
		return errors.New("XFS realtime storage is not covered by the project data-byte quota")
	}
	if project == 0 || xflags&0x200 == 0 {
		return errors.New("project directory requires a nonzero project ID and PROJINHERIT")
	}
	if version != 1 || flags&0x30 != 0x30 {
		return errors.New("XFS project quota accounting and enforcement must both be active")
	}
	if quotaVersion != 1 || quotaFlags != 2 || quotaProject != project {
		return errors.New("XFS returned a different quota identity")
	}
	if blocks == 0 || inodes == 0 {
		return errors.New("XFS project byte and inode hard limits must both be finite and nonzero")
	}
	return nil
}

func (p *Provider) removeVolumeBacking(run store.Run) error {
	parent := filepath.Dir(p.volumeBackingPath(run))
	info, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("quota volume parent is not an exact private directory")
	}
	return os.RemoveAll(p.volumeBackingPath(run))
}

func (p *Provider) checkQuota(path string) (quotaIdentity, error) {
	if p.config.quotaCheck != nil {
		return p.config.quotaCheck(path)
	}
	return inspectProjectQuota(path)
}

func (p *Provider) admitStorage() error {
	if p.config.quotaCheck == nil {
		daemon, ok := p.docker.(interface{ DaemonHost() string })
		if !ok || !strings.HasPrefix(daemon.DaemonHost(), "unix://") {
			return errors.New("quota-backed execution requires the same-host Linux Docker daemon over a local Unix socket")
		}
	}
	root := p.config.RuntimeStorageRoot
	if root == "" {
		if p.config.quotaCheck == nil {
			return errors.New("execution requires runtimeStorageRoot on Linux XFS with enforced project byte and inode hard quotas")
		}
		root = p.config.StateRoot
	}
	want, err := p.checkQuota(root)
	if err != nil {
		return fmt.Errorf("runtime storage admission: %w", err)
	}
	if p.config.quotaCheck == nil {
		stateInfo, err := os.Stat(p.config.StateRoot)
		if err != nil {
			return fmt.Errorf("inspect durable state filesystem: %w", err)
		}
		stateDevice, _, err := safeio.Identity(stateInfo)
		if err != nil {
			return err
		}
		if stateDevice == want.Device {
			return errors.New("runtime quota storage must be on a different filesystem from durable StateRoot")
		}
	}
	got, err := p.checkQuota(p.root)
	if err != nil {
		return fmt.Errorf("clone storage admission: %w", err)
	}
	if got != want {
		return errors.New("clone root does not inherit the runtime storage project quota")
	}
	return nil
}

func (p *Provider) volumeBackingPath(run store.Run) string {
	return filepath.Join(p.root, "opencode-volumes", run.VolumeIdentity)
}

func (p *Provider) volumeOptions(run store.Run) map[string]string {
	if p.config.RuntimeStorageRoot == "" && p.config.quotaCheck != nil {
		return nil
	}
	return map[string]string{"type": "none", "o": "bind", "device": p.volumeBackingPath(run)}
}

func (p *Provider) attestExecutionVolume(run store.Run, item volume.Volume) error {
	if !maps.Equal(item.Options, p.volumeOptions(run)) {
		return errors.New("execution rejects legacy or non-quota-backed Docker volumes")
	}
	if len(item.Options) == 0 {
		return nil
	}
	return p.attestQuotaChild(p.volumeBackingPath(run))
}

func (p *Provider) prepareVolumeBacking(run store.Run) error {
	if len(p.volumeOptions(run)) == 0 {
		return nil
	}
	parent := filepath.Dir(p.volumeBackingPath(run))
	if err := os.Mkdir(parent, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("quota volume parent must be a private directory")
	}
	if err := p.attestQuotaChild(parent); err != nil {
		return err
	}
	path := p.volumeBackingPath(run)
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := p.attestQuotaChild(path); err != nil {
		return err
	}
	// The parent remains server-owned and inaccessible to the worker. Docker
	// bind-mounts only the leaf; the worker cannot replace that leaf.
	// The server need not own CAP_CHOWN. Host users cannot traverse the 0700
	// parent; the worker gets access only through its dedicated leaf mount.
	return os.Chmod(path, 0777)
}

func (p *Provider) attestQuotaChild(path string) error {
	want, err := p.checkQuota(p.root)
	if err != nil {
		return err
	}
	got, err := p.checkQuota(path)
	if err != nil {
		return err
	}
	if got != want {
		return errors.New("storage directory does not inherit the runtime project quota")
	}
	return nil
}
