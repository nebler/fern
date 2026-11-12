package runsc

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/nebler/fern/internal/setup"
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

// Limits are what a box may use, so that one runaway agent can't starve the
// other boxes and the daemon. They are set when the box wakes.
type Limits struct {
	CPUs   float64
	Memory int64 // bytes
	Pids   int64 // processes and threads
}

// DefaultLimits are a box's limits unless Config says otherwise, and always
// an agent box's.
var DefaultLimits = Limits{CPUs: 2, Memory: 2 << 30, Pids: 4096}

// minMemory is the least memory a box may be given. A box with only idle
// services measures about 32 MiB (lab/t3-run.sh); less than this is a typo.
const minMemory = 128 << 20

// Check says what is wrong with the limits, if anything.
func (l Limits) Check() error {
	switch {
	case l.CPUs < 0.1:
		return fmt.Errorf("%g CPUs: a box needs at least 0.1", l.CPUs)
	case l.Memory < minMemory:
		return fmt.Errorf("%d MiB of memory: a box needs at least %d", l.Memory>>20, minMemory>>20)
	case l.Pids < 64:
		return fmt.Errorf("%d processes: a box needs at least 64", l.Pids)
	}
	return nil
}

// limitsOf returns the limits of a box of kind. An agent box has the
// defaults. A services' box has the configured ones, or what its repository
// asked for, up to the configured most: the workspace's services.yaml is
// the agent's to write, so it must not be able to take the whole node. A
// golden builder's boxes have the same: a setup script builds what a thread
// would, and one sized for the repository's box doesn't fit a smaller one.
func (c Config) limitsOf(kind Kind, asked setup.Limits) (Limits, error) {
	if kind == Agent {
		return DefaultLimits, nil
	}
	lim := c.Limits
	if lim == (Limits{}) {
		lim = DefaultLimits
	}
	most := c.MaxLimits
	if most == (Limits{}) {
		most = lim
	}
	if asked.CPUs != 0 {
		lim.CPUs = min(asked.CPUs, most.CPUs)
	}
	if asked.Memory != 0 {
		lim.Memory = min(asked.Memory<<20, most.Memory)
	}
	if asked.Pids != 0 {
		lim.Pids = min(asked.Pids, most.Pids)
	}
	if err := lim.Check(); err != nil {
		return Limits{}, fmt.Errorf("services.yaml: limits: %w", err)
	}
	return lim, nil
}

// boxSpec returns the spec of box: fern-init (under tini, which reaps
// orphans) as the box's user with no capabilities, a read-only root, the
// environment's folders at their own paths and the box's own /tmp. image is
// the image's environment; vars are the box's own (its token, its login).
func boxSpec(box Box, b layout, image, vars []string, lim Limits) spec {
	env := box.Env
	none := []string{}
	mounts := []mount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/etc/resolv.conf", Type: "bind", Source: b.resolv, Options: []string{"rbind", "ro"}},
		{Destination: "/etc/hosts", Type: "bind", Source: b.hosts(), Options: []string{"rbind", "ro"}},
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
			Env:             slices.Concat(image, vars),
			Cwd:             "/",
			NoNewPrivileges: true,
			Capabilities:    capabilities{Bounding: none, Effective: none, Inheritable: none, Permitted: none, Ambient: none},
			Rlimits:         []rlimit{{Type: "RLIMIT_NOFILE", Hard: 1 << 20, Soft: 1 << 20}},
		},
		Root:     root{Path: b.rootfs, Readonly: true},
		Hostname: box.ID,
		Mounts:   mounts,
		Linux: linux{
			CgroupsPath: "/" + cgroupParent + "/" + box.ID,
			Resources: resources{
				CPU:    cpu{Quota: int64(lim.CPUs * 100_000), Period: 100_000},
				Memory: memory{Limit: lim.Memory},
				Pids:   pids{Limit: lim.Pids},
			},
			Namespaces: []namespace{
				{Type: "pid"}, {Type: "network", Path: b.netns()}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"},
			},
		},
	}
}

// boxLimitsVar tells an agent box's fern-init what its environment's own box
// gets: CPUs, MiB of memory and processes, as FERN_BOX_LIMITS=2:2048:4096.
func boxLimitsVar(lim Limits) string {
	return fmt.Sprintf("FERN_BOX_LIMITS=%g:%d:%d", lim.CPUs, lim.Memory>>20, lim.Pids)
}

// boxVars are the variables fern-init needs: the token that authenticates
// the daemon, the wake's ID (stop requests name it), the workspace, what
// box's kind turns off, and Claude's folder and login if it gets them.
func boxVars(box Box, wakeID, token, login string, mcpPort int) []string {
	vars := []string{
		"FERN_BOX_TOKEN=" + token,
		"FERN_WAKE_ID=" + wakeID,
		"FERN_WORKSPACE=" + box.Env.Workspace,
	}
	switch box.Kind {
	case Services:
	case Agent:
		vars = append(vars, "FERN_SERVICES=off", "FERN_MCP=http://"+net.JoinHostPort(DaemonHost, strconv.Itoa(mcpPort)))
	case Builder:
		vars = append(vars, "FERN_ON_DEMAND=off")
	case Setup:
		vars = append(vars, "FERN_SERVICES=later")
	}
	if box.Env.Claude != "" {
		vars = append(vars, "CLAUDE_CONFIG_DIR="+box.Env.Claude)
	}
	if login != "" {
		vars = append(vars, "CLAUDE_CODE_OAUTH_TOKEN="+login)
	}
	return vars
}
