//go:build linux && (amd64 || arm64)

package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Reconciled trees may predate the current policy. Check every writable inode,
// not just their root, before starting a worker; existing wrong-project files
// could otherwise be extended despite forbidding project-changing syscalls.
func inspectProjectTree(ctx context.Context, path string, want quotaIdentity) error {
	return filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("quota tree contains a special file")
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		var attr [28]byte
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x801c581f, uintptr(unsafe.Pointer(&attr[0])))
		if errno != 0 {
			return errno
		}
		flags := binary.NativeEndian.Uint32(attr[:4])
		if uint64(st.Dev) != want.Device || binary.NativeEndian.Uint32(attr[12:16]) != want.Project || flags&0x101 != 0 || (entry.IsDir() && flags&0x200 == 0) {
			return errors.New("writable inode escapes the enforced runtime project quota")
		}
		return nil
	})
}

// Read-only Linux UAPI calls. No mount, quota assignment, privileged helper,
// or automatic provisioning. quotactl_fd requires Linux 5.14 or newer.
func inspectProjectQuota(path string) (quotaIdentity, error) {
	var result quotaIdentity
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) || resolved != path {
		return result, errors.New("quota path must be an exact absolute directory without symlinks")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return result, err
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return result, err
	}
	if fs.Type != unix.XFS_SUPER_MAGIC {
		return result, errors.New("runtime storage must be XFS with project quota enforcement")
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return result, err
	}
	// linux/fs.h struct fsxattr is 28 bytes; _IOR('X', 31, struct fsxattr).
	var attr [28]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x801c581f, uintptr(unsafe.Pointer(&attr[0])))
	if errno != 0 {
		return result, fmt.Errorf("FSGETXATTR: %w", errno)
	}
	project := binary.NativeEndian.Uint32(attr[12:16])
	flags := binary.NativeEndian.Uint32(attr[:4])
	// Q_XGETQSTATV v1 is a fixed 160-byte UAPI structure; only the
	// version and flags are needed. QCMD(cmd, PRJQUOTA) = cmd<<8 | 2.
	var stat [160]byte
	stat[0] = 1
	if err := quotaRead(fd, 0x5808, 0, stat[:]); err != nil {
		return result, err
	}
	var limits [112]byte
	if err := quotaRead(fd, 0x5803, project, limits[:]); err != nil {
		return result, err
	}
	result = quotaIdentity{Device: uint64(st.Dev), Project: project, Blocks: binary.NativeEndian.Uint64(limits[8:16]), Inodes: binary.NativeEndian.Uint64(limits[24:32])}
	if err := validateQuotaKernelState(project, flags, stat[0], binary.NativeEndian.Uint16(stat[2:4]), limits[0], limits[1], binary.NativeEndian.Uint32(limits[4:8]), result.Blocks, result.Inodes); err != nil {
		return quotaIdentity{}, err
	}
	return result, nil
}

func quotaRead(fd int, command, project uint32, data []byte) error {
	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL_FD, uintptr(fd), uintptr(command<<8|2), uintptr(project), uintptr(unsafe.Pointer(&data[0])), 0, 0)
	if errno != 0 {
		return fmt.Errorf("read enforced XFS project quota (Linux >=5.14; Q_XGETQUOTA requires host CAP_SYS_ADMIN): %w", errno)
	}
	return nil
}
