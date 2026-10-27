//go:build !linux

package main

import "net"

// peerGroup can't tell who is at the other end outside Linux.
func peerGroup(net.Conn) int { return 0 }
