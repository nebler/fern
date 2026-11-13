package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/api"
)

// The free space below which the daemon refuses new snapshots and
// environments, on the pool and on the disk under it: a share of the pool,
// and at least this much. They are the store's minFreeShare and minFreeBytes
// (internal/store/retention.go, Space.Least), repeated here because the
// store builds only for Linux and this command's tests run anywhere.
const (
	leastShare = 0.05
	leastBytes = 2 << 30
)

// leastFree is what the daemon wants free on a pool of size bytes, and on
// the disk under it.
func leastFree(size uint64) uint64 {
	return max(leastBytes, uint64(float64(size)*leastShare))
}

// poolCommand is fern pool grow SIZE, as root on the node.
func poolCommand(ctx context.Context, c command, _ *api.Client, std stdio) (int, error) {
	if c.args[0] != "grow" {
		return 2, fmt.Errorf("unknown command %q: want grow", c.args[0])
	}
	return thisNode().grow(ctx, c.args[1], std.out)
}

// loopPool is a pool that is a file on the server's disk, as the installer
// makes it, in its three sizes. A grow moves them in this order, so after
// one that was cut short the file is bigger than its device, or the device
// bigger than the filesystem: the next grow reads that and goes on from
// there.
type loopPool struct {
	device string // its loop device, /dev/loop0
	file   string // the file the device is of, /var/lib/fern-img/pool.img
	// What the file is long, what of it is written, what the kernel has the
	// device as, and what btrfs has of the device.
	fileSize, written, deviceSize, fsSize uint64
}

// grow makes the pool want big: an absolute size (70G) or one more than the
// pool is now (+10G). It lengthens the pool's file, tells the loop device,
// and has btrfs take the new room, each only if it is still to do, so it is
// safe to run again after it was stopped anywhere. Nothing is unmounted and
// boxes keep running.
//
// It refuses a size under the file's (a file under a mounted filesystem is
// never cut), a pool that is no file, and a size the disk under the file
// can't keep: the file is sparse and grows into that disk as the pool
// fills, so a full pool must still leave the disk what the daemon wants
// free there (leastFree), or the disk runs out first and everything on the
// server fails with it.
func (n node) grow(ctx context.Context, want string, out io.Writer) (int, error) {
	p, err := n.loopPool(ctx)
	if err != nil {
		return 1, err
	}
	// "+10G" is more than the pool is now, which is what btrfs has: after a
	// grow that was cut short the file is already longer, and the same
	// command finishes it instead of adding to it twice.
	target, err := growTarget(want, p.fsSize)
	if err != nil {
		return 2, err
	}
	if target < p.fileSize {
		return 1, fmt.Errorf("%s is under the pool's file, which is %s (%s): a pool is never made smaller. `fern pool grow %s` finishes a grow that was cut short",
			sizeArg(target), size(p.fileSize), p.file, sizeArg(p.fileSize))
	}
	if target == p.fileSize && p.deviceSize >= p.fileSize && p.fsSize >= p.deviceSize {
		_, err := fmt.Fprintf(out, "the pool is %s already: nothing to do\n", size(p.fsSize))
		return 0, errors.Join(err, n.sayRoom(out, p))
	}

	if target > p.fileSize {
		_, free, err := n.room(filepath.Dir(p.file))
		if err != nil {
			return 1, err
		}
		// What the file may still take of the disk, with what it has taken.
		backs := free + p.written
		if target+leastFree(target) > backs {
			return 1, fmt.Errorf("the server's disk can't keep a %s pool: the pool is a file that grows into the disk as it fills (%s, %s of it written), "+
				"and the disk has %s free. Full, the pool must still leave the disk %s (5%% of the pool, at least 2 GiB), below which Fern refuses new snapshots. %s",
				size(target), p.file, size(p.written), size(free), size(leastFree(target)), most(backs, p.fileSize))
		}
		if err := os.Truncate(p.file, int64(target)); err != nil {
			return 1, fmt.Errorf("lengthen the pool's file: %w", err)
		}
		if _, err := fmt.Fprintf(out, "lengthened %s from %s to %s\n", p.file, size(p.fileSize), size(target)); err != nil {
			return 1, err
		}
		p.fileSize = target
	} else if _, err := fmt.Fprintf(out, "%s is %s already: finishing a grow that was cut short\n", p.file, size(p.fileSize)); err != nil {
		return 1, err
	}

	if p.deviceSize < p.fileSize {
		if msg, err := exec.CommandContext(ctx, n.losetup, "-c", p.device).CombinedOutput(); err != nil {
			return 1, fmt.Errorf("losetup -c %s: %w: %s", p.device, err, strings.TrimSpace(string(msg)))
		}
		if p.deviceSize, err = n.deviceSize(p.device); err != nil {
			return 1, err
		}
		if p.deviceSize < p.fileSize {
			return 1, fmt.Errorf("%s is still %s after losetup -c, and its file is %s", p.device, size(p.deviceSize), size(p.fileSize))
		}
		if _, err := fmt.Fprintf(out, "told %s of its file's size (losetup -c)\n", p.device); err != nil {
			return 1, err
		}
	}

	if p.fsSize < p.deviceSize {
		if msg, err := exec.CommandContext(ctx, n.btrfs, "filesystem", "resize", "max", n.pool).CombinedOutput(); err != nil {
			return 1, fmt.Errorf("btrfs filesystem resize max %s: %w: %s", n.pool, err, strings.TrimSpace(string(msg)))
		}
		if p.fsSize, err = n.filesystemSize(ctx); err != nil {
			return 1, err
		}
		if _, err := fmt.Fprintf(out, "btrfs took the room (btrfs filesystem resize max): the pool is %s\n", size(p.fsSize)); err != nil {
			return 1, err
		}
	}
	return 0, n.sayRoom(out, p)
}

