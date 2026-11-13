package health_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/backup"
	"github.com/nebler/fern/internal/health"
	"github.com/nebler/fern/internal/store"
)

type fakeStore struct {
	envs   []string
	failed map[string]store.BuildFailure
	space  store.Space
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

func (f fakeStore) Space() (store.Space, error) { return f.space, nil }

func TestProblems(t *testing.T) {
	roomy := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 50 << 30}, Least: 5 << 30}
	hour := time.Hour
	tests := []struct {
		name   string
		store  fakeStore
		every  time.Duration // how often backups are due; 0: none set up
		backup string        // the pool's backup.json; "": none yet
		want   []string      // each problem's name and the start of its detail
	}{
		{name: "nothing wrong", store: fakeStore{envs: []string{"t3-1", "golden-app-12345678"}, space: roomy}, every: hour},
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
			got, err := health.Checker{Store: tt.store, Pool: pool, BackupEvery: tt.every}.Problems(t.Context())
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
		})
	}
}
