package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A disk limit per environment (docs/briefs/disk-limit-per-environment.md)
// is a btrfs simple quota group, 1/N, that holds every subvolume of the
// environment: its workspace, its snapshots, a restore's new workspace. A
// subvolume joins its group right after it is made (QGROUP_ASSIGN), not with
// inherit at creation, which leaves 16 KiB per deleted snapshot in the group.
// The group's number is in the environment's record; the group, its use and
// its limit are in the kernel's quota tree, on the disk. With limits off
// (no --env-disk-limit, records without a group) none of this runs.

// ErrNoLimit means an environment has no quota group: limits were off when
// it was made, and the daemon hasn't been started with them since.
var ErrNoLimit = errors.New("no disk limit")

// Quota is an environment's disk limit and what counts against it: every
// block one of its subvolumes wrote, since it joined its group, that its
// workspace or a snapshot still holds. Blocks shared from a golden count
// against the golden's builder; snapshots hold what earlier turns wrote, so
// deleting a file frees only what no snapshot holds.
type Quota struct {
	Group uint64 // its group is 1/Group; 0: it has none
	Used  uint64 // bytes
	Limit uint64 // bytes; 0: none
}

// Near reports whether q's environment has used 90% of its limit or more.
func (q Quota) Near() bool { return q.Limit > 0 && q.Used >= q.Limit/10*9 }

// Full reports whether q's environment has reached its limit: writes in its
// box fail with EDQUOT, "Disk quota exceeded". They fail a little before
// it, as a write reserves room before it counts: at 98% of a 16 MiB limit,
// 320 KiB under it (lab, 2026-11-13). So within 1% of it, and at least 1
// MiB, is full.
func (q Quota) Full() bool {
	return q.Limit > 0 && q.Used+max(q.Limit/100, 1<<20) >= q.Limit
}

// The layouts of linux/btrfs.h's quota ioctls, whose numbers are built as
// ioctl.go builds the others'.
const (
	btrfsQuotaCtl     = btrfsIoctlWrite | btrfsIoctlRead | (16 << 16) | (0x94 << 8) | 40
	btrfsQgroupAssign = btrfsIoctlWrite | (24 << 16) | (0x94 << 8) | 41
	btrfsQgroupCreate = btrfsIoctlWrite | (16 << 16) | (0x94 << 8) | 42
	btrfsQgroupLimit  = btrfsIoctlRead | (48 << 16) | (0x94 << 8) | 43
	btrfsFSInfo       = btrfsIoctlRead | (1024 << 16) | (0x94 << 8) | 31
	btrfsTreeSearch   = btrfsIoctlWrite | btrfsIoctlRead | (4096 << 16) | (0x94 << 8) | 17

	btrfsRootTree    = 1   // the tree of the subvolumes' root items
	btrfsFSTree      = 5   // the pool's top level, whose group is 0/5
	btrfsRootItemKey = 132 // a subvolume's root item, there until the cleaner drops it

	quotaEnableSimple = 4      // BTRFS_QUOTA_CTL_ENABLE_SIMPLE_QUOTA, Linux 6.7
	quotaDisable      = 2      // BTRFS_QUOTA_CTL_DISABLE
	limitMaxRfer      = 1 << 0 // BTRFS_QGROUP_LIMIT_MAX_RFER
	noLimit           = ^uint64(0)
	level1            = 1 << 48 // a group's level, above its number
)

type btrfsQuotaCtlArgs struct{ Cmd, Status uint64 }

type btrfsQgroupAssignArgs struct{ Assign, Src, Dst uint64 }

type btrfsQgroupCreateArgs struct{ Create, QgroupID uint64 }

type btrfsQgroupLimitArgs struct {
	QgroupID                                  uint64
	Flags, MaxRfer, MaxExcl, RsvRfer, RsvExcl uint64
}

type btrfsSearchArgs struct {
	TreeID, MinObjectID, MaxObjectID, MinOffset, MaxOffset, MinTransID, MaxTransID uint64
	MinType, MaxType, NrItems, _                                                   uint32
	_                                                                              [4]uint64
	_                                                                              [4096 - 104]byte
}

type btrfsFSInfoArgs struct {
	MaxID, NumDevices uint64
	FSID              [16]byte
	_                 [1024 - 32]byte
}

