package runtime

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

const (
	backupVolumeTarget = "/fern-backup-volume"
	backupStageLabel   = "dev.fern.backup-stage"
)

type volumeSnapshot struct {
	existed bool
	path    string
}

// ManagedVolumeNames returns the canonical durable volumes expected by spec.
func ManagedVolumeNames(spec Spec) []string {
	return append([]string(nil), specVolumeNames(spec)...)
}

// ExportManagedVolumes copies every expected Fern-owned volume into a directory
// named after the volume. It never starts a helper or removes a durable volume.
func (d *Docker) ExportManagedVolumes(ctx context.Context, spec Spec, destination string) ([]string, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := requireEmptyDirectory(destination); err != nil {
		return nil, err
	}
	names := ManagedVolumeNames(spec)
	for _, name := range names {
		if _, err := d.inspectManagedVolume(ctx, spec.Name, name); err != nil {
			return nil, err
		}
		path := filepath.Join(destination, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, fmt.Errorf("create volume export %q: %w", name, err)
		}
		if err := d.copyVolumeOut(ctx, spec.Image, name, path); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// RestoreManagedVolumes verifies sources through temporary Docker volumes before
// replacing the canonical volumes. Docker cannot rename volumes, so activation
// is sequential; failures restore host-side snapshots on a best-effort basis.
func (d *Docker) RestoreManagedVolumes(ctx context.Context, spec Spec, sources map[string]string, generation string) (resultErr error) {
	if err := spec.Validate(); err != nil {
		return err
	}
	names := ManagedVolumeNames(spec)
	if len(sources) != len(names) {
		return errors.New("restore volume set does not match workspace configuration")
	}
	for _, name := range names {
		if sources[name] == "" {
			return fmt.Errorf("restore volume set is missing %q", name)
		}
	}

	work, err := os.MkdirTemp("", ".fern-volume-restore-")
	if err != nil {
		return fmt.Errorf("create volume restore staging: %w", err)
	}
	if err := os.Chmod(work, 0o700); err != nil {
		_ = os.RemoveAll(work)
		return err
	}
	defer os.RemoveAll(work)

	prior := make(map[string]volumeSnapshot, len(names))
	verified := make(map[string]string, len(names))
	var stages []string
	defer func() {
		cleanupCtx, cancel := detachedContext(ctx, cleanupTimeout)
		defer cancel()
		for _, name := range stages {
			if err := d.cli.VolumeRemove(cleanupCtx, name, false); err != nil && !errdefs.IsNotFound(err) {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove restore staging volume %q: %w", name, err))
			}
		}
	}()

	for _, name := range names {
		_, inspectErr := d.inspectManagedVolume(ctx, spec.Name, name)
		snapshot := filepath.Join(work, "prior", name)
		switch {
		case inspectErr == nil:
			if err := os.MkdirAll(snapshot, 0o700); err != nil {
				return err
			}
			if err := d.copyVolumeOut(ctx, spec.Image, name, snapshot); err != nil {
				return err
			}
			prior[name] = volumeSnapshot{existed: true, path: snapshot}
		case errdefs.IsNotFound(inspectErr):
			prior[name] = volumeSnapshot{}
		default:
			return inspectErr
		}

		stage := restoreStageVolumeName(spec.Name, name, generation)
		if _, err := d.cli.VolumeInspect(ctx, stage); err == nil {
			return fmt.Errorf("restore staging volume already exists: %s", stage)
		} else if !errdefs.IsNotFound(err) {
			return fmt.Errorf("inspect restore staging volume %q: %w", stage, err)
		}
		created, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: stage, Labels: map[string]string{
			managedLabel: labelTrue, workspaceLabel: spec.Name, backupStageLabel: generation,
		}})
		if err != nil {
			return fmt.Errorf("create restore staging volume %q: %w", stage, err)
		}
		if created.Name != stage || created.Labels[managedLabel] != labelTrue || created.Labels[workspaceLabel] != spec.Name || created.Labels[backupStageLabel] != generation {
			return fmt.Errorf("restore staging volume %q failed ownership attestation", stage)
		}
		stages = append(stages, stage)
		if err := d.copyVolumeIn(ctx, spec.Image, stage, sources[name]); err != nil {
			return err
		}
		verification := filepath.Join(work, "verified", name)
		if err := os.MkdirAll(verification, 0o700); err != nil {
			return err
		}
		if err := d.copyVolumeOut(ctx, spec.Image, stage, verification); err != nil {
			return err
		}
		if err := compareTrees(sources[name], verification); err != nil {
			return fmt.Errorf("verify restore staging volume %q: %w", name, err)
		}
		verified[name] = verification
	}

	var activated []string
	for _, name := range names {
		if prior[name].existed {
			if err := d.cli.VolumeRemove(ctx, name, false); err != nil {
				return d.rollbackVolumes(ctx, spec, activated, prior, fmt.Errorf("remove current volume %q: %w", name, err))
			}
		}
		activated = append(activated, name)
		if _, err := d.ensureVolume(ctx, spec.Name, name); err != nil {
			return d.rollbackVolumes(ctx, spec, activated, prior, err)
		}
		if err := d.copyVolumeIn(ctx, spec.Image, name, verified[name]); err != nil {
			return d.rollbackVolumes(ctx, spec, activated, prior, err)
		}
		check := filepath.Join(work, "activated", name)
		if err := os.MkdirAll(check, 0o700); err != nil {
			return d.rollbackVolumes(ctx, spec, activated, prior, err)
		}
		if err := d.copyVolumeOut(ctx, spec.Image, name, check); err != nil {
			return d.rollbackVolumes(ctx, spec, activated, prior, err)
		}
		if err := compareTrees(verified[name], check); err != nil {
			return d.rollbackVolumes(ctx, spec, activated, prior, fmt.Errorf("verify activated volume %q: %w", name, err))
		}
	}
	return nil
}

