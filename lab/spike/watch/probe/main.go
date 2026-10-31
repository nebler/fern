//go:build spike

// Command probe prints the fanotify events under a directory, each with the
// writer's PID and cgroup: can the daemon tell which box changed a file?
//
//	probe DIR
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	dir := os.Args[1]
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_REPORT_DFID_NAME, unix.O_RDONLY|unix.O_LARGEFILE)
	check(err)
	mask := uint64(unix.FAN_MODIFY | unix.FAN_CLOSE_WRITE | unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_MOVED_FROM |
		unix.FAN_MOVED_TO | unix.FAN_ATTRIB | unix.FAN_ONDIR | unix.FAN_EVENT_ON_CHILD)
	dirs := map[string]string{} // file handle → path
	check(filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		check(unix.FanotifyMark(fd, unix.FAN_MARK_ADD, mask, unix.AT_FDCWD, p))
		h, _, err := unix.NameToHandleAt(unix.AT_FDCWD, p, 0)
		check(err)
		dirs[fmt.Sprintf("%d:%x", h.Type(), h.Bytes())] = p
		return nil
	}))
	fmt.Fprintln(os.Stdout, "marked", len(dirs), "dirs")
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		check(err)
		for b := buf[:n]; len(b) > 0; {
			m := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&b[0]))
			ev := b[m.Metadata_len:m.Event_len]
			// One info record: header (4), fsid (8), handle bytes (4), type (4), handle, name.
			path := "?"
			if len(ev) > 20 {
				hlen := binary.NativeEndian.Uint32(ev[12:])
				htype := int32(binary.NativeEndian.Uint32(ev[16:]))
				handle := ev[20 : 20+hlen]
				name := strings.TrimRight(string(ev[20+hlen:]), "\x00")
				path = dirs[fmt.Sprintf("%d:%x", htype, handle)] + "/" + name
			}
			cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", m.Pid))
			comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", m.Pid))
			fmt.Fprintf(os.Stdout, "%#x pid=%d %s cg=%s %s\n", m.Mask, m.Pid, strings.TrimSpace(string(comm)), strings.TrimSpace(string(cg)), path)
			b = b[m.Event_len:]
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
