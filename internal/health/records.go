package health

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/api"
)

// The room for btrfs's records below which it is a problem, when the
// device has less than recordsSpare unallocated to add to it.
const (
	recordsLeast = 256 << 20
	recordsSpare = 1 << 30
)

// recordsEvery is how long a reading of btrfs's room for its records is
// used before btrfs is asked again. Asking runs a program that walks the
// filesystem's block groups, and the notifier looks every minute: on a busy
// pool that is too often, and the room moves slowly.
const recordsEvery = 10 * time.Minute

// recordsTimeout bounds one run of btrfs: a look for the node's problems
// never waits longer for it.
const recordsTimeout = 20 * time.Second

// Records reads btrfs's room for its own records (its metadata): what is
// left of what it has set aside, and what the device has unallocated to
// set aside more. With neither, writes fail while free space still shows,
// which statfs, and so the store's guard, doesn't see: on 2026-11-13 a pool
// said 3.4 GiB free with 1 MiB unallocated and its metadata three quarters
// used, about 2 GiB of writes from failing.
//
// It asks btrfs at most every recordsEvery and answers from the last
// reading in between, so after the pool is grown a problem is listed for up
// to that long. It is on no wake's or snapshot's path: only the looks for
// the node's problems read it (the notifier's, `fern doctor`, `fern top`,
// the fern_problems metric).
type Records struct {
	Btrfs string // btrfs-progs' btrfs
	Pool  string // the pool

	mu   sync.Mutex
	at   time.Time // when btrfs was last asked; zero: never
	room room
	ok   bool // whether btrfs said, that time
}

// room is btrfs's room for its records, in bytes.
type room struct {
	unallocated uint64 // what the device has to add to the records' room
	size, used  uint64 // the room set aside for records, and what is used of it
}

// left is what is free of the room set aside.
func (r room) left() uint64 { return r.size - min(r.used, r.size) }

// low reports whether writes may soon fail for want of room for records.
func (r room) low() bool { return r.unallocated < recordsSpare && r.left() < recordsLeast }

// read returns the room as of the last reading, which is made now if the
// last is older than recordsEvery, and when that was; false if btrfs didn't
// say then. One caller asks btrfs at a time, and the others wait for its
// answer.
func (r *Records) read(ctx context.Context) (room, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < recordsEvery {
		return r.room, r.at, r.ok
	}
	run, cancel := context.WithTimeout(ctx, recordsTimeout)
	defer cancel()
	out, err := exec.CommandContext(run, r.Btrfs, "filesystem", "usage", "-b", r.Pool).Output()
	if err != nil && ctx.Err() != nil {
		// The caller left, or the daemon is stopping: btrfs said nothing
		// either way, and the last reading stands until someone stays.
		return r.room, r.at, r.ok
	}
	r.at = time.Now()
	r.room, r.ok = room{}, false
	if err == nil {
		r.room, r.ok = metadataRoom(string(out))
	}
	return r.room, r.at, r.ok
}

// problem returns the problem with btrfs's room for its records, if there
// is one. grow says whether the pool is a file, which `fern pool grow`
// makes larger.
func (r *Records) problem(ctx context.Context, grow bool) (api.Problem, bool) {
	got, at, ok := r.read(ctx)
	if !ok || !got.low() {
		return api.Problem{}, false
	}
	fix := "give the pool's partition more room, then `btrfs filesystem resize max " + r.Pool + "`, or delete threads that are done"
	if grow {
		fix = "`fern pool grow +10G` on the server, as root, or delete threads that are done"
	}
	return api.Problem{
		Name: "pool records", At: at,
		Detail: fmt.Sprintf("btrfs has %d MiB left for its records and %d MiB it could add: the pool can fail writes with free space showing",
			got.left()>>20, got.unallocated>>20),
		Fix: fix + "; the daemon reads this every ten minutes, so it stays listed for up to that long",
	}, true
}

// metadataRoom reads from `btrfs filesystem usage -b` what the device has
// unallocated, and the size and use of the metadata it has set aside.
func metadataRoom(usage string) (r room, ok bool) {
	var haveUnallocated, haveMetadata bool
	for line := range strings.SplitSeq(usage, "\n") {
		line = strings.TrimSpace(line)
		if rest, found := strings.CutPrefix(line, "Device unallocated:"); found {
			r.unallocated, haveUnallocated = number(rest)
		}
		if rest, found := strings.CutPrefix(line, "Metadata,"); found {
			// Metadata,DUP: Size:1073741824, Used:781533184 (72.79%)
			if _, after, found := strings.Cut(rest, "Size:"); found {
				r.size, haveMetadata = number(after)
			}
			if _, after, found := strings.Cut(rest, "Used:"); found {
				var usedOK bool
				r.used, usedOK = number(after)
				haveMetadata = haveMetadata && usedOK
			}
		}
	}
	return r, haveUnallocated && haveMetadata
}

// number reads the digits s starts with, after spaces.
func number(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	end := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(s)
	}
	n, err := strconv.ParseUint(s[:end], 10, 64)
	return n, err == nil
}
