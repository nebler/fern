package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

func runtimeStoragePrerequisite() (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("unsupported runtime storage platform: native Linux with enforced XFS project quotas is required; Docker Desktop is unsupported")
	}
	root := os.Getenv("FERN_RUNTIME_STORAGE_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("FERN_RUNTIME_STORAGE_ROOT must name an absolute operator-provisioned XFS project directory with inherited project ID and enforced nonzero hard byte and inode limits")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("FERN_RUNTIME_STORAGE_ROOT is not a directory")
	}
	// The provider validates XFS, project inheritance and enforced hard limits.
	// This harness never provisions quotas or substitutes unbounded storage.
	return root, nil
}

func requireNativeLinuxDocker(ctx context.Context, cli *client.Client) error {
	info, err := cli.Info(ctx)
	if err != nil {
		return err
	}
	if info.OSType != "linux" || strings.Contains(strings.ToLower(info.OperatingSystem), "docker desktop") || strings.EqualFold(info.Name, "docker-desktop") {
		return errors.New("unsupported Docker daemon: native Linux sharing the operator-provisioned XFS project root is required; Docker Desktop is unsupported")
	}
	return nil
}

func withinHarnessRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Called after exact compute cleanup. Ambiguous Docker observations retain the
// disposable directory rather than removing storage beneath a surviving writer.
func removeHarnessRoot(cli *client.Client, root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("retain harness root %s: %w", root, err)
	}
	for _, item := range containers {
		for _, mounted := range item.Mounts {
			if mounted.Source != "" && withinHarnessRoot(root, mounted.Source) {
				return fmt.Errorf("retain harness root %s: container %s still references it", root, item.ID)
			}
		}
	}
	volumes, err := cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return fmt.Errorf("retain harness root %s: %w", root, err)
	}
	for _, item := range volumes.Volumes {
		if device := item.Options["device"]; device != "" && withinHarnessRoot(root, device) {
			return fmt.Errorf("retain harness root %s: volume %s still references it", root, item.Name)
		}
	}
	return os.RemoveAll(root)
}
