package actor

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/store"
)

// interfaceMethods returns, by field, the methods of each of struct t's
// fields that are interfaces: what a value of t can ask of what it holds.
func interfaceMethods(t reflect.Type) map[string][]string {
	out := make(map[string][]string)
	for f := range t.Fields() {
		if f.Type.Kind() != reflect.Interface {
			continue
		}
		var names []string
		for m := range f.Type.Methods() {
			names = append(names, m.Name)
		}
		slices.Sort(names)
		out[f.Name] = names
	}
	return out
}

// Each of the Group's parts holds only the store's methods it uses, and the
// Group itself only its own: a part that starts using another goes through
// here, rather than reaching it through a Store that has every method.
func TestPartsHoldOnlyWhatTheyUse(t *testing.T) {
	tests := []struct {
		part reflect.Type
		want map[string][]string
	}{
		{reflect.TypeFor[builder](), map[string][]string{
			"store": {
				"BuildFailure", "BuildSource", "Create", "Delete", "DeleteSnapshot", "Get", "HasSnapshot",
				"List", "PruneGoldens", "SetBuildFailure", "Snapshot", "Sources", "StripSource",
			},
		}},
		{reflect.TypeFor[snapshotter](), map[string][]string{
			"store":    {"DeleteSnapshot", "List", "Prune", "PruneGoldens", "Snapshot"},
			"failures": {"SetSnapshotFailure"},
		}},
		{reflect.TypeFor[reporter](), map[string][]string{
			"store":    {"BuildFailure", "Quota", "Space"},
			"failures": {"SnapshotFailure"},
		}},
		{reflect.TypeFor[prewaker](), map[string][]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.part.Name(), func(t *testing.T) {
			if diff := cmp.Diff(tc.want, interfaceMethods(tc.part)); diff != "" {
				t.Errorf("interfaces held mismatch (-want +got):\n%s", diff)
			}
		})
	}
	t.Run("Group", func(t *testing.T) {
		want := []string{"Create", "Delete", "Get", "HasSnapshot", "List", "Orphans", "Restore"}
		if diff := cmp.Diff(want, interfaceMethods(reflect.TypeFor[Group]())["store"]); diff != "" {
			t.Errorf("Group's store methods mismatch (-want +got):\n%s", diff)
		}
	})
}

// noticeDisk is all a reporter needs of the disk: three methods, where the
// actors' Store has twenty.
type noticeDisk struct{ space store.Space }

func (d noticeDisk) Space() (store.Space, error) { return d.space, nil }

func (noticeDisk) Quota(context.Context, store.Env) (store.Quota, error) { return store.Quota{}, nil }

func (noticeDisk) BuildFailure(context.Context, string) (store.BuildFailure, bool, error) {
	return store.BuildFailure{}, false, nil
}

// A reporter works from its own interfaces alone: a full pool and a box
// that keeps crashing are told, with no Group and no whole store.
func TestReporterAlone(t *testing.T) {
	full := store.Space{Pool: store.Volume{Total: 100 << 30, Free: 1 << 30}, Least: 5 << 30}
	now := time.Now()
	r := reporter{
		store: noticeDisk{space: full},
		image: "box",
		crashing: func(_ context.Context, box string, _ time.Duration) ([]Crashing, error) {
			return []Crashing{{Box: box, Times: crashLimit, Since: now, Last: now, Cause: "out of memory", Retry: now.Add(crashWindow)}}, nil
		},
	}
	got := r.notices(t.Context(), store.Env{ID: "demo", Workspace: t.TempDir()})
	if len(got) != 2 || !strings.Contains(got[0], "no new snapshots") || !strings.Contains(got[1], "out of memory") {
		t.Errorf("notices = %q, want the full pool's, then the crashing box's", got)
	}
}