// most says the largest pool a disk can keep that backs it with backs
// bytes, for a pool whose file is fileSize now.
func most(backs, fileSize uint64) string {
	// target + leastFree(target) <= backs, for both arms of leastFree.
	fits := uint64(0)
	if backs > leastBytes {
		fits = min(backs-leastBytes, uint64(float64(backs)/(1+leastShare)))
	}
	fits = fits >> 30 << 30
	if fits <= fileSize {
		return "This disk can't keep a larger pool than it has: free space on it (`fern df`, `fern gc`), or give the server a larger disk"
	}
	return fmt.Sprintf("The most this disk can keep is `fern pool grow %s`", sizeArg(fits))
}

// sayRoom prints the room there is now: the pool's, and the disk's under
// its file, with what a full pool would leave of it.
func (n node) sayRoom(out io.Writer, p loopPool) error {
	total, free, err := n.room(n.pool)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "the pool has %s free of %s\n", size(free), size(total)); err != nil {
		return err
	}
	_, diskFree, err := n.room(filepath.Dir(p.file))
	if err != nil {
		return err
	}
	// The file's written part may have grown since it was read; the line is
	// for a person.
	var written uint64
	if st, err := statFile(p.file); err == nil {
		written = st.written
	}
	left := int64(diskFree) - int64(p.fileSize-min(written, p.fileSize))
	_, err = fmt.Fprintf(out, "the server's disk under it has %s free, and %s once the pool is full\n", size(diskFree), size(uint64(max(left, 0))))
	return err
}

