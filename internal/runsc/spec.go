package runsc

import (
	"path/filepath"

	"github.com/nebler/fern/internal/store"
)

// The parts of an OCI runtime spec (config.json) a box uses.
type (
	spec struct {
		OCIVersion string  `json:"ociVersion"`
		Process    process `json:"process"`
		Root       root    `json:"root"`
		Hostname   string  `json:"hostname"`
		Mounts     []mount `json:"mounts"`
		Linux      linux   `json:"linux"`
	}
	process struct {
		User            user         `json:"user"`
		Args            []string     `json:"args"`
		Env             []string     `json:"env"`
		Cwd             string       `json:"cwd"`
		NoNewPrivileges bool         `json:"noNewPrivileges"`
		Capabilities    capabilities `json:"capabilities"`
		Rlimits         []rlimit     `json:"rlimits"`
	}
	user struct {
		UID uint32 `json:"uid"`
		GID uint32 `json:"gid"`
	}
	// capabilities are all empty: the box has none.
	capabilities struct {
		Bounding    []string `json:"bounding"`
		Effective   []string `json:"effective"`
		Inheritable []string `json:"inheritable"`
		Permitted   []string `json:"permitted"`
		Ambient     []string `json:"ambient"`
	}
	rlimit struct {
		Type string `json:"type"`
		Hard uint64 `json:"hard"`
		Soft uint64 `json:"soft"`
	}
	root struct {
		Path     string `json:"path"`
		Readonly bool   `json:"readonly"`
	}
	mount struct {
		Destination string   `json:"destination"`
		Type        string   `json:"type"`
		Source      string   `json:"source"`
		Options     []string `json:"options,omitempty"`
	}
	linux struct {
		CgroupsPath string      `json:"cgroupsPath"`
		Resources   resources   `json:"resources"`
		Namespaces  []namespace `json:"namespaces"`
	}
	resources struct {
		CPU    cpu    `json:"cpu"`
		Memory memory `json:"memory"`
		Pids   pids   `json:"pids"`
	}
	cpu struct {
		Quota  int64  `json:"quota"`
		Period uint64 `json:"period"`
	}
	memory struct {
		Limit int64 `json:"limit"`
	}
	pids struct {
		Limit int64 `json:"limit"`
	}
	namespace struct {
		Type string `json:"type"`
		Path string `json:"path,omitempty"`
	}
)

// The box's limits, as the pod template has them: one runaway agent mustn't
// starve the other boxes and the daemon.
const (
	cpuLimit    = 2       // CPUs
	memoryLimit = 2 << 30 // bytes
	pidsLimit   = 4096    // as kubelet's pod-max-pids
)

// boxSpec returns the spec of env's box: fern-init (under tini, which reaps
// orphans as a pod's pause container did) as the box's user with no
// capabilities, a read-only root, the environment's folders at their own
// paths and the box's own /tmp. image is the image's environment; vars are
// the box's own (its token, its login).
func boxSpec(env store.Env, b box, image, vars []string) spec {
	none := []string{}
	mounts := []mount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: b.resolv, Options: []string{"rbind", "ro"}},
		{Destination: "/tmp", Type: "bind", Source: b.tmp, Options: []string{"rbind", "rw"}},
		{Destination: env.Workspace, Type: "bind", Source: env.Workspace, Options: []string{"rbind", "rw"}},
	}
	if env.Claude != "" {
		mounts = append(mounts, mount{Destination: env.Claude, Type: "bind", Source: env.Claude, Options: []string{"rbind", "rw"}})
	}
	if env.Git != "" {
		// The git folder and the worktree's .git file, which names it, are
		// read-only: a box that could change the folder's config or hooks,
		// or point the file elsewhere, could run code in T3's server, which
		// runs git there (architecture §13).
		gitFile := filepath.Join(env.Workspace, ".git")
		mounts = append(mounts,
			mount{Destination: env.Git, Type: "bind", Source: env.Git, Options: []string{"rbind", "ro"}},
			mount{Destination: gitFile, Type: "bind", Source: gitFile, Options: []string{"bind", "ro"}})
	}
	return spec{
		OCIVersion: "1.2.0",
		Process: process{
			User:            user{UID: store.BoxUID, GID: store.BoxGID},
			Args:            []string{"/usr/bin/tini", "-s", "--", "/usr/local/bin/fern-init"},
			Env:             append(append([]string{}, image...), vars...),
			Cwd:             "/",
			NoNewPrivileges: true,
			Capabilities:    capabilities{Bounding: none, Effective: none, Inheritable: none, Permitted: none, Ambient: none},
			Rlimits:         []rlimit{{Type: "RLIMIT_NOFILE", Hard: 1 << 20, Soft: 1 << 20}},
		},
		Root:     root{Path: b.rootfs, Readonly: true},
		Hostname: env.ID,
		Mounts:   mounts,
		Linux: linux{
			CgroupsPath: "/" + cgroupParent + "/" + env.ID,
			Resources: resources{
				CPU:    cpu{Quota: cpuLimit * 100_000, Period: 100_000},
				Memory: memory{Limit: memoryLimit},
				Pids:   pids{Limit: pidsLimit},
			},
			Namespaces: []namespace{
				{Type: "pid"}, {Type: "network", Path: b.netns()}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"},
			},
		},
	}
}

// boxVars are the variables fern-init needs: the token that authenticates
// the daemon, the wake's ID (stop requests name it), the workspace, and
// Claude's folder and login unless the box is a golden builder's.
func boxVars(env store.Env, wakeID, login string, builder bool) []string {
	vars := []string{
		"FERN_BOX_TOKEN=" + env.BoxToken,
		"FERN_POD_UID=" + wakeID,
		"FERN_WORKSPACE=" + env.Workspace,
	}
	if builder {
		return append(vars, "FERN_ON_DEMAND=off")
	}
	if env.Claude != "" {
		vars = append(vars, "CLAUDE_CONFIG_DIR="+env.Claude)
	}
	if login != "" {
		vars = append(vars, "CLAUDE_CODE_OAUTH_TOKEN="+login)
	}
	return vars
}
