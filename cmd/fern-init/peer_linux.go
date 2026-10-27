package main

import (
	"net"
	"syscall"
)

// peerGroup returns the process group of the process at the other end of a
// unix socket connection, or 0 if it can't tell.
func peerGroup(conn net.Conn) int {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int32
	_ = raw.Control(func(fd uintptr) {
		if cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
			pid = cred.Pid
		}
	})
	if pid == 0 {
		return 0
	}
	pgid, err := syscall.Getpgid(int(pid))
	if err != nil {
		return 0
	}
	return pgid
}
