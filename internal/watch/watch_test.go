package watch_test

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/watch"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// canWatch skips the test without CAP_SYS_ADMIN, which the daemon has, as
// root: unprivileged fanotify is allowed, but doesn't name the writer. Only
// a privileged group may have an unlimited queue.
func canWatch(t *testing.T) {
	t.Helper()
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_NOTIF|unix.FAN_REPORT_DFID_NAME|unix.FAN_UNLIMITED_QUEUE, unix.O_RDONLY)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		t.Skipf("fanotify: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
}

// TestWatch checks that a watch reports what this process writes, in
// folders it had and folders made since, and not what another process
// writes or what is in node_modules.
func TestWatch(t *testing.T) {
	canWatch(t)
	dir := t.TempDir()
	write(t, dir, "src/a.txt")
	self := os.Getpid()
	w := watch.New(func(pid int) string {
		if pid == self {
			return "agent"
		}
		return "services"
	}, slog.New(slog.DiscardHandler))
	got := make(chan []string, 16)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- w.Watch(ctx, dir, "agent", func(_ context.Context, paths []string) { got <- paths })
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	// The watch has marked its folders once it reports a write.
	ready := time.NewTicker(20 * time.Millisecond)
	defer ready.Stop()
	timeout := time.After(5 * time.Second)
	for waiting := true; waiting; {
		write(t, dir, "src/ready.txt")
		select {
		case <-got:
			waiting = false
		case <-ready.C:
		case <-timeout:
			t.Fatal("the watch reported nothing")
		}
	}
	// Its batch may still be gathering ready.txt's later writes.
	drain(got, 100*time.Millisecond)

	// Another process's write, which must not be reported, comes first.
	sh := exec.CommandContext(t.Context(), "sh", "-c", "echo x >> src/a.txt; echo y > src/other.txt")
	sh.Dir = dir
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("sh: %v: %s", err, out)
	}
	write(t, dir, "src/a.txt")
	write(t, dir, "src/new/deep/b.txt")
	write(t, dir, "node_modules/pkg/index.js")
	want := map[string]bool{"src/a.txt": true, "src/new/deep/b.txt": true}
	seen := make(map[string]bool)
	deadline := time.After(5 * time.Second)
	for len(seen) < len(want) {
		select {
		case paths := <-got:
			for _, p := range paths {
				seen[p] = true
			}
		case <-deadline:
			t.Fatalf("reported %v, want %v", seen, want)
		}
	}
	// What else is reported comes in the same batches or soon after.
	select {
	case paths := <-got:
		for _, p := range paths {
			seen[p] = true
		}
	case <-time.After(200 * time.Millisecond):
	}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("reported (-want +got):\n%s", diff)
	}
}

// drain drops what comes on got until it has been quiet for d.
func drain(got <-chan []string, d time.Duration) {
	for {
		select {
		case <-got:
		case <-time.After(d):
			return
		}
	}
}

func write(t *testing.T, dir, rel string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("x\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// event is a fanotify event as the kernel lays it out, with one DFID_NAME
// record.
func event(mask uint64, pid int32, handle []byte, name string) []byte {
	rec := binary.NativeEndian.AppendUint64(nil, 0) // fsid
	rec = binary.NativeEndian.AppendUint32(rec, uint32(len(handle)))
	rec = binary.NativeEndian.AppendUint32(rec, 1)
	rec = append(append(append(rec, handle...), name...), 0)
	for len(rec)%4 != 0 {
		rec = append(rec, 0)
	}
	info := append([]byte{unix.FAN_EVENT_INFO_TYPE_DFID_NAME, 0}, binary.NativeEndian.AppendUint16(nil, uint16(4+len(rec)))...)
	info = append(info, rec...)
	ev := binary.NativeEndian.AppendUint32(nil, uint32(24+len(info)))
	ev = append(ev, unix.FANOTIFY_METADATA_VERSION, 0)
	ev = binary.NativeEndian.AppendUint16(ev, 24)
	ev = binary.NativeEndian.AppendUint64(ev, mask)
	ev = binary.NativeEndian.AppendUint32(ev, ^uint32(0)) // FAN_NOFD
	ev = binary.NativeEndian.AppendUint32(ev, uint32(pid))
	return append(ev, info...)
}

func TestDecode(t *testing.T) {
	two := append(event(unix.FAN_MODIFY, 7, []byte{1, 2, 3, 4, 5, 6, 7, 8}, "a.txt"), event(unix.FAN_CREATE, 8, []byte{9}, "b")...)
	for _, tc := range []struct {
		name    string
		buf     []byte
		want    int
		wantErr bool
	}{
		{"none", nil, 0, false},
		{"two", two, 2, false},
		{"cut short", two[:len(two)-3], 0, true},
		{"cut in the header", two[:10], 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := watch.Decode(tc.buf)
			if (err != nil) != tc.wantErr || n != tc.want {
				t.Errorf("Decode = %d, %v; want %d, error %v", n, err, tc.want, tc.wantErr)
			}
		})
	}
}

// FuzzDecode checks that no read, however cut or garbled, panics the
// decoder.
func FuzzDecode(f *testing.F) {
	f.Add(event(unix.FAN_MODIFY, 7, []byte{1, 2, 3, 4}, "a.txt"))
	f.Add(event(unix.FAN_CREATE|unix.FAN_ONDIR, 1, nil, ""))
	f.Fuzz(func(_ *testing.T, buf []byte) {
		_, _ = watch.Decode(buf)
	})
}
