package runsc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestLastLog(t *testing.T) {
	for _, tc := range []struct{ box, want string }{
		{"t3-12", "/pool/envs/t3-12/last.log"},
		{"t3-12.agent", "/pool/envs/t3-12/last.agent.log"},
		{"golden-app-1a2b3c4d", "/pool/envs/golden-app-1a2b3c4d/last.log"},
	} {
		if got := LastLog("/pool", tc.box); got != tc.want {
			t.Errorf("LastLog(/pool, %s) = %s, want %s", tc.box, got, tc.want)
		}
	}
}

// keepLog keeps the end of a box's log as its environment's, from the start
// of a line and at most lastLogMax of it; a box with nothing to say leaves
// the log before, and a box whose environment is gone leaves nothing.
func TestKeepLog(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n"
	long := "the start, which is cut\n" + strings.Repeat(line, lastLogMax/len(line)+5)
	// The last lastLogMax bytes begin inside a line, which is left out.
	longEnd := long[len(long)-lastLogMax:]
	longEnd = longEnd[strings.IndexByte(longEnd, '\n')+1:]
	// One line longer than all that is kept: no line starts in it.
	endless := strings.Repeat("y", lastLogMax+10)
	for _, tc := range []struct {
		name   string
		log    *string // the box's log; nil: it has none
		before string  // the log kept of the run before; "": none
		noEnv  bool    // the environment's directory is gone
		want   string  // the log kept after; "": none
	}{
		{name: "a short log", log: new("postgres: starting\npostgres: ready\n"), want: "postgres: starting\npostgres: ready\n"},
		{name: "it replaces the one before", log: new("second run\n"), before: "first run\n", want: "second run\n"},
		{name: "only the end of a long one, from a line's start", log: &long, before: "first run\n", want: longEnd},
		{name: "a log of exactly the most is kept whole", log: new(long[len(long)-lastLogMax:]), want: long[len(long)-lastLogMax:]},
		{name: "a line longer than the most is kept cut", log: &endless, want: endless[10:]},
		{name: "no log leaves the one before", before: "first run\n", want: "first run\n"},
		{name: "an empty log leaves the one before", log: new(""), before: "first run\n", want: "first run\n"},
		{name: "no log and none before", want: ""},
		{name: "an environment that is gone gets no directory", log: new("late\n"), noEnv: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := t.TempDir()
			from, to := filepath.Join(pool, "boxes", "demo.agent", "log"), LastLog(pool, "demo.agent")
			dirs := []string{filepath.Dir(from)}
			if !tc.noEnv {
				dirs = append(dirs, filepath.Dir(to))
			}
			for _, dir := range dirs {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.log != nil {
				if err := os.WriteFile(from, []byte(*tc.log), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.before != "" {
				if err := os.WriteFile(to, []byte(tc.before), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Twice: a stop that a crash cut short keeps it again.
			for range 2 {
				if err := keepLog(from, to); err != nil {
					t.Fatalf("keepLog() error: %v", err)
				}
			}
			got, err := os.ReadFile(to)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("kept %d bytes, want %d; they differ:\n%s", len(got), len(tc.want), cmp.Diff(clip(tc.want), clip(string(got))))
			}
			if len(got) > lastLogMax {
				t.Errorf("kept %d bytes, more than the most, %d", len(got), lastLogMax)
			}
			if _, err := os.Stat(to + ".new"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the copy being written is still there: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(to)); tc.noEnv && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("keepLog made the directory of an environment that is gone: %v", err)
			}
		})
	}
}

// clip shortens a log for a diff: its start and its end.
func clip(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:200] + " […] " + s[len(s)-200:]
}

// A box that stops leaves its log's end with its environment, and nothing
// in boxes/; the next stop of the same box replaces it, and a stop of a box
// that isn't there (a sleep repeated) leaves it alone.
func TestSleepKeepsLog(t *testing.T) {
	newFakeNode(t, 0)
	d := newTestDriver(t)
	if err := os.MkdirAll(filepath.Join(d.cfg.Pool, "envs", "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		log  string // what the box printed in this run; "": it isn't there
		want map[string]string
	}{
		{name: "a box's first stop", log: "run 1\n", want: map[string]string{"last.agent.log": "run 1\n"}},
		{name: "a sleep repeated", want: map[string]string{"last.agent.log": "run 1\n"}},
		{name: "its next stop", log: "run 2\n", want: map[string]string{"last.agent.log": "run 2\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := d.layout("demo.agent")
			if tc.log != "" {
				if err := os.MkdirAll(b.data, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(b.data, "log"), []byte(tc.log), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.Sleep(t.Context(), "demo.agent"); err != nil {
				t.Fatalf("Sleep(demo.agent) error: %v", err)
			}
			if _, err := os.Stat(b.data); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s is still there after the stop: %v", b.data, err)
			}
			got := make(map[string]string)
			entries, err := os.ReadDir(filepath.Join(d.cfg.Pool, "envs", "demo"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				data, err := os.ReadFile(filepath.Join(d.cfg.Pool, "envs", "demo", e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				got[e.Name()] = string(data)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("the environment's directory after the stop (-want +got):\n%s", diff)
			}
		})
	}
}
