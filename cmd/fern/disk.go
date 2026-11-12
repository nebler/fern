package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/nebler/fern/internal/api"
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
	for _, p := range d.Parts {
		note := p.Note
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
