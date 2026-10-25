package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// These are the fixed-size Linux UAPI layouts from linux/btrfs.h. x/sys/unix
// does not expose the btrfs subvolume ioctls. Keep the buffers inline: the
// kernel reads the whole 4096-byte argument, including its reserved fields.
const (
	// FS_IOC_SETFLAGS supplies this architecture's _IOW direction bits.
	// Linux uses different direction encodings on, for example, powerpc.
	btrfsIoctlWrite     = unix.FS_IOC_SETFLAGS & 0xe0000000
	btrfsVolWrite       = btrfsIoctlWrite | (4096 << 16) | (0x94 << 8)
	btrfsSubvolCreate   = btrfsVolWrite | 14
	btrfsSnapCreateV2   = btrfsVolWrite | 23
	btrfsSnapDestroy    = btrfsVolWrite | 15
	btrfsSubvolReadOnly = 1 << 1
)

type btrfsVolArgs struct {
	FD   int64
	Name [4088]byte
}

type btrfsVolArgsV2 struct {
	FD       int64
	TransID  uint64
	Flags    uint64
	Reserved [4]uint64
	Name     [4040]byte
}

func createSubvolume(ctx context.Context, path string) error {
	var args btrfsVolArgs
	if err := volumeName(path, args.Name[:]); err != nil {
		return err
	}
	return volumeIoctl(ctx, path, btrfsSubvolCreate, unsafe.Pointer(&args))
}

func snapshotSubvolume(ctx context.Context, source, path string, readOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	args := btrfsVolArgsV2{FD: int64(f.Fd())}
	if readOnly {
		args.Flags = btrfsSubvolReadOnly
	}
	if err := volumeName(path, args.Name[:]); err != nil {
		return err
	}
	// Do not use the asynchronous snapshot flag: the synchronous ioctl flushes
	// the source's writes and commits the atomic snapshot, including database WAL.
	return volumeIoctl(ctx, path, btrfsSnapCreateV2, unsafe.Pointer(&args))
}

func deleteSubvolume(ctx context.Context, path string) error {
	var args btrfsVolArgs
	if err := volumeName(path, args.Name[:]); err != nil {
		return err
	}
	return volumeIoctl(ctx, path, btrfsSnapDestroy, unsafe.Pointer(&args))
}

func volumeName(path string, dst []byte) error {
	name := filepath.Base(path)
	if name == "." || name == ".." || name == "/" || len(name) >= len(dst) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("subvolume name %q: %w", name, unix.EINVAL)
	}
	copy(dst, name)
	return nil
}

func volumeIoctl(ctx context.Context, path string, request uintptr, args unsafe.Pointer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	// Use Syscall, not RawSyscall: a snapshot can block on dirty data and a
	// transaction commit, so the scheduler must release this thread's P.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, parent.Fd(), request, uintptr(args))
	if errno != 0 {
		return fmt.Errorf("btrfs ioctl %s: %w", path, errno)
	}
	return nil
}