// loopPool reads the pool's file and loop device and their sizes. A pool
// that isn't a file (a partition of its own) is an error that says so.
func (n node) loopPool(ctx context.Context) (loopPool, error) {
	f, err := os.Open(filepath.Join(n.proc, "self/mountinfo"))
	if err != nil {
		return loopPool{}, err
	}
	source := mountSource(f, n.pool)
	_ = f.Close()
	if source == "" {
		return loopPool{}, fmt.Errorf("%s isn't mounted from a device: is the pool mounted?", n.pool)
	}
	p := loopPool{device: source}
	backing, err := os.ReadFile(filepath.Join(n.sys, "block", filepath.Base(source), "loop/backing_file"))
	if err != nil {
		return loopPool{}, fmt.Errorf("the pool at %s is %s, not a file on the server's disk: Fern has nothing to grow there. "+
			"Grow the partition or volume itself, then `btrfs filesystem resize max %s`", n.pool, source, n.pool)
	}
	p.file = strings.TrimSpace(string(backing))
	st, err := statFile(p.file)
	if err != nil {
		return loopPool{}, err
	}
	p.fileSize, p.written = st.size, st.written
	if p.deviceSize, err = n.deviceSize(p.device); err != nil {
		return loopPool{}, err
	}
	if p.fsSize, err = n.filesystemSize(ctx); err != nil {
		return loopPool{}, err
	}
	return p, nil
}

// fileStat is a file's length and how much of it is written: a sparse
// file's holes take no room on its disk.
type fileStat struct{ size, written uint64 }

func statFile(path string) (fileStat, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fileStat{}, fmt.Errorf("stat %s: %w", path, err)
	}
	// st_blocks counts 512-byte blocks, whatever the filesystem's own.
	return fileStat{size: uint64(max(st.Size, 0)), written: uint64(max(st.Blocks, 0)) * 512}, nil
}

// deviceSize returns what the kernel has block device dev as, in bytes: its
// size in sysfs is in 512-byte sectors.
func (n node) deviceSize(dev string) (uint64, error) {
	path := filepath.Join(n.sys, "block", filepath.Base(dev), "size")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return sectors * 512, nil
}

// filesystemSize returns what btrfs has of the pool's device, in bytes.
func (n node) filesystemSize(ctx context.Context) (uint64, error) {
	out, err := exec.CommandContext(ctx, n.btrfs, "filesystem", "usage", "-b", n.pool).Output()
	if err != nil {
		return 0, fmt.Errorf("btrfs filesystem usage -b %s: %w (as root?)", n.pool, err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, found := strings.CutPrefix(strings.TrimSpace(line), "Device size:"); found {
			if size, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64); err == nil {
				return size, nil
			}
		}
	}
	return 0, fmt.Errorf("btrfs filesystem usage -b %s printed no device size", n.pool)
}

// growTarget reads a size for a pool that is now bytes long: "70G", or
// "+10G" for that much more. The units are M, G and T, in powers of 1024,
// as truncate takes them.
func growTarget(want string, now uint64) (uint64, error) {
	usage := fmt.Errorf("size %q: want a size such as 70G, or +10G for that much more (M, G or T)", want)
	digits, more := strings.CutPrefix(want, "+")
	if len(digits) < 2 {
		return 0, usage
	}
	shift, ok := map[byte]uint{'M': 20, 'G': 30, 'T': 40}[digits[len(digits)-1]]
	n, err := strconv.ParseUint(digits[:len(digits)-1], 10, 32)
	if !ok || err != nil || n == 0 {
		return 0, usage
	}
	if more {
		return now + n<<shift, nil
	}
	return n << shift, nil
}

// sizeArg writes bytes as grow takes a size: whole G if it is, else M.
func sizeArg(bytes uint64) string {
	if bytes%(1<<30) == 0 {
		return fmt.Sprintf("%dG", bytes>>30)
	}
	return fmt.Sprintf("%dM", bytes>>20)
}

// mountSource returns the device of the mount that holds path, the one
// whose mount point is the longest that path is in, from mountinfo, the
// kernel's list of mounts (proc(5)); "" if it names none. The store reads
// the same to find the disk under the pool (internal/store/space.go).
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

// statfsRoom returns the size and the free space of the filesystem that
// holds path.
func statfsRoom(path string) (total, free uint64, err error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(max(fs.Bsize, 0))
	return fs.Blocks * bsize, fs.Bavail * bsize, nil
}