// qgroupsDir returns where sysfs shows the quota groups of the btrfs
// filesystem holding root: /sys/fs/btrfs/<fsid>/qgroups, there while quotas
// are on.
func qgroupsDir(root string) (string, error) {
	var info btrfsFSInfoArgs
	if err := poolIoctl(root, btrfsFSInfo, unsafe.Pointer(&info)); err != nil {
		return "", err
	}
	id := info.FSID
	return fmt.Sprintf("/sys/fs/btrfs/%x-%x-%x-%x-%x/qgroups", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

// poolIoctl makes a btrfs ioctl on the directory path, which any of the
// filesystem's files would do for the quota ioctls.
func poolIoctl(path string, request uintptr, args unsafe.Pointer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// Syscall, not RawSyscall: enabling quotas commits a transaction.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), request, uintptr(args)); errno != 0 {
		return errno
	}
	return nil
}

// EnableLimits gives every environment made from now on a quota group with
// a limit of limit bytes (golden builders none), and has Reconcile bring the
// environments made without one in. It turns btrfs's simple quotas on if
// they are off, which counts nothing that exists: a limit counts what is
// written from then on. They stay on. Classic quotas, which cost seconds per
// snapshot on a large pool, are refused. Call it before anything deletes a
// subvolume: at the daemon's start, before Reconcile.
func (s *Store) EnableLimits(ctx context.Context, limit uint64) error {
	if limit == 0 {
		return errors.New("enable disk limits: a limit of 0")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.qgroups == "" {
		return errors.New("enable disk limits: the pool's filesystem can't be found in /sys/fs/btrfs")
	}
	mode, err := s.quotaMode()
	if err != nil {
		return fmt.Errorf("enable disk limits: %w", err)
	}
	if mode == "" {
		// Turned on while btrfs's cleaner still drops a deleted subvolume,
		// whose group the enable doesn't make, the kernel's next commit fails
		// to count a block it frees and forces the pool read-only (Linux
		// 6.10, lab, 2026-11-13). So first wait for the cleaner: nothing
		// deletes meanwhile, as the daemon calls this before Reconcile.
		if err := btrfs(ctx, "subvolume", "sync", s.root); err != nil {
			return fmt.Errorf("enable disk limits: wait for deleted subvolumes: %w", err)
		}
		args := btrfsQuotaCtlArgs{Cmd: quotaEnableSimple}
		err := poolIoctl(s.root, btrfsQuotaCtl, unsafe.Pointer(&args))
		if errors.Is(err, unix.EINVAL) {
			return errors.New("enable disk limits: this kernel has no btrfs simple quotas (Linux 6.7 or later)")
		}
		if err != nil {
			return fmt.Errorf("enable disk limits: turn simple quotas on: %w", err)
		}
		if mode, err = s.quotaMode(); err != nil {
			return fmt.Errorf("enable disk limits: %w", err)
		}
	}
	if mode != "squota" {
		return fmt.Errorf("enable disk limits: the pool has btrfs quotas of mode %q, not simple ones (squota): `btrfs quota disable %s` turns them off", mode, s.root)
	}
	s.limit = limit
	return nil
}

// quotaMode returns the mode of the pool's quotas, "squota" for simple
// ones, or "" while they are off.
func (s *Store) quotaMode() (string, error) {
	data, err := os.ReadFile(filepath.Join(s.qgroups, "mode"))
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(s.qgroups); err == nil {
			return "qgroup", nil // quotas on, from a kernel before simple ones
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Quota returns environment env's disk limit and use, read from sysfs: two
// small files, no program and not its record, as a box's wake reads it.
// Without a group it is the zero Quota. A group the kernel doesn't have
// (quotas were turned off, which forgets them) limits nothing until the
// environment's next subvolume, or the daemon's next start, makes it again.
func (s *Store) Quota(ctx context.Context, env Env) (Quota, error) {
	if env.Group == 0 {
		return Quota{}, nil
	}
	if err := ctx.Err(); err != nil {
		return Quota{}, err
	}
	q := Quota{Group: env.Group}
	dir := filepath.Join(s.qgroups, "1_"+strconv.FormatUint(env.Group, 10))
	for _, f := range []struct {
		name string
		to   *uint64
	}{{"referenced", &q.Used}, {"max_referenced", &q.Limit}} {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if errors.Is(err, fs.ErrNotExist) || s.qgroups == "" {
			return Quota{Group: env.Group}, nil
		}
		if err != nil {
			return Quota{}, fmt.Errorf("disk limit of %s: %w", env.ID, err)
		}
		if *f.to, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err != nil {
			return Quota{}, fmt.Errorf("disk limit of %s: %s: %w", env.ID, f.name, err)
		}
	}
	return q, nil
}

// SetLimit makes environment id's limit limit bytes, 0 for none. A limit
// under what it uses already is taken: its next write fails. The kernel
// keeps it, so no actor needs to: it is one ioctl, and a snapshot or a
// write at the same time sees the old limit or the new one.
func (s *Store) SetLimit(ctx context.Context, id string, limit uint64) error {
	env, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if env.Group == 0 {
		return fmt.Errorf("environment %s: %w: it was made while limits were off; the daemon brings it in at its next start with --env-disk-limit", id, ErrNoLimit)
	}
	if err := s.limitGroup(env.Group, limit); err != nil {
		return fmt.Errorf("limit %s: %w", id, err)
	}
	return nil
}

// limitGroup sets group 1/n's limit to limit bytes, or none for 0.
func (s *Store) limitGroup(n, limit uint64) error {
	args := btrfsQgroupLimitArgs{QgroupID: level1 | n, Flags: limitMaxRfer, MaxRfer: limit}
	if limit == 0 {
		args.MaxRfer = noLimit // the kernel's way to clear it
	}
	if err := poolIoctl(s.root, btrfsQgroupLimit, unsafe.Pointer(&args)); err != nil {
		return fmt.Errorf("btrfs qgroup limit 1/%d: %w", n, err)
	}
	return nil
}

// limitOf is the limit a new group of environment id gets: none for a
// golden builder's, which pays for the goldens that threads start from.
func (s *Store) limitOf(id string) uint64 {
	if IsBuilder(id) {
		return 0
	}
	return s.limit
}

// newGroup makes a quota group for environment id, with its limit, and
// returns its number, one above the largest that exists: never one a
// deleted environment's group, which can outlive it, still has. With
// marked, environment id is being made, and its marker names the group
// before the group exists, so that a crash leaves nothing unnamed but a
// group without members, which Reconcile destroys (ClearGroups).
func (s *Store) newGroup(ctx context.Context, id string, marked bool) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.reserveGroup(id, marked)
	if err != nil {
		return 0, err
	}
	if err := s.step("group: made"); err != nil {
		return 0, err
	}
	if limit := s.limitOf(id); limit > 0 {
		if err := s.limitGroup(n, limit); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// reserveGroup picks and makes newGroup's group under groupMu, which a
// ClearGroups holds too: it never sees a create's group made and not yet
// named, as the marker names it first. A bringIn's is named by the record
// after the lock; it runs in Reconcile, before anything can call
// ClearGroups, and a group destroyed under it would be made again by its
// first join.
func (s *Store) reserveGroup(id string, marked bool) (uint64, error) {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	if s.lastGroup == 0 {
		groups, err := s.groups()
		if err != nil {
			return 0, err
		}
		for _, n := range groups {
			s.lastGroup = max(s.lastGroup, n)
		}
	}
	for n := s.lastGroup + 1; ; n++ {
		if marked {
			if err := s.writeMarker(id, "", n); err != nil {
				return 0, err
			}
		}
		args := btrfsQgroupCreateArgs{Create: 1, QgroupID: level1 | n}
		err := poolIoctl(s.root, btrfsQgroupCreate, unsafe.Pointer(&args))
		if errors.Is(err, unix.EEXIST) {
			continue // made by something else since the last look
		}
		if err != nil {
			return 0, fmt.Errorf("btrfs qgroup create 1/%d: %w", n, err)
		}
		s.lastGroup = n
		return n, nil
	}
}

// groups returns the numbers of the pool's level-1 quota groups, from
// sysfs; none while quotas are off.
func (s *Store) groups() ([]uint64, error) {
	dirs, err := os.ReadDir(s.qgroups)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []uint64
	for _, d := range dirs {
		if rest, ok := strings.CutPrefix(d.Name(), "1_"); ok {
			if n, err := strconv.ParseUint(rest, 10, 64); err == nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// destroyGroup destroys group 1/n, if the kernel lets it. It refuses
// (EBUSY) while the group has a member: a deleted subvolume's group stays
// one until btrfs's cleaner has dropped it and nothing else holds its
// blocks (a builder's golden that threads hold), and then until clearStale
// destroys it; on kernels before 6.17's "fix squota compressed stats leak",
// for good. Such a group is left for a later ClearGroups; it is 40 bytes
// and counts nothing anyone writes.
func (s *Store) destroyGroup(n uint64) error {
	args := btrfsQgroupCreateArgs{Create: 0, QgroupID: level1 | n}
	err := poolIoctl(s.root, btrfsQgroupCreate, unsafe.Pointer(&args))
	if err == nil || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EBUSY) || errors.Is(err, unix.ENOTCONN) {
		return nil
	}
	return fmt.Errorf("btrfs qgroup destroy 1/%d: %w", n, err)
}

// join puts the subvolume at path into quota group 1/group, after it was
// made: what it already holds comes with it. Joining twice is harmless,
// and a group of 0 is no group. A group the kernel no longer has (quotas
// were turned off and on, which forgets them) is made again, with the
// environment's limit.
func (s *Store) join(ctx context.Context, id, path string, group uint64) error {
	if group == 0 {
		return nil
	}
	// Crash tests stop here, before the join, by the subvolume's role: a
	// snapshot's name can hold the time.
	step := "join: " + filepath.Base(path)
	if filepath.Base(filepath.Dir(path)) == "snaps" {
		step = "join: snapshot"
	}
	if err := s.step(step); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var info btrfsSubvolInfo
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), btrfsGetSubvolInfo, uintptr(unsafe.Pointer(&info))); errno != 0 {
		return fmt.Errorf("btrfs subvolume info %s: %w", path, errno)
	}
	args := btrfsQgroupAssignArgs{Assign: 1, Src: info.TreeID, Dst: level1 | group}
	assign := func() error {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), btrfsQgroupAssign, uintptr(unsafe.Pointer(&args)))
		if errno == 0 || errno == unix.EEXIST || errno == unix.ENOTCONN {
			return nil // joined now, before, or quotas are off: nothing counts
		}
		return errno
	}
	err = assign()
	if errors.Is(err, unix.ENOENT) {
		create := btrfsQgroupCreateArgs{Create: 1, QgroupID: level1 | group}
		if err := poolIoctl(s.root, btrfsQgroupCreate, unsafe.Pointer(&create)); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("btrfs qgroup create 1/%d: %w", group, err)
		}
		if limit := s.limitOf(id); limit > 0 {
			if err := s.limitGroup(group, limit); err != nil {
				return err
			}
		}
		err = assign()
	}
	if err != nil {
		return fmt.Errorf("btrfs qgroup assign %s to 1/%d: %w", path, group, err)
	}
	return nil
}

// ClearGroups destroys the quota groups no environment's record or marker
// names, as far as the kernel lets it (destroyGroup): those of deleted
// environments, and one a crash left before its environment's record named
// it. It returns how many it destroyed. Nothing is done while quotas are
// off, and nothing while an environment's record can't be read, whose group
// it can't tell.
func (s *Store) ClearGroups(ctx context.Context) (int, error) {
	if s.qgroups == "" {
		return 0, nil
	}
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	groups, err := s.groups()
	if err != nil || len(groups) == 0 {
		return 0, err
	}
	named, err := s.namedGroups()
	if errors.Is(err, errBadRecord) {
		return 0, nil // Reconcile reports the environment
	}
	if err != nil {
		return 0, fmt.Errorf("clear quota groups: %w", err)
	}
	if err := s.clearStale(ctx); err != nil {
		return 0, fmt.Errorf("clear quota groups: %w", err)
	}
	cleared := 0
	for _, n := range groups {
		if named[n] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return cleared, err
		}
		if err := s.destroyGroup(n); err != nil {
			return cleared, err
		}
		if _, err := os.Stat(filepath.Join(s.qgroups, "1_"+strconv.FormatUint(n, 10))); errors.Is(err, fs.ErrNotExist) {
			cleared++
		}
	}
	return cleared, nil
}

// clearStale destroys the quota groups of deleted subvolumes that hold
// nothing any more, as `btrfs qgroup clear-stale` does, which takes them out
// of their environment's group. The kernel keeps a deleted subvolume's own
// group (0/N) while another subvolume holds what it wrote, and keeps it,
// empty, after that one goes too: a workspace deleted before the snapshots
// that hold its blocks, as a delete and a restore do. Its environment's
// group can't be destroyed while it is a member (Linux 7.0, server,
// 2026-11-13). One that still holds blocks someone else uses stays: they
// count there.
func (s *Store) clearStale(ctx context.Context) error {
	dirs, err := os.ReadDir(s.qgroups)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		rest, ok := strings.CutPrefix(d.Name(), "0_")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(rest, 10, 64)
		if err != nil || n == btrfsFSTree {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.qgroups, d.Name(), "referenced"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // destroyed since the look
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) != "0" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Only a subvolume that is gone: an older kernel may let a live
		// one's group be destroyed, which would stop it counting.
		if gone, err := s.subvolumeGone(n); err != nil || !gone {
			if err != nil {
				return err
			}
			continue
		}
		destroy := btrfsQgroupCreateArgs{Create: 0, QgroupID: n}
		err = poolIoctl(s.root, btrfsQgroupCreate, unsafe.Pointer(&destroy))
		if err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("btrfs qgroup destroy 0/%d: %w", n, err)
		}
	}
	return nil
}

