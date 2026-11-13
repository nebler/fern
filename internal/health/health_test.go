package health_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

type fakeStore struct {
	envs      []string
	failed    map[string]store.BuildFailure
	snapshots map[string]store.SnapshotFailure // the environments whose last snapshot failed
	space     store.Space
}

func (f fakeStore) List(context.Context) ([]store.Env, error) {
	var out []store.Env
	for _, id := range f.envs {
		out = append(out, store.Env{ID: id})
	}
	return out, nil
}

func (f fakeStore) BuildFailure(_ context.Context, id string) (store.BuildFailure, bool, error) {
	failure, ok := f.failed[id]
	return failure, ok, nil
}

func (f fakeStore) SnapshotFailure(_ context.Context, id string) (store.SnapshotFailure, bool, error) {
	failure, ok := f.snapshots[id]
	return failure, ok, nil
}

func (f fakeStore) Space() (store.Space, error) { return f.space, nil }

// fakeBoxes are the boxes that aren't woken for now.
type fakeBoxes []actor.Crashing

func (f fakeBoxes) Crashing(context.Context) ([]actor.Crashing, error) { return f, nil }

func TestProblems(t *testing.T) {
	roomy := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Least: 5 << 30}
	hour := time.Hour
	tests := []struct {
		name   string
		store  fakeStore
		every  time.Duration // how often backups are due; 0: none set up
		backup string        // the pool's backup.json; "": none yet
		boxes  fakeBoxes     // the boxes that keep crashing
		t3     int           // T3's answer; 0: no T3, -1: it is down
		want   []string      // each problem's name and the start of its detail
		fix    string        // a part of the first problem's fix
	}{
		{name: "nothing wrong", store: fakeStore{envs: []string{"t3-1", "golden-app-12345678"}, space: roomy}, every: hour},
		{name: "T3 answers", store: fakeStore{space: roomy}, t3: http.StatusOK},
		{
			name:  "T3 answers with an error, before a backup that fails",
			store: fakeStore{space: roomy}, every: hour, t3: http.StatusServiceUnavailable,
			backup: `{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","error":"x","good":"2026-11-10T08:00:00Z"}`,
			want:   []string{"t3: T3 doesn't answer at http://", "backups: the last backup failed"},
			fix:    "`systemctl status fern-t3` and `journalctl -u fern-t3` say why; `systemctl restart fern-t3` starts it again",
		},
		{name: "T3 is down", store: fakeStore{space: roomy}, t3: -1, want: []string{"t3: T3 doesn't answer at http://"}},
		{
			name: "a pool that takes no more snapshots",
			store: fakeStore{space: store.Space{
				Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Under: &store.Volume{Path: "/img", Total: 75 << 30, Free: 1 << 30}, Least: 5 << 30,
			}},
			want: []string{"disk: no new snapshots or threads: the pool is almost full: the pool is a file on the server's disk, which has 1.0 GiB free"},
		},
		{
			name: "a builder whose build failed, and not the one that holds a checkout for it",
			store: fakeStore{
				envs: []string{"golden-app-12345678", "golden-app-12345678-src", "golden-web-12345678", "t3-1"},
				failed: map[string]store.BuildFailure{
					"golden-app-12345678":     {Key: "golden-1", Error: ".fern/setup exited with code 1 after 4s; it printed: pnpm: not found\nmore"},
					"golden-app-12345678-src": {Error: "never read"},
					"t3-1":                    {Error: "never read"},
				},
				space: roomy,
			},
			want: []string{"golden build: golden-app-12345678: its threads start without what the setup script and the services' first start make: .fern/setup exited with code 1 after 4s; it printed: pnpm: not found"},
		},
		{
			name:  "an agent box that isn't woken for now, before a failed build",
			store: fakeStore{envs: []string{"golden-app-12345678", "t3-1"}, failed: map[string]store.BuildFailure{"golden-app-12345678": {Error: "x"}}, space: roomy},
			boxes: fakeBoxes{{
				Box: "t3-1.agent", Times: 3, Since: time.Date(2026, 11, 13, 14, 2, 10, 0, time.UTC), Last: time.Date(2026, 11, 13, 14, 4, 0, 0, time.UTC),
				Retry: time.Date(2026, 11, 13, 14, 7, 10, 0, time.UTC), Cause: "wake t3-1.agent: the box exited as it started; its log ends: no such file\nmore",
			}},
			want: []string{
				"box: t3-1.agent keeps crashing: it died or failed to wake 3 times since 14:02 UTC, and its commands fail until 14:07 UTC, " +
					"when the next one tries again. The last time: wake t3-1.agent: the box exited as it started; its log ends: no such file",
				"golden build: golden-app-12345678",
			},
			fix: "`fern logs t3-1` shows what the box last printed and what the daemon said of it; `fern wake t3-1.agent` tries again now",
		},
		{
			name: "a thread whose last snapshot failed, before a failed build",
			store: fakeStore{
				envs:   []string{"golden-app-12345678", "t3-1", "t3-2"},
				failed: map[string]store.BuildFailure{"golden-app-12345678": {Error: "x"}},
				snapshots: map[string]store.SnapshotFailure{"t3-2": {
					Snapshot: "t3-0123", At: time.Date(2026, 11, 13, 14, 2, 0, 0, time.UTC), Error: "snapshot t3-2/t3-0123: btrfs: input/output error\nmore",
				}},
				space: roomy,
			},
			want: []string{
				"snapshot: t3-2: the snapshot of a turn wasn't taken (t3-0123), so a revert to that turn is refused: snapshot t3-2/t3-0123: btrfs: input/output error",
				"golden build: golden-app-12345678",
			},
			fix: "the next turn's snapshot is tried as usual, and one that works clears this; `fern snapshot t3-2 probe` tries one now " +
				"(`fern rmsnap t3-2 probe` removes it again); `fern logs t3-2` has what the daemon said",
		},
		{
			name:  "backups that fail",
			store: fakeStore{space: roomy}, every: hour,
			backup: `{"at":"` + time.Now().UTC().Format(time.RFC3339) + `","error":"put: no route to host","good":"2026-11-10T08:00:00Z"}`,
			want:   []string{"backups: the last backup failed (the last that worked: 2026-11-10 08:00 UTC): put: no route to host"},
		},
		{
			name:  "backups that stopped",
			store: fakeStore{space: roomy}, every: hour,
			backup: `{"at":"2026-11-10T08:00:00Z","good":"2026-11-10T08:00:00Z"}`,
			want:   []string{"backups: no backup since 2026-11-10 08:00 UTC, and they are due every 1h0m0s"},
		},
		{
			name:   "a failed backup where none are set up any more",
			store:  fakeStore{space: roomy},
			backup: `{"at":"2026-11-10T08:00:00Z","error":"x","good":"0001-01-01T00:00:00Z"}`,
		},
		{
			name:  "a result that can't be read",
			store: fakeStore{space: roomy}, every: hour, backup: "{",
			want: []string{"backups: the last backup's result can't be read"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := t.TempDir()
			if tt.backup != "" {
				if err := os.WriteFile(filepath.Join(pool, backup.StatusFile), []byte(tt.backup), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c := health.Checker{Store: tt.store, Pool: pool, BackupEvery: tt.every, Boxes: tt.boxes}
			if tt.t3 != 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/.well-known/t3/environment" {
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(tt.t3)
				}))
				c.T3 = &health.T3{URL: srv.URL, Grace: time.Nanosecond}
				if tt.t3 < 0 {
					srv.Close() // nothing listens
				} else {
					defer srv.Close()
				}
				// A T3 is a problem once it has failed for its grace, which
				// the first look starts.
				if _, err := c.Problems(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := c.Problems(t.Context())
			if err != nil {
				t.Fatalf("Problems() error: %v", err)
			}
			if got == nil {
				t.Error("Problems() = nil, want an empty list: it is JSON on the wire")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Problems() = %+v, want %d", got, len(tt.want))
			}
			var starts []string
			for i, p := range got {
				line := p.Name + ": " + p.Detail
				if p.Fix == "" {
					t.Errorf("%s has no fix", line)
				}
				if strings.Contains(line, "\n") {
					t.Errorf("%q spans lines", line)
				}
				starts = append(starts, line[:min(len(line), len(tt.want[i]))])
			}
			if diff := cmp.Diff(tt.want, starts); diff != "" {
				t.Errorf("Problems() mismatch (-want +got):\n%s", diff)
			}
			if tt.fix != "" && got[0].Fix != tt.fix {
				t.Errorf("the fix of %s = %q, want %q", got[0].Name, got[0].Fix, tt.fix)
			}
		})
	}
}

// A T3 that fails to answer once, as one does while an update restarts it,
// is no problem yet: it is one only after failing for its grace.
func TestT3RestartIsNoProblem(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens
	c := health.Checker{Store: fakeStore{}, Pool: t.TempDir(), T3: &health.T3{URL: srv.URL}}
	for range 2 {
		got, err := c.Problems(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("Problems() within T3's grace = %+v, want none", got)
		}
	}
}