func (d *Docker) rollbackVolumes(ctx context.Context, spec Spec, names []string, prior map[string]volumeSnapshot, cause error) error {
	rollbackCtx, cancel := detachedContext(ctx, cleanupTimeout)
	defer cancel()
	for index := len(names) - 1; index >= 0; index-- {
		name := names[index]
		err := d.cli.VolumeRemove(rollbackCtx, name, false)
		if err != nil && !errdefs.IsNotFound(err) {
			cause = errors.Join(cause, fmt.Errorf("rollback remove volume %q: %w", name, err))
			continue
		}
		if !prior[name].existed {
			continue
		}
		if _, err := d.ensureVolume(rollbackCtx, spec.Name, name); err != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback create volume %q: %w", name, err))
			continue
		}
		if err := d.copyVolumeIn(rollbackCtx, spec.Image, name, prior[name].path); err != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback populate volume %q: %w", name, err))
		}
	}
	return cause
}

func (d *Docker) inspectManagedVolume(ctx context.Context, workspace, name string) (volume.Volume, error) {
	inspection, err := d.cli.VolumeInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return volume.Volume{}, err
		}
		return volume.Volume{}, fmt.Errorf("inspect managed volume %q: %w", name, err)
	}
	if inspection.Labels[managedLabel] != labelTrue || inspection.Labels[workspaceLabel] != workspace {
		return volume.Volume{}, fmt.Errorf("%w: volume %q", ErrUnmanaged, name)
	}
	return inspection, nil
}

func (d *Docker) copyVolumeOut(ctx context.Context, image, name, destination string) (resultErr error) {
	created, err := d.cli.ContainerCreate(ctx, &container.Config{Image: image}, &container.HostConfig{Mounts: []mount.Mount{{
		Type: mount.TypeVolume, Source: name, Target: backupVolumeTarget, ReadOnly: true,
	}}}, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return fmt.Errorf("create volume export helper for %q: %w", name, err)
	}
	defer func() {
		cleanupCtx, cancel := detachedContext(ctx, cleanupTimeout)
		defer cancel()
		if err := d.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove volume export helper for %q: %w", name, err))
		}
	}()
	stream, _, err := d.cli.CopyFromContainer(ctx, created.ID, backupVolumeTarget+"/.")
	if err != nil {
		return fmt.Errorf("export volume %q: %w", name, err)
	}
	defer stream.Close()
	if err := extractDockerArchive(stream, destination); err != nil {
		return fmt.Errorf("extract volume %q: %w", name, err)
	}
	return nil
}

