package runsc

// blocked are the destinations a box may not reach: private, shared,
// loopback and link-local ranges (other boxes, the cluster, the node's
// services, a cloud's metadata endpoint).
var blocked = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4",
}

// The chains of the boxes' firewall, in the filter table.
const (
	chainOut = "FERN-BOX-OUT" // what boxes send through the node
	chainIn  = "FERN-BOX-IN"  // what boxes send to the node itself
)

// firewall is the iptables commands that give boxes the internet and
// nothing else, as the network policy did for pods (architecture §13): no
// private or link-local address, no other box, nothing on the node (which a
// box reaches at its own address, not through a route), and nothing but
// answers coming in, since only the daemon, on the node, connects to a box.
// Every command is safe to repeat: chains are made if missing and filled
// afresh, and the jumps to them go first, ahead of k3s's own rules.
// Commands whose failure means only "already there" are marked so.
func firewall() []rule {
	var rules []rule
	for _, c := range []string{chainOut, chainIn} {
		rules = append(rules, rule{args: []string{"-N", c}, mayFail: true}, rule{args: []string{"-F", c}})
	}
	for _, dst := range blocked {
		rules = append(rules, rule{args: []string{"-A", chainOut, "-d", dst, "-j", "DROP"}})
	}
	rules = append(rules,
		rule{args: []string{"-A", chainOut, "-j", "ACCEPT"}},
		// Answers to the daemon's connections; nothing a box starts.
		rule{args: []string{"-A", chainIn, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}},
		rule{args: []string{"-A", chainIn, "-j", "DROP"}},
	)
	jumps := [][]string{
		{"FORWARD", "-i", "fbx+", "-j", chainOut},
		{"FORWARD", "-o", "fbx+", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"FORWARD", "-o", "fbx+", "-j", "DROP"},
		{"INPUT", "-i", "fbx+", "-j", chainIn},
	}
	// Inserted in reverse, so they end up in this order at the top.
	for i := len(jumps) - 1; i >= 0; i-- {
		rules = append(rules, rule{args: append([]string{"-I"}, jumps[i]...), unless: append([]string{"-C"}, jumps[i]...)})
	}
	nat := []string{"-t", "nat", "POSTROUTING", "-s", boxNet.String(), "!", "-o", "fbx+", "-j", "MASQUERADE"}
	rules = append(rules, rule{
		args:   append([]string{"-t", "nat", "-A"}, nat[2:]...),
		unless: append([]string{"-t", "nat", "-C"}, nat[2:]...),
	})
	return rules
}

// rule is one iptables command. It is skipped if unless succeeds (the rule
// is there), and its failure ignored if mayFail.
type rule struct {
	args    []string
	unless  []string
	mayFail bool
}
