package domain

import (
	"strings"
)

// Resources is the canonical resource namespace for a run. Its spelling is
// persisted by the current resource contract.
type Resources struct{ stem string }

// NewResources derives the names from a Fern-generated run ID.
func NewResources(id RunID) Resources {
	if id == "" {
		return Resources{}
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(id), "run_"), "-", "")
	return Resources{stem: "run-" + compact}
}

func (r Resources) Clone() string {
	if r.stem == "" {
		return ""
	}
	return r.stem + "-clone"
}
func (r Resources) Volume() string {
	if r.stem == "" {
		return ""
	}
	return "fern-" + r.stem + "-opencode"
}
func (r Resources) Container() string {
	if r.stem == "" {
		return ""
	}
	return "fern-" + r.stem
}
func (r Resources) Endpoint() string {
	if r.stem == "" {
		return ""
	}
	return r.stem + "-endpoint"
}
func (r Resources) Matches(clone, volume, container, endpoint string) bool {
	return r.stem != "" && clone == r.Clone() && volume == r.Volume() && container == r.Container() && endpoint == r.Endpoint()
}
