package runsc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/nebler/fern/internal/store"
)

// Every box gets a /30 of boxNet: the host's end of its veth is the first
// address, the box the second.
var boxNet = netip.MustParsePrefix("10.89.0.0/16")

// maxBoxes is how many /30s boxNet holds.
const maxBoxes = 1 << (32 - 16 - 2)

// cgroupParent is the cgroup under the root that holds every box's.
const cgroupParent = "fern"

// box names the host's resources of one environment's box.
type box struct {
	id  string
	cid string // runsc's ID for it
	// dir is <bundles>/<id>, the bundle while the box may run. It is on the
	// node's tmpfs, as containerd keeps a pod's: the spec holds the login.
	dir    string
	rootfs string // its root: an overlay of the image, for the mount points
	data   string // <pool>/boxes/<id>: what may be big, its /tmp and its log
	tmp    string // its /tmp, emptied at every wake
	resolv string // its /etc/resolv.conf
	short  string // a short stable name for its netns and veth
}

func newBox(bundles, pool, id string) box {
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(bundles, id)
	data := filepath.Join(pool, "boxes", id)
	return box{
		// runsc finds a container by a prefix of its ID, so "m1" would be
		// ambiguous next to "m10": every box's ID has the same length.
		id: id, cid: "fern-" + hex.EncodeToString(sum[:16]), dir: dir,
		rootfs: filepath.Join(dir, "rootfs"), data: data, tmp: filepath.Join(data, "tmp"),
		resolv: filepath.Join(bundles, "resolv.conf"),
		short:  hex.EncodeToString(sum[:])[:11],
	}
}

// hosts is the path of the box's /etc/hosts.
func (b box) hosts() string { return filepath.Join(b.dir, "hosts") }

// DaemonHost is the name an agent box reaches the daemon at: the node's end
// of its link.
const DaemonHost = "fern-daemon"

// hostsFile is a box's /etc/hosts: loopback, its own name and, for an agent
// box, the daemon at host.
func hostsFile(id string, host netip.Addr) string {
	s := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + id + "\n"
	if store.IsAgentBox(id) {
		s += host.String() + "\t" + DaemonHost + "\n"
	}
	return s
}

// netns is the path of the box's network namespace, where ip netns keeps
// it: /var/run, which isn't /run everywhere (k3s's image).
func (b box) netns() string { return "/var/run/netns/" + b.nsName() }

func (b box) nsName() string { return "fern-" + b.short }

// veth is the host's end of the box's link; interface names have at most 15
// characters. An agent box's starts with fag, which the firewall lets reach
// the daemon's MCP port.
func (b box) veth() string {
	if store.IsAgentBox(b.id) {
		return "fag" + b.short
	}
	return "fbx" + b.short
}

// addrs returns the host's and the box's address in /30 number n.
func addrs(n int) (host, guest netip.Addr, err error) {
	if n < 0 || n >= maxBoxes {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("no address for box %d: at most %d boxes", n, maxBoxes)
	}
	base := boxNet.Addr().As4()
	off := uint32(n) * 4
	v := uint32(base[0])<<24 | uint32(base[1])<<16 | off
	ip := func(x uint32) netip.Addr {
		return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
	}
	return ip(v + 1), ip(v + 2), nil
}

// netUp is the commands that make the box's network: its namespace, a veth
// to the host, and the routes both ways. gVisor's netstack takes the
// namespace's addresses when the box boots, so a namespace serves one wake.
func netUp(b box, n int) ([][]string, error) {
	host, guest, err := addrs(n)
	if err != nil {
		return nil, err
	}
	ns := b.nsName()
	return [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "link", "add", b.veth(), "type", "veth", "peer", "name", "eth0", "netns", ns},
		{"ip", "addr", "add", host.String() + "/30", "dev", b.veth()},
		{"ip", "link", "set", b.veth(), "up"},
		{"ip", "-n", ns, "addr", "add", guest.String() + "/30", "dev", "eth0"},
		{"ip", "-n", ns, "link", "set", "eth0", "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "add", "default", "via", host.String()},
	}, nil
}

// netDown is the commands that remove the box's network; each may fail
// because it is gone already.
func netDown(b box) [][]string {
	return [][]string{
		{"ip", "link", "del", b.veth()},
		{"ip", "netns", "del", b.nsName()},
	}
}
