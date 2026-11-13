package main

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/nebler/fern/internal/api"
	"github.com/nebler/fern/internal/store"
)

// diskUsage prints what uses the node's disk: fern df. A pool that is a
// file on the server's disk can only grow into what that disk has free, so
// the disk comes first.
func diskUsage(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	d, err := cl.Disk(ctx)
	if err != nil {
		return 0, err
	}
	tw := tabwriter.NewWriter(std.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\tSIZE\tFREE\t") // tabwriter reports errors on Flush
	if d.Under != nil {
		_, _ = fmt.Fprintf(tw, "the server's disk\t%s\t%s\t%s%s\n", size(d.Under.Total), size(d.Under.Free), d.Under.Path, low(d.Under.Free, d.Least))
	}
	_, _ = fmt.Fprintf(tw, "the pool\t%s\t%s\t%s%s\n", size(d.Pool.Total), size(d.Pool.Free), d.Pool.Path, low(d.Pool.Free, d.Least))
	if err := tw.Flush(); err != nil {
		return 0, err
	}
	if d.Under != nil {
		if _, err := fmt.Fprintln(std.out, "The pool is a file on the server's disk and grows into it: what the disk has free is what the pool can still take."); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintln(std.out); err != nil {
		return 0, err
	}
	tw = tabwriter.NewWriter(std.out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WHAT\tSIZE\tONLY ITS\t")
	var unused uint64
	limited := false
	for _, p := range d.Parts {
		note := p.Note
		if p.Limit > 0 {
			limited = true
			note = strings.TrimSuffix(fmt.Sprintf("wrote %s of its limit of %s%s; %s", size(p.Written), size(p.Limit), full(p.Written, p.Limit), note), "; ")
		}
		if p.Unused {
			unused += p.Exclusive
			note = "fern gc removes it: " + note
		}
		_, _ = fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", p.Kind, p.Name, size(p.Total), size(p.Exclusive), note)
	}
	if err := tw.Flush(); err != nil {
		return 0, err
	}
	_, err = fmt.Fprintf(std.out, "\nSIZE counts blocks shared with others (a workspace with its snapshots, a thread with its golden); ONLY ITS is the least that removing it frees.\n"+
		"fern gc would free at least %s.\n", size(unused))
	if err == nil && limited {
		_, err = fmt.Fprintln(std.out, "What a thread wrote and still holds, its snapshots included, counts against its disk limit; at the limit, writes in its box fail. `fern limit ENV SIZE` changes it.")
	}
	return 0, err
}

// full marks an environment at its disk limit, as the store tells it
// (store.Quota.Full).
func full(written, limit uint64) string {
	if (store.Quota{Used: written, Limit: limit}).Full() {
		return ", AT THE LIMIT: writes in its box fail"
	}
	return ""
}

// setLimit sets an environment's disk limit: fern limit <env> <size>, or
// none.
func setLimit(ctx context.Context, c command, cl *api.Client, std stdio) (int, error) {
	var bytes uint64
	if c.args[1] != "none" {
		var err error
		if bytes, err = store.ParseSize(c.args[1]); err != nil || bytes == 0 {
			return 2, fmt.Errorf("size %q: want a size such as 20G, or none", c.args[1])
		}
	}
	if err := cl.SetLimit(ctx, c.args[0], bytes); err != nil {
		return 0, err
	}
	if bytes == 0 {
		_, err := fmt.Fprintf(std.out, "%s has no disk limit now\n", c.args[0])
		return 0, err
	}
	_, err := fmt.Fprintf(std.out, "%s's disk limit is %s now\n", c.args[0], size(bytes))
	return 0, err
}

// low marks a volume with less free than the daemon refuses new snapshots
// and environments at.
func low(free, least uint64) string {
	if free >= least {
		return ""
	}
	return fmt.Sprintf("  ALMOST FULL: under %s free, new snapshots and threads are refused", size(least))
}

// collect removes what nothing on the node needs: fern gc.
func collect(ctx context.Context, _ command, cl *api.Client, std stdio) (int, error) {
	freed, err := cl.GC(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range freed.Removed {
		if _, err := fmt.Fprintln(std.out, "removed", r); err != nil {
			return 0, err
		}
	}
	if len(freed.Removed) == 0 {
		if _, err := fmt.Fprintln(std.out, "nothing to remove but snapshots past their retention, if any"); err != nil {
			return 0, err
		}
	}
	for _, f := range freed.Failed {
		if _, err := fmt.Fprintln(std.err, "fern gc:", f); err != nil {
			return 0, err
		}
	}
	if freed.Under != nil {
		if _, err := fmt.Fprintf(std.out, "the server's disk has %s free of %s\n", size(freed.Under.Free), size(freed.Under.Total)); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(std.out, "the pool has %s free of %s\n", size(freed.Pool.Free), size(freed.Pool.Total)); err != nil {
		return 0, err
	}
	if len(freed.Failed) > 0 {
		return 1, nil
	}
	return 0, nil
}

// size formats bytes for a person.
func size(bytes uint64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%d KiB", bytes>>10)
	}
}
