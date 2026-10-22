// Package csi is Fern's CSI node plugin. It bind-mounts one of an
// environment's folders (its workspace, Claude's folder or the main
// repository's git folder) into the pod that names it in an inline volume.
package csi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebler/fern/internal/store"
)

// Name is the CSI driver name that box pods use in their inline volumes.
const Name = "fern.dev"

// Volume context keys. Kubelet adds the pod's identity because the CSIDriver
// object sets podInfoOnMount.
const (
	keyEnv       = "env"
	keyFolder    = "folder" // "workspace" (the default), "claude" or "git"
	keyEphemeral = "csi.storage.k8s.io/ephemeral"
	keyPod       = "csi.storage.k8s.io/pod.name"
	keyNamespace = "csi.storage.k8s.io/pod.namespace"
	keyPodUID    = "csi.storage.k8s.io/pod.uid"
	keyAccount   = "csi.storage.k8s.io/serviceAccount.name"
)

// Plugin serves the CSI Identity and Node services.
type Plugin struct {
	csispec.UnimplementedIdentityServer
	csispec.UnimplementedNodeServer

	node string
	env  func(ctx context.Context, id string) (store.Env, error)
	log  *slog.Logger
}

// New returns a Plugin for node that looks environments up with env.
func New(node string, env func(ctx context.Context, id string) (store.Env, error), log *slog.Logger) *Plugin {
	return &Plugin{node: node, env: env, log: log}
}

// Register adds the plugin's services to s.
func (p *Plugin) Register(s *grpc.Server) {
	csispec.RegisterIdentityServer(s, p)
	csispec.RegisterNodeServer(s, p)
}

// GetPluginInfo returns the driver's name.
func (p *Plugin) GetPluginInfo(context.Context, *csispec.GetPluginInfoRequest) (*csispec.GetPluginInfoResponse, error) {
	return &csispec.GetPluginInfoResponse{Name: Name, VendorVersion: "0.1.0"}, nil
}

// GetPluginCapabilities returns no capabilities: the plugin has no controller.
func (p *Plugin) GetPluginCapabilities(context.Context, *csispec.GetPluginCapabilitiesRequest) (*csispec.GetPluginCapabilitiesResponse, error) {
	return &csispec.GetPluginCapabilitiesResponse{}, nil
}

// Probe reports the plugin as ready.
func (p *Plugin) Probe(context.Context, *csispec.ProbeRequest) (*csispec.ProbeResponse, error) {
	return &csispec.ProbeResponse{}, nil
}

// NodeGetInfo returns the node's ID.
func (p *Plugin) NodeGetInfo(context.Context, *csispec.NodeGetInfoRequest) (*csispec.NodeGetInfoResponse, error) {
	return &csispec.NodeGetInfoResponse{NodeId: p.node}, nil
}

// NodeGetCapabilities returns no capabilities: inline volumes need no staging.
func (p *Plugin) NodeGetCapabilities(context.Context, *csispec.NodeGetCapabilitiesRequest) (*csispec.NodeGetCapabilitiesResponse, error) {
	return &csispec.NodeGetCapabilitiesResponse{}, nil
}

// NodePublishVolume bind-mounts the environment's folder at the target path.
// Publishing a volume that is already mounted succeeds.
//
// A read-only volume is made read-only by the container runtime, from the
// pod's volume mount: a read-only remount here would change only this
// namespace's copy of the mount, not the one that reaches the box.
func (p *Plugin) NodePublishVolume(ctx context.Context, req *csispec.NodePublishVolumeRequest) (*csispec.NodePublishVolumeResponse, error) {
	vc := req.GetVolumeContext()
	env, target := vc[keyEnv], req.GetTargetPath()
	switch {
	case vc[keyEphemeral] != "true":
		return nil, status.Error(codes.InvalidArgument, "only inline ephemeral volumes are supported")
	case env == "":
		return nil, status.Error(codes.InvalidArgument, `volume attribute "env" is required`)
	case target == "":
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	// Mounts aren't authorized yet; log who asked, for the record.
	p.log.InfoContext(ctx, "publish",
		slog.String("env", env),
		slog.String("folder", vc[keyFolder]),
		slog.String("pod", vc[keyNamespace]+"/"+vc[keyPod]),
		slog.String("pod_uid", vc[keyPodUID]),
		slog.String("service_account", vc[keyAccount]),
		slog.String("target", target))

	src, err := p.source(ctx, env, vc[keyFolder])
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mounted, err := isMountpoint(target)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !mounted {
		if err := unix.Mount(src, target, "", unix.MS_BIND, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "bind %s on %s: %v", src, target, err)
		}
	}
	return &csispec.NodePublishVolumeResponse{}, nil
}

// source returns the host path of environment id's folder.
func (p *Plugin) source(ctx context.Context, id, folder string) (string, error) {
	env, err := p.env(ctx, id)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidID) {
		return "", status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	var src string
	switch folder {
	case "", "workspace":
		src = env.Workspace
	case "claude":
		src = env.Claude
	case "git":
		src = env.Git
	default:
		return "", status.Errorf(codes.InvalidArgument, "unknown folder %q", folder)
	}
	if src == "" {
		return "", status.Errorf(codes.NotFound, "environment %s has no %s folder", id, folder)
	}
	return src, nil
}

// NodeUnpublishVolume unmounts the target path and removes it. Unpublishing
// a volume that isn't mounted succeeds.
func (p *Plugin) NodeUnpublishVolume(ctx context.Context, req *csispec.NodeUnpublishVolumeRequest) (*csispec.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	mounted, err := isMountpoint(target)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if mounted {
		if err := unix.Unmount(target, 0); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
		}
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	p.log.InfoContext(ctx, "unpublish", slog.String("target", target))
	return &csispec.NodeUnpublishVolumeResponse{}, nil
}

// isMountpoint reports whether path is a mount point in this mount namespace.
func isMountpoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Field 5 is the mount point, with spaces and the like octal-escaped.
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescape(fields[4]) == path {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}
	return false, nil
}

func unescape(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
