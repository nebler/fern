package health

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nebler/fern/internal/api"
)

// killsFor is how long a process the kernel killed in a box stays a problem
// after it was seen: long enough for a person told of it to look.
const killsFor = 15 * time.Minute

// Memory looks at the memory all boxes use together, in the cgroup that
// holds every box's (runsc.Cgroup), which has a limit of its own: whether it
// is nearly used up, and whether the kernel has killed in a box since the
// last look. The kernel keeps the count; the times it rose are in this
// daemon's memory only, as a crashing box's are, so a restart forgets them.
type Memory struct {
	dir string

	// Doctor, the metrics and the notifier look at once, and each must see
	// a kill the other saw first.
	mu     sync.Mutex
	kills  uint64    // memory.events' oom_kill at the last look
	looked time.Time // when that was
	// The kills still a problem: how many, since when they can have
	// happened, and when the last of them was seen.
	recent     uint64
	from, seen time.Time
}

// NewMemory returns a Memory for the boxes' cgroup at dir. Kills counted
// there before now are not news.
func NewMemory(dir string) *Memory {
	m := &Memory{dir: dir, looked: time.Now()}
	m.kills, _ = m.oomKills() // one that can't be read is a problem at the first look
	return m
}

// problems returns what is wrong with the boxes' memory now: none, that
// it is nearly used up, that the kernel killed in a box, or both.
func (m *Memory) problems() []api.Problem {
	const (
		fix   = "`fern top` shows what each box uses and `fern sleep ENV` frees one's; --boxes-memory (MiB) in FERN_DAEMON_ARGS in /etc/fern/daemon.env raises what they may use together, if the server has it"
		again = "`systemctl restart fern-daemon` makes the boxes' cgroup and its limit again"
	)
	used, limit, high, err := m.usage()
	if err != nil {
		return []api.Problem{{Name: "memory", Detail: "what the boxes use together can't be read: " + err.Error(), Fix: again}}
	}
	var out []api.Problem
	if limit > 0 && used >= high {
		out = append(out, api.Problem{
			Name: "memory",
			Detail: fmt.Sprintf("the boxes together use %s of the %s they may: from %s on the kernel slows them, and at the limit it stops the box that uses the most",
				gib(used), gib(limit), gib(high)),
			Fix: fix,
		})
	}
	kills, err := m.oomKills()
	if err != nil {
		return append(out, api.Problem{Name: "memory", About: "killed", Detail: "whether the kernel stopped a box for its memory can't be read: " + err.Error(), Fix: again})
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case kills > m.kills:
		if m.recent == 0 || now.Sub(m.seen) >= killsFor {
			m.recent, m.from = 0, m.looked
		}
		m.recent += kills - m.kills
		m.seen = now
	case now.Sub(m.seen) >= killsFor:
		m.recent = 0
	}
	// A count that fell is a cgroup made anew, by a restart of the node.
	m.kills, m.looked = kills, now
	if m.recent > 0 {
		const clock = "15:04 UTC"
		out = append(out, api.Problem{
			Name: "memory", About: "killed", At: m.seen,
			Detail: fmt.Sprintf("the kernel stopped a box for memory, killing %d of the boxes' processes between %s and %s: a box at its own limit, or the one that used the most when the boxes together were at theirs (%s)",
				m.recent, m.from.UTC().Format(clock), m.seen.UTC().Format(clock), gib(limit)),
			Fix: "its thread was told at the command that ended with it, and the next command starts the box again; --box-memory (MiB) raises a box's own limit and --boxes-memory what they may use together, in FERN_DAEMON_ARGS in /etc/fern/daemon.env",
		})
	}
	return out
}

// usage returns what the boxes use together, their limit and where the
// kernel starts to slow them, in bytes. Without a limit, limit is 0; without
// that point, it is a tenth before the limit. What they use leaves out the
// page cache, which memory.current counts and the kernel takes back first,
// at no harm to a box: a box that has read a few files is not short of
// memory. gVisor keeps what a box's programs allocate in shared memory,
// which the kernel can't drop, so that counts (as in fern top).
func (m *Memory) usage() (used, limit, high int64, err error) {
	if used, err = m.bytes("memory.current"); err != nil {
		return 0, 0, 0, err
	}
	stat, err := m.stat()
	if err != nil {
		return 0, 0, 0, err
	}
	used = max(used-(stat["file"]-stat["shmem"]), 0)
	if limit, err = m.bytes("memory.max"); err != nil {
		return 0, 0, 0, err
	}
	if high, err = m.bytes("memory.high"); err != nil {
		return 0, 0, 0, err
	}
	if high == 0 || high > limit {
		high = limit - limit/10
	}
	return used, limit, high, nil
}

// bytes reads a cgroup file that holds a number of bytes, or "max" for
// none, which is 0.
func (m *Memory) bytes(name string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, name))
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s is %q", name, s)
	}
	return n, nil
}

// stat reads the cgroup's memory.stat, whose "file" holds its page cache
// with its shared memory, and "shmem" the shared memory alone.
func (m *Memory) stat() (map[string]int64, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, "memory.stat"))
	if err != nil {
		return nil, err
	}
	stat := map[string]int64{}
	for line := range strings.Lines(string(data)) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name != "file" && name != "shmem" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("memory.stat: %s is %q", name, value)
		}
		stat[name] = n
	}
	return stat, nil
}

// oomKills returns how many processes in the boxes the kernel has killed
// for memory since their cgroup was made (memory.events' oom_kill): those of
// a box at its own limit too.
func (m *Memory) oomKills() (uint64, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, "memory.events"))
	if err != nil {
		return 0, err
	}
	for sc := bufio.NewScanner(bytes.NewReader(data)); sc.Scan(); {
		if v, ok := strings.CutPrefix(sc.Text(), "oom_kill "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("memory.events: oom_kill is %q", v)
			}
			return n, nil
		}
	}
	return 0, errors.New("memory.events has no oom_kill")
}

// gib writes bytes as GiB, with a decimal.
func gib(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
}