// subvolumeGone reports whether the pool has no subvolume n: no root item,
// which a deleted one keeps until btrfs's cleaner has dropped it.
func (s *Store) subvolumeGone(n uint64) (bool, error) {
	args := btrfsSearchArgs{
		TreeID: btrfsRootTree, MinObjectID: n, MaxObjectID: n,
		MaxOffset: noLimit, MaxTransID: noLimit,
		MinType: btrfsRootItemKey, MaxType: btrfsRootItemKey, NrItems: 1,
	}
	if err := poolIoctl(s.root, btrfsTreeSearch, unsafe.Pointer(&args)); err != nil {
		return false, fmt.Errorf("btrfs tree search for subvolume %d: %w", n, err)
	}
	return args.NrItems == 0, nil
}

// namedGroups returns the groups the environments' records and markers
// name, or an error that is errBadRecord if a record can't be used.
func (s *Store) namedGroups() (map[uint64]bool, error) {
	dirs, err := os.ReadDir(filepath.Join(s.root, "envs"))
	if err != nil {
		return nil, err
	}
	named := make(map[uint64]bool, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		rec, err := s.readRecord(d.Name())
		switch {
		case err == nil:
			named[rec.Group] = true
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("%s: %w: %w", d.Name(), errBadRecord, err)
		}
		_, group, err := s.readMarker(d.Name())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		named[group] = true
	}
	return named, nil
}

