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
// what each of its boxes has printed: its services' output and fern-init's.
// For a box that isn't running that is the log of its last run, whose end
// its stop kept in the environment's directory: a box that crashed, or
// failed to wake, says why there.
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
		title, path, data, err := n.boxLog(id, box)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a box that has never run, or an environment that is gone
		}
		if err != nil {
			return err
		}
		logged := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if _, err := fmt.Fprintf(out, "\n== %s, %s (the last %d of %d lines)\n", title, path, min(len(logged), lines), len(logged)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, strings.Join(logged[max(len(logged)-lines, 0):], "\n")); err != nil {
			return err
		}
	}
	return nil
}

// boxLog reads what box of environment id has printed, and says what it is
// and where: the log of the box that runs (boxes/<box>/log in the pool), or
// else that of its last run, whose end its stop kept in the environment's
// directory (runsc.LastLog). A box with neither is fs.ErrNotExist.
func (n node) boxLog(id, box string) (title, path string, data []byte, err error) {
	last := filepath.Join(n.pool, "envs", id, "last"+strings.TrimPrefix(box, id)+".log")
	kept, lastErr := os.Stat(last)
	path = filepath.Join(n.pool, "boxes", box, "log")
	if data, err = os.ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
		title = fmt.Sprintf("box %s's own log", box)
		if lastErr == nil {
			// Why it stopped the last time, such as a crash, is in that one.
			title += " (its last run's: " + last + ")"
		}
		return title, path, data, err
	}
	if lastErr != nil {
		return "", "", nil, lastErr
	}
	title = fmt.Sprintf("box %s isn't running: the log of its last run, kept when it stopped (%s)",
		box, kept.ModTime().UTC().Format("2006-01-02 15:04 UTC"))
	data, err = os.ReadFile(last)
	return title, last, data, err
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
