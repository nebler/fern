package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nebler/fern/internal/api"
)

// logs prints what the node has logged about one environment: fern logs
// <env>, as root on the node. First the daemon's lines that name it or its
// agent box (what woke it, put it to sleep, snapshotted it, failed), then
// what each of its boxes that runs has printed: its services' output and
// fern-init's. A box's own log goes when the box stops.
// It reads the node's own journal and files, so it works with the daemon
// down, which is when it is needed most.
func logs(ctx context.Context, c command, _ *api.Client, std stdio) (int, error) {
	return 0, thisNode().logs(ctx, c.args[0], c.since, c.lines, std.out)
}

func (n node) logs(ctx context.Context, id, since string, lines int, out io.Writer) error {
	if id == "" || filepath.Base(id) != id || strings.HasPrefix(id, ".") {
		return fmt.Errorf("%q is not an environment's ID", id)
	}
	journal, err := exec.CommandContext(ctx, n.journalctl, "-u", "fern-daemon", "-o", "short-iso", "--no-pager", "--since", since).Output()
	if err != nil {
		return fmt.Errorf("the daemon's journal: %w (fern logs reads it as root, on the node)", err)
	}
	var about []string
	sc := bufio.NewScanner(bytes.NewReader(journal))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if names(sc.Text(), id) {
			about = append(about, sc.Text())
		}
	}
	if _, err := fmt.Fprintf(out, "== the daemon about %s, since %s (%d lines, the last %d shown)\n", id, since, len(about), min(len(about), lines)); err != nil {
		return err
	}
	for _, line := range about[max(len(about)-lines, 0):] {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	for _, box := range []string{id, id + ".agent"} {
		path := filepath.Join(n.pool, "boxes", box, "log")
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a box that isn't running
		}
		if err != nil {
			return err
		}
		logged := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if _, err := fmt.Fprintf(out, "\n== box %s's own log, %s (the last %d of %d lines)\n", box, path, min(len(logged), lines), len(logged)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, strings.Join(logged[max(len(logged)-lines, 0):], "\n")); err != nil {
			return err
		}
	}
	return nil
}

// names reports whether a line of the daemon's log is about environment id
// or its agent box: one of its values (env=…, source=…, box=…) is the ID,
// whole. t3-12 is not named by a line about t3-123.
func names(line, id string) bool {
	for rest := line; ; {
		i := strings.Index(rest, "="+id)
		if i < 0 {
			return false
		}
		rest = rest[i+1+len(id):]
		if rest == "" || rest[0] == ' ' || strings.HasPrefix(rest, ".agent ") || rest == ".agent" {
			return true
		}
	}
}