// bringIn gives environment env, made while limits were off, a quota group
// with the limit, records it, and puts its workspace and snapshots in. What
// they hold already stays uncounted. A crash before the record leaves a
// group without members, which ClearGroups destroys; one after it, a
// workspace that Reconcile puts in at its next start.
func (s *Store) bringIn(ctx context.Context, env Env) error {
	n, err := s.newGroup(ctx, env.ID, false)
	if err != nil {
		return err
	}
	rec := record{Workspace: env.Workspace, Git: env.Git, Created: env.Created, Group: n}
	if err := s.writeRecord(env.ID, rec); err != nil {
		return err
	}
	if err := s.step("bring in: recorded"); err != nil {
		return err
	}
	if err := s.joinWorkspace(ctx, env.ID, env.Workspace, n); err != nil {
		return err
	}
	snaps, err := s.Snapshots(ctx, env.ID)
	if err != nil {
		return err
	}
	for _, snap := range snaps {
		if err := s.join(ctx, env.ID, s.snapPath(env.ID, snap.ID), n); err != nil {
			return err
		}
	}
	return nil
}

// joinWorkspace puts environment id's workspace into its group, if it is
// there: a worktree T3 removed is not.
func (s *Store) joinWorkspace(ctx context.Context, id, workspace string, group uint64) error {
	if group == 0 {
		return nil
	}
	if fi, err := os.Lstat(workspace); errors.Is(err, fs.ErrNotExist) || err == nil && !isSubvolume(fi) {
		return nil
	} else if err != nil {
		return err
	}
	return s.join(ctx, id, workspace, group)
}

// ParseSize reads a size as a person writes it: bytes, or a number with K,
// M, G or T after it (powers of 1024, an "iB" or "B" after them allowed),
// such as 20G or 512MiB.
func ParseSize(s string) (uint64, error) {
	num := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "B"), "i")
	shift := uint(0)
	if num != "" {
		if sh, ok := map[byte]uint{'K': 10, 'k': 10, 'M': 20, 'm': 20, 'G': 30, 'g': 30, 'T': 40, 't': 40}[num[len(num)-1]]; ok {
			shift, num = sh, num[:len(num)-1]
		}
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil || n > noLimit>>shift {
		return 0, fmt.Errorf("size %q: want bytes, or a number with K, M, G or T, such as 20G", s)
	}
	return n << shift, nil
}
