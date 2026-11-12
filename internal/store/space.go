package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Volume is a filesystem's room.
type Volume struct {
	Path        string // a folder on it
	Total, Free uint64 // bytes
}

// Space is the room the pool has. A pool that is a file on another disk (a
// loop-mounted image, as the installer makes it) can only grow into what
// that disk has free: Under is that disk, or nil for a pool on a partition
// of its own.
type Space struct {
	Pool  Volume
	Under *Volume
	// Least is the free space below which new snapshots and environments
	// are refused (ErrPoolFull), on the pool and on the disk under it.
	Least uint64
}

// Space returns the room the pool has now.
func (s *Store) Space() (Space, error) {
	pool, err := volume(s.root)
	if err != nil {
		return Space{}, err
	}
	sp := Space{Pool: pool, Least: max(s.minFreeBytes, uint64(float64(pool.Total)*minFreeShare))}
	if s.under != "" {
		under, err := volume(s.under)
		if err != nil {
			return Space{}, err
		}
		sp.Under = &under
	}
	return sp, nil
}

func volume(path string) (Volume, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Volume{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	return Volume{Path: path, Total: st.Blocks * uint64(st.Bsize), Free: st.Bavail * uint64(st.Bsize)}, nil
}

// diskUnder returns the folder of the file the filesystem holding root is
// mounted from, or "" if it isn't a file: the kernel names a loop device's
// file in sysfs.
func diskUnder(root string) string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	dev := mountSource(f, root)
	if dev == "" {
		return ""
	}
	file, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(dev), "loop/backing_file"))
	if err != nil {
		return ""
	}
	return filepath.Dir(strings.TrimSpace(string(file)))
}

// mountSource returns the device of the mount that holds path, the one
// whose mount point is the longest that path is in, from mountinfo, the
// kernel's list of mounts (proc(5)); "" if it names none.
func mountSource(mountinfo io.Reader, path string) string {
	path = filepath.Clean(path)
	var source, at string
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		// ID parent major:minor root mount-point options tags… - type source options
		before, after, ok := strings.Cut(sc.Text(), " - ")
		fields, rest := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 5 || len(rest) < 2 {
			continue
		}
		point := fields[4]
		in := path == point || point == "/" || strings.HasPrefix(path, point+"/")
		// A later mount at the same point covers an earlier one.
		if in && len(point) >= len(at) {
			source, at = rest[1], point
		}
	}
	if !strings.HasPrefix(source, "/dev/") {
		return ""
	}
	return source
}

// Usage is the disk space of a part of the pool. Shared blocks, which
// snapshots and copies of one another hold together, are in Total; Exclusive
// is what nothing else holds, and so the least that deleting the part frees.
type Usage struct {
	Total, Exclusive uint64 // bytes
}

// Usage returns the disk space of environment id: its workspace, snapshots
// and Claude's folder. Blocks its workspace at a path shares with its own
// snapshots count as shared, so Exclusive is low for such an environment.
// btrfs reads every file's extents for it: seconds for a large workspace.
func (s *Store) Usage(ctx context.Context, id string) (Usage, error) {
	env, err := s.Get(ctx, id)
	if err != nil {
		return Usage{}, err
	}
	paths := []string{s.envDir(id)}
	if env.Workspace != s.livePath(id) {
		if _, err := os.Lstat(env.Workspace); err == nil {
			paths = append(paths, env.Workspace)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Usage{}, err
		}
	}
	return PathUsage(ctx, paths...)
}

// PathUsage returns the disk space of paths in the pool together, each
// counted by itself.
func PathUsage(ctx context.Context, paths ...string) (Usage, error) {
	args := append([]string{"filesystem", "du", "--summarize", "--raw"}, paths...)
	out, err := exec.CommandContext(ctx, "btrfs", args...).Output()
	if err != nil {
		return Usage{}, fmt.Errorf("btrfs %s: %w", strings.Join(args, " "), err)
	}
	return parseUsage(out)
}

// parseUsage adds up the rows `btrfs filesystem du --raw` prints: a header,
// then Total, Exclusive, Set shared and the name for each path.
func parseUsage(out []byte) (Usage, error) {
	var u Usage
	rows := 0
	for line := range bytes.Lines(out) {
		fields := strings.Fields(string(line))
		if len(fields) < 4 {
			continue
		}
		total, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue // the header
		}
		// "-" for a path btrfs couldn't tell it of.
		exclusive, _ := strconv.ParseUint(fields[1], 10, 64)
		u.Total += total
		u.Exclusive += exclusive
		rows++
	}
	if rows == 0 {
		return Usage{}, fmt.Errorf("btrfs filesystem du printed no sizes: %q", out)
	}
	return u, nil
}
