package store_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/nebler/fern/internal/store"
)

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
		bad  bool
	}{
		{in: "0", want: 0},
		{in: "4096", want: 4096},
		{in: "512K", want: 512 << 10},
		{in: "20G", want: 20 << 30},
		{in: "20GiB", want: 20 << 30},
		{in: "20GB", want: 20 << 30},
		{in: "1T", want: 1 << 40},
		{in: "300m", want: 300 << 20},
		{in: "", bad: true},
		{in: "G", bad: true},
		{in: "-1G", bad: true},
		{in: "1.5G", bad: true},
		{in: "20X", bad: true},
		{in: "99999999999T", bad: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := store.ParseSize(tc.in)
			if tc.bad {
				if err == nil {
					t.Errorf("ParseSize(%q) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ParseSize(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestQuotaNearFull(t *testing.T) {
	for _, tc := range []struct {
		q          store.Quota
		near, full bool
	}{
		{q: store.Quota{Group: 1, Used: 50 << 30}},
		{q: store.Quota{Group: 1, Used: 5 << 30, Limit: 10 << 30}},
		{q: store.Quota{Group: 1, Used: 9 << 30, Limit: 10 << 30}, near: true},
		{q: store.Quota{Group: 1, Used: 10<<30 - 64<<10, Limit: 10 << 30}, near: true, full: true},
		{q: store.Quota{Group: 1, Used: 11 << 30, Limit: 10 << 30}, near: true, full: true},
	} {
		if got := [2]bool{tc.q.Near(), tc.q.Full()}; got != [2]bool{tc.near, tc.full} {
			t.Errorf("%+v: Near, Full = %v, want %v", tc.q, got, [2]bool{tc.near, tc.full})
		}
	}
}

// commit commits what was written: a group counts it at the commit.
func commit(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Syncfs(int(f.Fd())); err != nil {
		t.Fatal(err)
	}
}

func quota(t *testing.T, st *store.Store, id string) store.Quota {
	t.Helper()
	env, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	q, err := st.Quota(t.Context(), env)
	if err != nil {
		t.Fatalf("Quota(%s) error: %v", id, err)
	}
	return q
}

// written returns how much environment id's group counts after a write of
// n bytes into dir, which is removed again.
func written(t *testing.T, st *store.Store, id, dir string, n int) uint64 {
	t.Helper()
	commit(t, dir)
	before := quota(t, st, id).Used
	probe := filepath.Join(dir, ".quota-probe")
	if err := os.WriteFile(probe, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	commit(t, dir)
	after := quota(t, st, id).Used
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	commit(t, dir)
	return after - before
}

// checkGroups checks that every environment has a quota group and that its
// workspace is in it: what is written there counts.
func checkGroups(t *testing.T, st *store.Store) {
	t.Helper()
	envs, err := st.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range envs {
		if env.Group == 0 {
			t.Errorf("environment %s has no quota group", env.ID)
			continue
		}
		if got := written(t, st, env.ID, env.Workspace, 1<<20); got < 1<<20 {
			t.Errorf("environment %s: a write of 1 MiB in its workspace counts %d bytes in its group, want it all: the workspace isn't in it", env.ID, got)
		}
	}
}

// groupExists reports whether the pool has quota group 1/n.
func groupExists(t *testing.T, st *store.Store, n uint64) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(store.QgroupsDir(st), "1_"+strconv.FormatUint(n, 10)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestQuotaWithoutLimits(t *testing.T) {
	st, _ := openPool(t)
	env := create(t, st, "demo", store.CreateOptions{})
	if env.Group != 0 {
		t.Errorf("Group = %d without limits, want 0", env.Group)
	}
	if q := quota(t, st, "demo"); q != (store.Quota{}) {
		t.Errorf("Quota(demo) = %+v without limits, want none", q)
	}
	if err := st.SetLimit(t.Context(), "demo", 1<<30); !errors.Is(err, store.ErrNoLimit) {
		t.Errorf("SetLimit(demo) error = %v, want %v", err, store.ErrNoLimit)
	}
}

// TestLimit fills an environment to its limit: the write fails with
// EDQUOT, the daemon can still snapshot, and a higher limit lets it write.
func TestLimit(t *testing.T) {
	const limit = 16 << 20
	st, _ := openPoolWith(t, limit)
	env := create(t, st, "demo", store.CreateOptions{})
	if q := quota(t, st, "demo"); q.Group != env.Group || q.Group == 0 || q.Limit != limit {
		t.Fatalf("Quota(demo) = %+v, want group %d and a limit of %d", q, env.Group, limit)
	}
	big := filepath.Join(env.Workspace, "big")
	if err := os.WriteFile(big, make([]byte, 2*limit), 0o644); !errors.Is(err, unix.EDQUOT) {
		t.Fatalf("writing twice the limit: error = %v, want EDQUOT", err)
	}
	commit(t, env.Workspace)
	if q := quota(t, st, "demo"); !q.Full() {
		t.Errorf("Quota(demo) = %+v after the failed write, want it full", q)
	}
	// The next turn's snapshot and a revert still work.
	snapshot(t, st, "demo", "full")
	if err := st.Restore(t.Context(), "demo", "full"); err != nil {
		t.Fatalf("Restore(demo, full) at the limit: %v", err)
	}
	if err := st.SetLimit(t.Context(), "demo", 4*limit); err != nil {
		t.Fatalf("SetLimit(demo) error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.Workspace, "more"), make([]byte, limit), 0o644); err != nil {
		t.Errorf("writing after the limit was raised: %v", err)
	}
	if err := st.SetLimit(t.Context(), "demo", 0); err != nil {
		t.Fatalf("SetLimit(demo, 0) error: %v", err)
	}
	if q := quota(t, st, "demo"); q.Limit != 0 {
		t.Errorf("Quota(demo) = %+v after the limit was removed, want none", q)
	}
}

// TestLimitGroups checks which group each subvolume an environment gets
// counts in: a restore's workspace its own, a fork's its own, and a
// builder's has no limit.
func TestLimitGroups(t *testing.T) {
	st, _ := openPoolWith(t, testLimit)
	env := create(t, st, "demo", store.CreateOptions{})
	snapshot(t, st, "demo", "s1")
	if err := st.Restore(t.Context(), "demo", "s1"); err != nil {
		t.Fatal(err)
	}
	if got := written(t, st, "demo", env.Workspace, 1<<20); got < 1<<20 {
		t.Errorf("a write in the restored workspace counts %d bytes, want 1 MiB", got)
	}
	fork := create(t, st, "demo-fork-1", store.CreateOptions{Fork: &store.SnapshotRef{Env: "demo"}})
	if fork.Group == 0 || fork.Group == env.Group {
		t.Errorf("fork's group = %d, the source's %d: want one of its own", fork.Group, env.Group)
	}
	if got := written(t, st, "demo-fork-1", fork.Workspace, 1<<20); got < 1<<20 {
		t.Errorf("a write in the fork counts %d bytes in its group, want 1 MiB", got)
	}
	builder := create(t, st, store.BuilderID("repo"), store.CreateOptions{})
	q := quota(t, st, builder.ID)
	if q.Group == 0 || q.Limit != 0 {
		t.Errorf("Quota(builder) = %+v, want a group without a limit", q)
	}
	limits := map[string]uint64{}
	for _, id := range []string{"demo", "demo-fork-1", builder.ID} {
		limits[id] = quota(t, st, id).Limit
	}
	want := map[string]uint64{"demo": testLimit, "demo-fork-1": testLimit, builder.ID: 0}
	if diff := cmp.Diff(want, limits); diff != "" {
		t.Errorf("limits (-want +got):\n%s", diff)
	}
	// A deleted environment's group goes, now or at a later ClearGroups:
	// the kernel keeps it while the cleaner hasn't dropped its subvolumes,
	// and on kernels before 6.17 for good.
	if err := st.Delete(t.Context(), "demo-fork-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClearGroups(t.Context()); err != nil {
		t.Errorf("ClearGroups() error: %v", err)
	}
	if got := quota(t, st, "demo").Group; got != env.Group {
		t.Errorf("demo's group = %d after ClearGroups, want %d", got, env.Group)
	}
}

// TestClearGroups checks that a group a create made, and stopped before
// any subvolume joined, is destroyed by Reconcile: on any kernel, as it
// never had a member.
func TestClearGroups(t *testing.T) {
	st, _ := openPoolWith(t, testLimit)
	store.SetFailAt(st, func(step string) error {
		if step == "create: marked" {
			return errCrash
		}
		return nil
	})
	if _, err := st.Create(t.Context(), "demo", store.CreateOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("Create() error = %v, want %v", err, errCrash)
	}
	store.SetFailAt(st, nil)
	// The marker names the group.
	groups, err := os.ReadDir(store.QgroupsDir(st))
	if err != nil {
		t.Fatal(err)
	}
	var made uint64
	for _, g := range groups {
		var n uint64
		if _, err := fmt.Sscanf(g.Name(), "1_%d", &n); err == nil {
			made = max(made, n)
		}
	}
	if made == 0 || !groupExists(t, st, made) {
		t.Fatalf("no group made")
	}
	if lost, err := st.Reconcile(t.Context()); err != nil || len(lost) != 0 {
		t.Fatalf("Reconcile() = %v, %v", lost, err)
	}
	if groupExists(t, st, made) {
		t.Errorf("group 1/%d is there after Reconcile, want it destroyed", made)
	}
}

// TestBringIn checks that environments made without limits get a group at
// the first start with them.
func TestBringIn(t *testing.T) {
	st, _ := openPool(t)
	env := create(t, st, "demo", store.CreateOptions{})
	if err := os.WriteFile(filepath.Join(env.Workspace, "old"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot(t, st, "demo", "s1")
	if err := st.EnableLimits(t.Context(), testLimit); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableLimits(t.Context(), testLimit); err != nil {
		t.Errorf("EnableLimits() a second time: %v", err)
	}
	if lost, err := st.Reconcile(t.Context()); err != nil || len(lost) != 0 {
		t.Fatalf("Reconcile() = %v, %v", lost, err)
	}
	checkGroups(t, st)
	// Whether the 4 MiB count depends on whether quotas were on when they
	// were written, as an earlier test can have turned them on.
	if q := quota(t, st, "demo"); q.Limit != testLimit {
		t.Errorf("Quota(demo) = %+v, want a limit of %d", q, testLimit)
	}
}

// TestClearGroupsAfterDelete checks that a deleted environment's group,
// which held a workspace and a snapshot, is destroyed once btrfs's cleaner
// has dropped them. Kernels before 6.17's "fix squota compressed stats
// leak" refuse it for good (lab/spike/squota/RESULTS.md): there it is
// logged, not failed, as Fern leaves such a group.
func TestClearGroupsAfterDelete(t *testing.T) {
	st, dir := openPoolWith(t, testLimit)
	env := create(t, st, "demo", store.CreateOptions{})
	if err := os.WriteFile(filepath.Join(env.Workspace, "f"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot(t, st, "demo", "s1")
	if err := st.Delete(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "btrfs", "subvolume", "sync", dir).CombinedOutput(); err != nil {
		t.Fatalf("btrfs subvolume sync: %v: %s", err, out)
	}
	if _, err := st.ClearGroups(t.Context()); err != nil {
		t.Fatalf("ClearGroups() error: %v", err)
	}
	if !groupExists(t, st, env.Group) {
		return
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		t.Fatal(err)
	}
	release := unix.ByteSliceToString(uts.Release[:])
	var major, minor int
	if _, err := fmt.Sscanf(release, "%d.%d", &major, &minor); err != nil {
		t.Fatalf("kernel release %q: %v", release, err)
	}
	out, _ := exec.CommandContext(t.Context(), "btrfs", "qgroup", "show", "-pcre", "--raw", dir).CombinedOutput()
	t.Logf("btrfs qgroup show:\n%s", out)
	if major > 6 || major == 6 && minor >= 17 {
		t.Errorf("group 1/%d is there after its environment was deleted and the cleaner dropped it, on Linux %s", env.Group, release)
	} else {
		t.Logf("group 1/%d stays on Linux %s, which lacks the fix (6.17)", env.Group, release)
	}
}
