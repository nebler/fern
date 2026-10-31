package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/boxexec"
)

// attribs returns the names of the files whose attributes changed under the
// folders fd watches since the last call, as an inotify watcher hears them.
func attribs(t *testing.T, fd int) []string {
	t.Helper()
	var names []string
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			slices.Sort(names)
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		for b := buf[:n]; len(b) >= unix.SizeofInotifyEvent; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&b[0]))
			name := strings.TrimRight(string(b[unix.SizeofInotifyEvent:unix.SizeofInotifyEvent+int(ev.Len)]), "\x00")
			if ev.Mask&unix.IN_ATTRIB != 0 {
				names = append(names, name)
			}
			b = b[unix.SizeofInotifyEvent+int(ev.Len):]
		}
	}
}

func TestTouch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	a, outside := filepath.Join(dir, "src", "a.txt"), filepath.Join(root, "outside.txt")
	old := time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)
	for _, f := range []string{a, outside} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	for _, d := range []string{root, dir, filepath.Join(dir, "src")} {
		if _, err := unix.InotifyAddWatch(fd, d, unix.IN_ATTRIB); err != nil {
			t.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("POST "+boxexec.TouchPath, touch("tok", dir))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	if err := boxexec.Touch(t.Context(), srv.Client(), addr, "wrong", []string{"src/a.txt"}); err == nil {
		t.Error("a touch with the wrong token succeeded")
	}
	if got := attribs(t, fd); len(got) > 0 {
		t.Errorf("a refused touch changed %v", got)
	}
	// Only src/a.txt is a file in the workspace.
	paths := []string{"src/a.txt", "missing.txt", "src", "link", "../outside.txt", outside}
	if err := boxexec.Touch(t.Context(), srv.Client(), addr, "tok", paths); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"a.txt"}, attribs(t, fd)); diff != "" {
		t.Errorf("touched (-want +got):\n%s", diff)
	}
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("src/a.txt's mtime = %v, want it kept at %v", fi.ModTime(), old)
	}
	if err := boxexec.Touch(t.Context(), srv.Client(), addr, "tok", nil); err != nil {
		t.Errorf("an empty touch: %v", err)
	}
}
