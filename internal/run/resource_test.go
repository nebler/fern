package run

import (
	"github.com/nebler/fern/internal/task"
	"testing"
)

func TestResourcesPreserveNames(t *testing.T) {
	id := task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000001")
	for _, generation := range []int64{1, 12} {
		r, err := NewResources(id, generation)
		if err != nil {
			t.Fatal(err)
		}
		stem := "run-0198d34d6a5075fbb1f2000000000001-g1"
		if generation == 12 {
			stem += "2"
		}
		if !r.Matches(stem+"-clone", "fern-"+stem+"-opencode", "fern-"+stem, stem+"-endpoint") {
			t.Fatalf("names changed: %+v", r)
		}
		if r.Matches(r.Clone(), r.Volume(), r.Container()+"x", r.Endpoint()) {
			t.Fatal("accepted mismatched resource")
		}
	}
	for _, generation := range []int64{0, -1} {
		if _, err := NewResources(id, generation); err == nil {
			t.Fatal("accepted invalid generation")
		}
	}
	if _, err := NewResources("invalid", 1); err == nil {
		t.Fatal("accepted invalid task")
	}
	if (Resources{}).Matches("", "", "", "") {
		t.Fatal("accepted zero resources")
	}
}
