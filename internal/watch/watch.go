// Package watch reports the files one box writes in a workspace, as the host
// sees them.
//
// gVisor tells a box only of its own changes: a dev server in an
// environment's box never hears of the files Claude writes from its agent
// box, so it doesn't reload. The host sees every write, and fanotify names the
// process that made it, whose cgroup names its box. The daemon watches the
// workspace while the agent box is awake and passes the agent's changes on
// to the services' box (boxexec.TouchPath).
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// batchDelay is how long changes gather before they are reported: an edit
// is a few writes, and a dev server reloads once for a batch.
const batchDelay = 20 * time.Millisecond

// mask is what the watch hears: the writes, and what changes the folders
// it watches.
const mask = unix.FAN_MODIFY | unix.FAN_CREATE | unix.FAN_DELETE | unix.FAN_MOVED_FROM | unix.FAN_MOVED_TO |
	unix.FAN_ONDIR | unix.FAN_EVENT_ON_CHILD

// skip says whether a folder isn't watched: dev servers don't watch these,
// and they hold most of a workspace's folders.
func skip(name string) bool { return name == ".git" || name == "node_modules" }

// Watcher watches workspaces.
type Watcher struct {
	boxOf func(pid int) string
	log   *slog.Logger
}

// New returns a Watcher that learns which box a process runs in from boxOf
// ("" for none), such as runsc.BoxOf.
func New(boxOf func(pid int) string, log *slog.Logger) *Watcher {
	return &Watcher{boxOf: boxOf, log: log}
}

// Watch calls changed with the files under dir that box created, wrote or
// renamed into place, as paths relative to dir, until ctx is done or reading
// the changes fails, whose error it returns. Changes
// within batchDelay come in one call; changed runs on Watch's goroutine.
// Files in skipped folders (.git, node_modules) are left out.
func (w *Watcher) Watch(ctx context.Context, dir, box string, changed func(ctx context.Context, paths []string)) error {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|unix.FAN_REPORT_DFID_NAME,
		unix.O_RDONLY|unix.O_LARGEFILE)
	if err != nil {
		return fmt.Errorf("watch %s: fanotify: %w", dir, err)
	}
	// Non-blocking, so the poller owns it and Close ends a Read.
	f := os.NewFile(uintptr(fd), "fanotify")
	t := &tree{fd: fd, root: dir, dirs: make(map[string]string)}
	if _, err := t.add("."); err != nil {
		_ = f.Close()
		return fmt.Errorf("watch %s: %w", dir, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	events := make(chan []event)
	var reading sync.WaitGroup
	var readErr error
	reading.Go(func() {
		defer cancel()
		readErr = read(ctx, f, events)
	})
	w.report(ctx, t, box, events, changed)
	cancel()
	_ = f.Close() // ends the read
	reading.Wait()
	if readErr != nil {
		return fmt.Errorf("watch %s: %w", dir, readErr)
	}
	return nil
}

// report applies the events, and calls changed with box's changes a batch
// at a time, until ctx is done.
func (w *Watcher) report(ctx context.Context, t *tree, box string, events <-chan []event, changed func(ctx context.Context, paths []string)) {
	pending := make(map[string]bool)
	batch := time.NewTimer(0)
	batch.Stop()
	defer batch.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case evs := <-events:
			before := len(pending)
			t.apply(evs, w.writers(box), pending, w.log)
			if before == 0 && len(pending) > 0 {
				batch.Reset(batchDelay)
			}
		case <-batch.C:
			paths := make([]string, 0, len(pending))
			for p := range pending {
				paths = append(paths, p)
			}
			clear(pending)
			slices.Sort(paths)
			changed(ctx, paths)
		}
	}
}

// writers returns a function that says whether a process runs in box. It
// remembers its answers: one is used for one read's events, too soon after
// for a PID to be reused.
func (w *Watcher) writers(box string) func(pid int) bool {
	seen := make(map[int]bool)
	return func(pid int) bool {
		is, ok := seen[pid]
		if !ok {
			is = w.boxOf(pid) == box
			seen[pid] = is
		}
		return is
	}
}

// read passes the events read from f to events until ctx is done or a
// read fails.
func read(ctx context.Context, f *os.File, events chan<- []event) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			return err
		}
		evs, err := decode(buf[:n])
		if err != nil {
			return err
		}
		select {
		case events <- evs:
		case <-ctx.Done():
			return nil
		}
	}
}

// tree is the folders a watch has marked, by file handle.
type tree struct {
	fd   int
	root string
	dirs map[string]string // file handle → folder, relative to root
}

// apply adds to pending the files the events say mine changed, and marks
// the folders they made.
func (t *tree) apply(evs []event, mine func(pid int) bool, pending map[string]bool, log *slog.Logger) {
	for _, e := range evs {
		if e.mask&unix.FAN_Q_OVERFLOW != 0 {
			log.Warn("a workspace's changes overflowed; some are lost", slog.String("dir", t.root))
			continue
		}
		parent, ok := t.dirs[e.handle]
		if !ok || e.name == "" || e.name == "." {
			continue
		}
		rel := path.Join(parent, e.name)
		if e.mask&unix.FAN_ONDIR == 0 {
			if mine(e.pid) && e.mask&(unix.FAN_MODIFY|unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0 {
				pending[rel] = true
			}
			continue
		}
		switch {
		case e.mask&(unix.FAN_DELETE|unix.FAN_MOVED_FROM) != 0:
			t.forget(rel)
		case e.mask&(unix.FAN_CREATE|unix.FAN_MOVED_TO) != 0 && !skip(e.name):
			// What was written in it before its mark is found by walking it.
			files, err := t.add(rel)
			if err != nil {
				log.Warn("watching a new folder failed", slog.String("dir", filepath.Join(t.root, rel)), slog.Any("error", err))
			}
			if mine(e.pid) {
				for _, f := range files {
					pending[f] = true
				}
			}
		}
	}
}

// add marks folder rel and the folders under it, and returns the files in
// them. A folder that is gone by the time it is marked is left out.
func (t *tree) add(rel string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(filepath.Join(t.root, rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		r, err := filepath.Rel(t.root, p)
		if err != nil {
			return err
		}
		r = filepath.ToSlash(r)
		if !d.IsDir() {
			if d.Type().IsRegular() {
				files = append(files, r)
			}
			return nil
		}
		if r != "." && skip(d.Name()) {
			return filepath.SkipDir
		}
		if err := unix.FanotifyMark(t.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_ONLYDIR|unix.FAN_MARK_DONT_FOLLOW, mask, unix.AT_FDCWD, p); err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
				return filepath.SkipDir
			}
			return fmt.Errorf("mark %s: %w", p, err)
		}
		h, _, err := unix.NameToHandleAt(unix.AT_FDCWD, p, 0)
		if err != nil {
			return fmt.Errorf("handle of %s: %w", p, err)
		}
		t.dirs[handleKey(h.Type(), h.Bytes())] = r
		return nil
	})
	return files, err
}

// forget drops folder rel and those under it. A folder renamed within the
// workspace is found again under its new name (add), with its marks.
func (t *tree) forget(rel string) {
	for h, d := range t.dirs {
		if d == rel || strings.HasPrefix(d, rel+"/") {
			delete(t.dirs, h)
		}
	}
}
