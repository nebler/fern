package run

import (
	"errors"
	"strconv"
	"strings"

	"github.com/nebler/fern/internal/task"
)

// Resources is the canonical resource namespace for a run generation. Its
// spelling is persisted by the current resource contract.
type Resources struct{ stem string }

func NewResources(id task.TaskID, generation int64) (Resources, error) {
	if _, err := task.ParseTaskID(string(id)); err != nil {
		return Resources{}, err
	}
	if generation <= 0 {
		return Resources{}, errors.New("positive run generation is required")
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(id), "tsk_"), "-", "")
	return Resources{stem: "run-" + compact + "-g" + strconv.FormatInt(generation, 10)}, nil
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
