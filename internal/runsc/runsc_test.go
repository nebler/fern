package runsc

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/nebler/fern/internal/store"
)

var env = store.Env{
	ID: "demo", Workspace: "/pool/worktrees/r/demo", Claude: "/pool/envs/demo/claude",
	Git: "/pool/repos/r/.git", BoxToken: "tok",
}

// mounts returns the spec's bind mounts as destination: options.
func binds(s spec) map[string]string {
	m := make(map[string]string)
	for _, mt := range s.Mounts {
		if mt.Type == "bind" {
			m[mt.Destination] = mt.Source + " " + strings.Join(mt.Options, ",")
		}
	}
	return m
}

func TestBoxSpec(t *testing.T) {
	box := Box{ID: env.ID, Kind: Services, Env: env}
	b := newLayout("/run/boxes", "/pool", box.ID)
	s := boxSpec(box, b, []string{"PATH=/bin"}, boxVars(box, "w1", "login"))
	want := map[string]string{
		"/etc/resolv.conf": "/run/boxes/resolv.conf rbind,ro",
		"/etc/hosts":       "/run/boxes/demo/hosts rbind,ro",
		"/tmp":             "/pool/boxes/demo/tmp rbind,rw",
		env.Workspace:      env.Workspace + " rbind,rw",
		env.Claude:         env.Claude + " rbind,rw",
		// The git folder and the worktree's .git file are read-only: a box
		// that could change them could run code in T3's server.
		env.Git:                 env.Git + " rbind,ro",
		env.Workspace + "/.git": env.Workspace + "/.git bind,ro",
	}
	if diff := cmp.Diff(want, binds(s)); diff != "" {
		t.Errorf("bind mounts (-want +got):\n%s", diff)
	}
	if !s.Root.Readonly || s.Root.Path != "/run/boxes/demo/rootfs" {
		t.Errorf("root = %+v, want the box's overlay, read-only", s.Root)
	}
	if s.Process.User.UID != store.BoxUID || len(s.Process.Capabilities.Bounding) != 0 || !s.Process.NoNewPrivileges {
		t.Errorf("process runs as %+v with capabilities %v, want the box's user, none", s.Process.User, s.Process.Capabilities)
	}
	for _, v := range []string{"PATH=/bin", "FERN_BOX_TOKEN=tok", "FERN_WAKE_ID=w1", "CLAUDE_CONFIG_DIR=" + env.Claude, "CLAUDE_CODE_OAUTH_TOKEN=login"} {
		if !slices.Contains(s.Process.Env, v) {
			t.Errorf("env lacks %s: %v", v, s.Process.Env)
		}
	}
	if s.Linux.CgroupsPath != "/fern/demo" || s.Linux.Resources.Memory.Limit != 2<<30 || s.Linux.Resources.Pids.Limit != 4096 {
		t.Errorf("cgroup %s with %+v, want /fern/demo with the pod's limits", s.Linux.CgroupsPath, s.Linux.Resources)
	}
	if ns := s.Linux.Namespaces[1]; ns.Type != "network" || ns.Path != b.netns() {
		t.Errorf("network namespace %+v, want %s", ns, b.netns())
	}
}

// Each kind of box turns off what it doesn't run, and a box without
// Claude's folder gets no Claude variables.
func TestBoxVars(t *testing.T) {
	noClaude := env
	noClaude.Claude = ""
	for _, tc := range []struct {
		name string
		box  Box
		want []string // the variables beyond the token, the wake and the workspace
	}{
		{
			"services with Claude",
			Box{ID: "demo", Kind: Services, Env: env},
			[]string{"CLAUDE_CONFIG_DIR=" + env.Claude, "CLAUDE_CODE_OAUTH_TOKEN=login"},
		},
		{"services", Box{ID: "demo", Kind: Services, Env: noClaude}, nil},
		{
			"agent",
			Box{ID: "demo.agent", Kind: Agent, Env: env},
			[]string{"FERN_SERVICES=off", "CLAUDE_CONFIG_DIR=" + env.Claude, "CLAUDE_CODE_OAUTH_TOKEN=login"},
		},
		{"builder", Box{ID: "golden-r", Kind: Builder, Env: noClaude}, []string{"FERN_ON_DEMAND=off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := ""
			if tc.box.Env.Claude != "" {
				login = "login" // Wake reads it only for a box with Claude
			}
			got := boxVars(tc.box, "w1", login)[3:]
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("vars (-want +got):\n%s", diff)
			}
		})
	}
}

// Only an agent box's /etc/hosts names the daemon.
func TestHostsFile(t *testing.T) {
	host := netip.MustParseAddr("10.89.0.1")
	for _, kind := range []Kind{Services, Agent, Builder} {
		got := strings.Contains(hostsFile(Box{ID: "demo", Kind: kind}, host), "10.89.0.1\t"+DaemonHost)
		if want := kind == Agent; got != want {
			t.Errorf("kind %d: hosts names the daemon = %t, want %t", kind, got, want)
		}
	}
}

func TestAddrs(t *testing.T) {
	for _, tc := range []struct {
		n           int
		host, guest string
	}{
		{0, "10.89.0.1", "10.89.0.2"},
		{1, "10.89.0.5", "10.89.0.6"},
		{64, "10.89.1.1", "10.89.1.2"},
		{maxBoxes - 1, "10.89.255.253", "10.89.255.254"},
	} {
		host, guest, err := addrs(tc.n)
		if err != nil || host.String() != tc.host || guest.String() != tc.guest {
			t.Errorf("addrs(%d) = %s, %s, %v; want %s, %s", tc.n, host, guest, err, tc.host, tc.guest)
		}
	}
	if _, _, err := addrs(maxBoxes); err == nil {
		t.Errorf("addrs(%d) succeeded, want no address left", maxBoxes)
	}
}

// Interface names have at most 15 characters, and differ per environment.
func TestNames(t *testing.T) {
	a, b := newLayout("/run/boxes", "/pool", strings.Repeat("x", 63)), newLayout("/run/boxes", "/pool", "demo")
	if len(a.veth(Agent)) > 15 || len(a.nsName()) > 32 {
		t.Errorf("names too long: %s, %s", a.veth(Agent), a.nsName())
	}
	if a.veth(Services) == b.veth(Services) {
		t.Errorf("two environments share veth %s", a.veth(Services))
	}
}

// The firewall replaces its own table whole, so loading it twice is the same
// as once, and keeps boxes from every private range and the node.
func TestFirewall(t *testing.T) {
	rules := firewall(7071)
	if !strings.HasPrefix(rules, "add table inet fern\ndelete table inet fern\n") {
		t.Errorf("firewall doesn't replace its table:\n%s", rules)
	}
	for _, want := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8",
		"hook input", `iifname "fbx*" drop`, `oifname "fbx*" drop`, "masquerade",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("firewall lacks %q:\n%s", want, rules)
		}
	}
}
