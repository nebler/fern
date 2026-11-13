package runsc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Cgroup is the cgroup that holds every box's: its memory is what all boxes
// use together, and its limits are theirs together.
const Cgroup = "/sys/fs/cgroup/" + cgroupParent

// nodeReserve is the memory the boxes together leave to the node itself. A
// server with T3 idle and no box awake uses about 0.9 GiB (the VPS's
// baseline: 1,372 MiB with ten boxes awake at 48 MiB each, of which T3's
// server is 270 MiB and the daemon 11); the rest is for T3 at work, the
// commands and gits it and the daemon start, a backup's send, and the file
// cache the pool lives on.
const nodeReserve = 1536 << 20

// GroupMemory returns the most memory all boxes together may use on a
// machine with total bytes of it: all but nodeReserve, and on a small
// machine at least half. Each box has a limit of its own (Limits), but four
// boxes at 2 GiB are more than an 8 GB server has, and the kernel then kills
// what it likes, T3 or the daemon included; with a limit on their cgroup it
// kills in a box.
func GroupMemory(total int64) int64 {
	return max(total-nodeReserve, total/2)
}

// groupHigh is where the kernel starts to take memory back from the boxes
// and to slow them, under a limit for all of them: a tenth before it, so
// that they are slow before one is killed.
func groupHigh(limit int64) int64 { return limit - limit/10 }

// machineMemory returns the machine's memory in bytes, from a /proc/meminfo.
func machineMemory(meminfo string) (int64, error) {
	data, err := os.ReadFile(meminfo)
	if err != nil {
		return 0, err
	}
	for sc := bufio.NewScanner(bytes.NewReader(data)); sc.Scan(); {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
		if err != nil || kb <= 0 {
			return 0, fmt.Errorf("%s: MemTotal is %q", meminfo, strings.TrimSpace(rest))
		}
		return kb << 10, nil
	}
	return 0, errors.New(meminfo + " has no MemTotal")
}

// limitGroup gives the boxes' cgroup at dir its limit for all boxes
// together, and where the kernel slows them before it. Writing the same
// again changes nothing.
func limitGroup(dir string, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("a limit of %d bytes for all boxes", limit)
	}
	for _, f := range []struct {
		name  string
		bytes int64
	}{{"memory.high", groupHigh(limit)}, {"memory.max", limit}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(strconv.FormatInt(f.bytes, 10)), 0o644); err != nil {
			return err
		}
	}
	return nil
}
