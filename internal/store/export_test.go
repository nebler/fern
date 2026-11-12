package store

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
