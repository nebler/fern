package runsc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"
)

// Every box gets a /30 of boxNet: the host's end of its veth is the first
// address, the box the second.
var boxNet = netip.MustParsePrefix("10.89.0.0/16")

// maxBoxes is how many /30s boxNet holds.
const maxBoxes = 1 << (32 - 16 - 2)

// cgroupParent is the cgroup under the root that holds every box's.
const cgroupParent = "fern"

// layout names the node's resources of one box.
type layout struct {
	id  string
	cid string // runsc's ID for it
	// dir is <bundles>/<id>, the bundle while the box may run. It is on the
	// node's tmpfs: the spec holds the login.
	dir    string
	rootfs string // its root: an overlay of the image, for the mount points
	data   string // <pool>/boxes/<id>: what may be big, its /tmp and its log
	tmp    string // its /tmp, emptied at every wake
	resolv string // its /etc/resolv.conf
	short  string // a short stable name for its netns and veth
}

func newLayout(bundles, pool, id string) layout {
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(bundles, id)
	data := filepath.Join(pool, "boxes", id)
	return layout{
		// runsc finds a container by a prefix of its ID, so "m1" would be
		// ambiguous next to "m10": every box's ID has the same length.
		id: id, cid: "fern-" + hex.EncodeToString(sum[:16]), dir: dir,
		rootfs: filepath.Join(dir, "rootfs"), data: data, tmp: filepath.Join(data, "tmp"),
		resolv: filepath.Join(bundles, "resolv.conf"),
		short:  hex.EncodeToString(sum[:])[:11],
	}
}

// hosts is the path of the box's /etc/hosts.
func (b layout) hosts() string { return filepath.Join(b.dir, "hosts") }

// DaemonHost is the name an agent box reaches the daemon at: the node's end
// of its link.
const DaemonHost = "fern-daemon"

// hostsFile is box's /etc/hosts: loopback, its own name and, for an agent
// box, the daemon at host. Without it, localhost goes to DNS.
func hostsFile(box Box, host netip.Addr) string {
	s := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n127.0.1.1\t" + box.ID + "\n"
	if box.Kind == Agent {
		s += host.String() + "\t" + DaemonHost + "\n"
	}
	return s
}

// netns is the path of the box's network namespace, where ip netns keeps
// it: /var/run, which isn't /run everywhere.
func (b layout) netns() string { return "/var/run/netns/" + b.nsName() }

func (b layout) nsName() string { return "fern-" + b.short }

// veth is the host's end of the link of a box of kind; interface names have
// at most 15 characters. An agent box's starts with fag, which the firewall
// lets reach the daemon's MCP port.
func (b layout) veth(kind Kind) string {
	if kind == Agent {
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

// netUp is the commands that make the network of a box of kind: its
// namespace, a veth to the host, and the routes both ways. gVisor's netstack
// takes the namespace's addresses when the box boots, so a namespace serves
// one wake.
func netUp(b layout, kind Kind, n int) ([][]string, error) {
	host, guest, err := addrs(n)
	if err != nil {
		return nil, err
	}
	ns, veth := b.nsName(), b.veth(kind)
	return [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "link", "add", veth, "type", "veth", "peer", "name", "eth0", "netns", ns},
		{"ip", "addr", "add", host.String() + "/30", "dev", veth},
		{"ip", "link", "set", veth, "up"},
		{"ip", "-n", ns, "addr", "add", guest.String() + "/30", "dev", "eth0"},
		{"ip", "-n", ns, "link", "set", "eth0", "up"},
		{"ip", "-n", ns, "link", "set", "lo", "up"},
		{"ip", "-n", ns, "route", "add", "default", "via", host.String()},
	}, nil
}

// netDown is the ip batch that removes the box's network, whatever its
// kind. Each command may fail because what it removes is gone already (one
// of the two veths always is): ip -force goes on to the next.
func netDown(b layout) string {
	return "link del " + b.veth(Services) + "\n" +
		"link del " + b.veth(Agent) + "\n" +
		"netns del " + b.nsName() + "\n"
}
