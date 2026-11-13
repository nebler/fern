package store

import "unsafe"

// SetFailAt makes s call failAt at every step of a change with more than
// one, and stop there if it returns an error.
func SetFailAt(s *Store, failAt func(step string) error) { s.failAt = failAt }

// SetRetention makes s keep keepTurns snapshots and refuse new ones below
// minFreeBytes free.
func SetRetention(s *Store, keepTurns int, minFreeBytes uint64) {
	s.keepTurns, s.minFreeBytes = keepTurns, minFreeBytes
}

// SetDiskUnder makes s take dir for a folder on the disk its pool's file is
// on.
func SetDiskUnder(s *Store, dir string) { s.under = dir }

// MountSource is mountSource.
var MountSource = mountSource

// ParseUsage is parseUsage.
var ParseUsage = parseUsage

// SetEnvLimit makes limit the disk limit of s's new environments, without
// turning quotas on: 0 makes them without a quota group.
func SetEnvLimit(s *Store, limit uint64) { s.limit = limit }

// QgroupsDir is where sysfs shows s's pool's quota groups.
func QgroupsDir(s *Store) string { return s.qgroups }

// DisableQuotas turns off the quotas of the btrfs filesystem that holds
// path, which forgets every group: a benchmark's cleanup, so the next one
// starts as a pool without them does.
func DisableQuotas(path string) error {
	args := btrfsQuotaCtlArgs{Cmd: quotaDisable}
	return poolIoctl(path, btrfsQuotaCtl, unsafe.Pointer(&args))
}
