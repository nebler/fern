package runsc

import (
	"fmt"
	"strings"
)

// links are the node's ends of boxes' links: services' boxes and agent boxes.
var links = []string{"fbx*", "fag*"}

// blocked are the destinations a box may not reach: private, shared,
// loopback and link-local ranges (other boxes, the node's services, a
// cloud's metadata endpoint), multicast and reserved ones.
var blocked = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"127.0.0.0/8", "169.254.0.0/16", "0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4",
}

// firewall is the nftables ruleset that gives boxes the internet and
// nothing else (architecture §13): no private or link-local address, no
// other box, nothing on the node, and nothing but answers coming in, since
// only the daemon, on the node, connects to a box. It is a table of its
// own, so a drop in it is final whatever other tables accept. Loading it
// replaces the table in one transaction, so it is safe to repeat; nothing
// else may use the table's name (lab/vps.sh's host firewall is fern_host).
//
// Agent boxes (links fag*) get the same, except that they may connect to the
// daemon's MCP port on the node; nothing else reaches that port.
func firewall(mcpPort int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet fern\ndelete table inet fern\n")
	fmt.Fprintf(&b, "table inet fern {\n")
	fmt.Fprintf(&b, "\tchain forward {\n\t\ttype filter hook forward priority -10; policy accept;\n")
	for _, l := range links {
		fmt.Fprintf(&b, "\t\tiifname %q ip daddr { %s } drop\n", l, strings.Join(blocked, ", "))
		fmt.Fprintf(&b, "\t\tiifname %q meta nfproto ipv6 drop\n", l)
		fmt.Fprintf(&b, "\t\toifname %q ct state established,related accept\n", l)
		fmt.Fprintf(&b, "\t\toifname %q drop\n", l)
	}
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority -10; policy accept;\n")
	fmt.Fprintf(&b, "\t\tiifname \"fag*\" tcp dport %d accept\n", mcpPort)
	fmt.Fprintf(&b, "\t\ttcp dport %d drop\n", mcpPort)
	for _, l := range links {
		fmt.Fprintf(&b, "\t\tiifname %q ct state established,related accept\n", l)
		fmt.Fprintf(&b, "\t\tiifname %q drop\n", l)
	}
	fmt.Fprintf(&b, "\t}\n")
	fmt.Fprintf(&b, "\tchain postrouting {\n\t\ttype nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "\t\tip saddr %s oifname != \"fbx*\" oifname != \"fag*\" masquerade\n\t}\n}\n", boxNet)
	return b.String()
}
