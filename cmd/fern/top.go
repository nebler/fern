package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nebler/fern/internal/api"
)

// top prints what the node is doing now, on one screen, once: fern top, as
// root on the node. A row for each box that is up, with what it uses and
// what holds it awake; then the environments there are, the room the pool
// and the disk under it have, and what the daemon knows is wrong, as doctor
// words it. It always exits 0: doctor is the check.
func top(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	return 0, thisNode().top(ctx, cl, time.Now(), std.out)
}

// upBox is a box that is up, as the node's own files show it.
type upBox struct {
	id     string
	since  time.Time     // when it was started; zero if its bundle doesn't say
	memory uint64        // bytes it holds now, without the page cache
	cpu    time.Duration // CPU time it has used since its start, gVisor's included
}

// top writes what the node is doing at now to out. It reads the boxes from
// their cgroups and the environments from the pool, so those are there with
// the daemon down; what holds a box awake and what is wrong it asks the
// daemon. It doesn't ask for GET /disk, which sizes every environment.
func (n node) top(ctx context.Context, cl *api.Client, now time.Time, out io.Writer) error {
	boxes, err := n.boxes()
	if err != nil {
		return err
	}
	// What is wrong comes first: it also says whether the daemon answers,
	// which the boxes' holds need.
	ctxP, cancel := context.WithTimeout(ctx, 5*time.Second)
	problems, daemonErr := cl.Problems(ctxP)
	cancel()
	holds := make(map[string]api.Holds)
	asked := make(map[string]bool) // by environment: one call answers for both its boxes
	for _, b := range boxes {
		if daemonErr != nil || asked[envOf(b.id)] {
			continue
		}
		asked[envOf(b.id)] = true
		// An actor in the middle of a change answers after it: the row then
		// has no holds, rather than top waiting.
		ctxH, cancel := context.WithTimeout(ctx, 2*time.Second)
		hs, err := cl.Holds(ctxH, envOf(b.id))
		cancel()
		if err != nil {
			continue
		}
		for _, h := range hs {
			holds[h.Box] = h
		}
	}

	if len(boxes) == 0 {
		if _, err := fmt.Fprintln(out, "no box is up"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "ENV\tBOX\tUP\tMEMORY\tCPU\tHELD BY") // tabwriter reports errors on Flush
		for _, b := range boxes {
			kind := "services"
			if envOf(b.id) != b.id {
				kind = "agent"
			}
			up := "?"
			if !b.since.IsZero() {
				up = brief(now.Sub(b.since))
			}
			held := "?"
			if h, ok := holds[b.id]; ok {
				held = heldBy(h, now)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f MiB\t%s\t%s\n", envOf(b.id), kind, up, float64(b.memory)/(1<<20), brief(b.cpu), held)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}

	records, err := filepath.Glob(filepath.Join(n.pool, "envs", "*", "env.json"))
	if err != nil {
		return err
	}
	findings := []finding{
		{name: "environments", ok: true, detail: fmt.Sprintf("%d, and %d boxes up", len(records), len(boxes))},
		n.checkPool(),
	}
	if f, ok := n.checkDisk(); ok {
		findings = append(findings, f)
	}
	switch {
	case daemonErr != nil:
		findings = append(findings, finding{
			name: "daemon", detail: "no word of what holds the boxes or of what is wrong: " + daemonErr.Error(),
			fix: "systemctl status fern-daemon; journalctl -u fern-daemon -n 50",
		})
	case len(problems) == 0:
		findings = append(findings, finding{name: "problems", ok: true, detail: "none that the daemon knows of"})
	}
	for _, p := range problems {
		findings = append(findings, ofProblem(p))
	}
	_, err = report(out, findings)
	return err
}

// envOf returns the environment of box: an agent box is its environment's
// ID with ".agent" (store.AgentBox).
func envOf(box string) string { return strings.TrimSuffix(box, ".agent") }

// boxes returns the boxes that are up, by name: each has a cgroup of its
// own under n.cgroup (internal/runsc), which says what it uses; one with no
// process in it has exited and waits to be cleared, and isn't up. A box's
// bundle is written as it starts, which says since when.
func (n node) boxes() ([]upBox, error) {
	entries, err := os.ReadDir(n.cgroup)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // no daemon has run since the node started
	}
	if err != nil {
		return nil, err
	}
	var out []upBox
	for _, e := range entries {
		if !e.IsDir() {
			continue // the parent cgroup's own files
		}
		dir := filepath.Join(n.cgroup, e.Name())
		if procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs")); err != nil || len(strings.TrimSpace(string(procs))) == 0 {
			continue // exited, or stopped meanwhile
		}
		b := upBox{id: e.Name()}
		// Without the page cache, which comes and goes. gVisor keeps what the
		// box's programs allocate in shared memory, so that counts.
		stat := cgroupStat(filepath.Join(dir, "memory.stat"))
		b.memory = stat["anon"] + stat["shmem"]
		b.cpu = time.Duration(cgroupStat(filepath.Join(dir, "cpu.stat"))["usage_usec"]) * time.Microsecond
		if fi, err := os.Stat(filepath.Join(n.bundles, b.id, "config.json")); err == nil {
			b.since = fi.ModTime()
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(x, y upBox) int { return strings.Compare(x.id, y.id) })
	return out, nil
}

// cgroupStat reads a cgroup's file of "name value" lines, such as
// memory.stat; a file that can't be read has no values.
func cgroupStat(path string) map[string]uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	stat := make(map[string]uint64)
	for line := range strings.Lines(string(data)) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		if v, err := strconv.ParseUint(value, 10, 64); err == nil {
			stat[name] = v
		}
	}
	return stat
}

// heldBy says in a few words what holds a box awake at now: what runs in
// it, and when it sleeps and by which rule (fern why has the same).
func heldBy(h api.Holds, now time.Time) string {
	if !h.Awake {
		return "nothing: the daemon has it as asleep"
	}
	var parts []string
	for _, r := range h.Running {
		parts = append(parts, fmt.Sprintf("%s %d (%s)", r.Kind, r.N, brief(now.Sub(r.Since))))
	}
	switch {
	case !h.SleepAt.IsZero():
		parts = append(parts, fmt.Sprintf("sleeps in %s: %s", brief(max(h.SleepAt.Sub(now), 0)), h.Why))
	case len(parts) == 0:
		parts = append(parts, h.Why)
	}
	return strings.Join(parts, ", ")
}

// brief is d in at most two units, for a column: 0.8s, 45s, 12m05s, 3h07m,
// 2d04h.
func brief(d time.Duration) string {
	d = max(d, 0)
	switch s := int64(d / time.Second); {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	default:
		return fmt.Sprintf("%dd%02dh", s/86400, s%86400/3600)
	}
}
