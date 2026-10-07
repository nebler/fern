package run

import (
	"testing"

	"github.com/nebler/fern/internal/task"
)

func TestResourcesPreserveNames(t *testing.T) {
	r := NewResources(task.RunID("run_0198d34d-6a50-75fb-b1f2-000000000001"))
	stem := "run-0198d34d6a5075fbb1f2000000000001"
	if !r.Matches(stem+"-clone", "fern-"+stem+"-opencode", "fern-"+stem, stem+"-endpoint") {
		t.Fatalf("names changed: %+v", r)
	}
	if r.Matches(r.Clone(), r.Volume(), r.Container()+"x", r.Endpoint()) {
		t.Fatal("accepted mismatched resource")
	}
	if NewResources("").Matches("", "", "", "") || (Resources{}).Matches("", "", "", "") {
		t.Fatal("accepted zero resources")
	}
}
