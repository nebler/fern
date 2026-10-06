package task

import "testing"

func TestStateValidation(t *testing.T) {
	t.Parallel()
	for _, state := range allTaskStates {
		if !state.Valid() {
			t.Errorf("task state %s rejected", state)
		}
	}
	for _, state := range allAttemptStates {
		if !state.Valid() {
			t.Errorf("attempt state %s rejected", state)
		}
	}
	for _, state := range allResultStates {
		if !state.Valid() {
			t.Errorf("result state %s rejected", state)
		}
	}
	if TaskState("not_a_state").Valid() || AttemptState("not_a_state").Valid() || ResultState("collecting").Valid() {
		t.Error("unknown states must be invalid")
	}
}
