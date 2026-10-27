package runsc

import (
	"fmt"
	"strings"
)

// blocked are the destinations a box may not reach: private, shared,
// loopback and link-local ranges (other boxes, the cluster, the node's
// services, a cloud's metadata endpoint), multicast and reserved ones.
var blocked = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4",
}

// firewall is the nftables ruleset that gives boxes the internet and
// nothing else, as the network policy did for pods (architecture §13): no
// private or link-local address, no other box, nothing on the node (which a
// box reaches at the node's own addresses, after kube-proxy's DNAT for a
// service), and nothing but answers coming in, since only the daemon, on
// the node, connects to a box. It is a table of its own, which Kubernetes'
// tools (kube-proxy, kube-router) don't rewrite as they do iptables'
// chains, and a drop in it is final whatever other tables accept. Loading
// it replaces the table in one transaction, so it is safe to repeat.
func firewall() string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet fern\ndelete table inet fern\n")
	fmt.Fprintf(&b, "table inet fern {\n")
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority -10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname \"fbx*\" ip daddr { %s } drop\n", strings.Join(blocked, ", "))
	fmt.Fprintf(&b, "\t\tiifname \"fbx*\" meta nfproto ipv6 drop\n")
	fmt.Fprintf(&b, "\t\toifname \"fbx*\" ct state established,related accept\n")
	fmt.Fprintf(&b, "\t\toifname \"fbx*\" drop\n\t}\n")
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname \"fbx*\" ct state established,related accept\n")
	fmt.Fprintf(&b, "\t\tiifname \"fbx*\" drop\n\t}\n")
	fmt.Fprintf(&b, "\tchain postrouting {\n\t\ttype nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname != \"fbx*\" masquerade\n\t}\n}\n", boxNet)
	return b.String()
}
