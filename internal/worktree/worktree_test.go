package worktree_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/worktree"
)

func TestTop(t *testing.T) {
	tests := []struct {
		name, pool, path string
		want             string
		ok               bool
	}{
		{name: "the top", pool: "/var/lib/fern", path: "/var/lib/fern/worktrees/fern/t3-1", want: "/var/lib/fern/worktrees/fern/t3-1", ok: true},
		{name: "a folder in it", pool: "/var/lib/fern", path: "/var/lib/fern/worktrees/fern/t3-1/cmd/fern", want: "/var/lib/fern/worktrees/fern/t3-1", ok: true},
		{name: "not clean", pool: "/var/lib/fern/", path: "/var/lib/fern//worktrees/fern/./t3-1/", want: "/var/lib/fern/worktrees/fern/t3-1", ok: true},
		{name: "a repository's folder", pool: "/var/lib/fern", path: "/var/lib/fern/worktrees/fern"},
		{name: "worktrees itself", pool: "/var/lib/fern", path: "/var/lib/fern/worktrees"},
		{name: "an environment's own folder", pool: "/var/lib/fern", path: "/var/lib/fern/envs/demo/live/a"},
		{name: "a main repository", pool: "/var/lib/fern", path: "/var/lib/fern/repos/fern/x"},
		{name: "outside the pool", pool: "/var/lib/fern", path: "/home/worktrees/fern/t3-1"},
		{name: "out through ..", pool: "/var/lib/fern", path: "/var/lib/fern/worktrees/../../x/y/z"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := worktree.Top(tc.pool, tc.path)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Top(%q, %q) = %q, %v; want %q, %v", tc.pool, tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestEnvID pins the mapping T3 computes on its own side, fernEnvironmentId
// in images/t3/fern-checkpoints.patch: the IDs below are what that function
// gives, so a change to either side fails here.
func TestEnvID(t *testing.T) {
	want := map[string]string{
		"/var/lib/fern/worktrees/fern/t3-83c08bfd": "t3-fcdf1356e08e5762",
		"/var/lib/fern/worktrees/my-app/feature-x": "t3-448b8d4e306f0553",
		"/srv/pool/worktrees/a/b":                  "t3-efab74ac557feeda",
	}
	got := make(map[string]string, len(want))
	for top := range want {
		got[top] = worktree.EnvID(top)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("EnvID (-want +got):\n%s", diff)
	}
}

func TestRepo(t *testing.T) {
	if got := worktree.Repo("/var/lib/fern/worktrees/my-app/feature-x"); got != "my-app" {
		t.Errorf("Repo = %q, want my-app", got)
	}
}