func (d *Docker) copyVolumeIn(ctx context.Context, image, name, source string) (resultErr error) {
	archive, err := archiveDirectory(source)
	if err != nil {
		return fmt.Errorf("archive volume %q: %w", name, err)
	}
	defer func() {
		archiveName := archive.Name()
		closeErr := archive.Close()
		if errors.Is(closeErr, os.ErrClosed) {
			closeErr = nil
		}
		resultErr = errors.Join(resultErr, closeErr, os.Remove(archiveName))
	}()
	created, err := d.cli.ContainerCreate(ctx, &container.Config{Image: image}, &container.HostConfig{Mounts: []mount.Mount{{
		Type: mount.TypeVolume, Source: name, Target: backupVolumeTarget,
	}}}, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return fmt.Errorf("create volume import helper for %q: %w", name, err)
	}
	defer func() {
		cleanupCtx, cancel := detachedContext(ctx, cleanupTimeout)
		defer cancel()
		if err := d.cli.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove volume import helper for %q: %w", name, err))
		}
	}()
	if err := d.cli.CopyToContainer(ctx, created.ID, backupVolumeTarget, archive, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("import volume %q: %w", name, err)
	}
	return nil
}

func restoreStageVolumeName(workspace, volumeName, generation string) string {
	sum := sha256.Sum256([]byte(workspace + "\x00" + volumeName + "\x00" + generation))
	return "fern-" + workspace + "-restore-" + hex.EncodeToString(sum[:8])
}

func requireEmptyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect volume export directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("volume export destination must be a real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("volume export destination must be empty")
	}
	return nil
}

func extractDockerArchive(stream io.Reader, destination string) error {
	seen := make(map[string]struct{})
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if name == "" || name == "." {
			if header.Typeflag != tar.TypeDir {
				return errors.New("Docker archive root is not a directory")
			}
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != name {
			return fmt.Errorf("unsafe Docker archive path %q", header.Name)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("duplicate Docker archive path %q", header.Name)
		}
		seen[name] = struct{}{}
		target := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, reader, header.Size)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("link or special Docker archive entry rejected: %s", header.Name)
		}
	}
}

func archiveDirectory(root string) (*os.File, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("volume source must be a real directory: %s", root)
	}
	file, err := os.CreateTemp("", ".fern-volume-*.tar")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	writer := tar.NewWriter(file)
	err = filepath.Walk(root, func(path string, entry os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Mode().IsRegular()) {
			return fmt.Errorf("link or special volume entry rejected: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(entry, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.ModTime = time.Time{}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if entry.Mode().IsRegular() {
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(writer, source)
			closeErr := source.Close()
			return errors.Join(copyErr, closeErr)
		}
		return nil
	})
	err = errors.Join(err, writer.Close())
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func compareTrees(expected, actual string) error {
	left, err := treeInventory(expected)
	if err != nil {
		return err
	}
	right, err := treeInventory(actual)
	if err != nil {
		return err
	}
	if len(left) != len(right) {
		return errors.New("volume entry count differs")
	}
	for name, value := range left {
		if right[name] != value {
			return fmt.Errorf("volume entry differs: %s", name)
		}
	}
	return nil
}

func treeInventory(root string) (map[string]string, error) {
	result := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if info.IsDir() {
			result[name] = "d"
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("link or special volume entry rejected: %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		result[name] = "f:" + hex.EncodeToString(digest.Sum(nil))
		return nil
	})
	return result, err
}
