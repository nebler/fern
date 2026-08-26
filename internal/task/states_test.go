package task

import (
	"errors"
	"testing"
)

func TestTaskTransitions(t *testing.T) {
	testTransitions(t, allTaskStates, map[TaskState][]TaskState{
		TaskQueued:           {TaskRunning, TaskCancelRequested, TaskUncertain, TaskRecoveryRequired, TaskFailed},
		TaskRunning:          {TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskUncertain, TaskRecoveryRequired},
		TaskInputRequired:    {TaskRunning, TaskCancelRequested, TaskFailed, TaskUncertain, TaskRecoveryRequired},
		TaskCancelRequested:  {TaskCanceled, TaskUncertain, TaskRecoveryRequired},
		TaskUncertain:        {TaskQueued, TaskRunning, TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskCanceled, TaskRecoveryRequired},
		TaskRecoveryRequired: {TaskQueued, TaskRunning, TaskCancelRequested, TaskFailed, TaskCanceled},
	}, AllowTaskTransition)
	testTerminal(t, allTaskStates, map[TaskState]bool{TaskCompleted: true, TaskFailed: true, TaskCanceled: true}, func(s TaskState) bool { return s.Terminal() })
}

func TestAttemptTransitions(t *testing.T) {
	testTransitions(t, allAttemptStates, map[AttemptState][]AttemptState{
		AttemptPrepared:         {AttemptDelivering, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed},
		AttemptDelivering:       {AttemptAdmitted, AttemptUncertain, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed},
		AttemptAdmitted:         {AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptRunning:          {AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptInputRequired:    {AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
		AttemptCancelRequested:  {AttemptCanceled, AttemptUncertain, AttemptRecoveryRequired},
		AttemptUncertain:        {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptCanceled, AttemptRecoveryRequired},
		AttemptRecoveryRequired: {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptCanceled, AttemptSuperseded},
	}, AllowAttemptTransition)
	testTerminal(t, allAttemptStates, map[AttemptState]bool{AttemptSucceeded: true, AttemptFailed: true, AttemptCanceled: true, AttemptSuperseded: true}, func(s AttemptState) bool { return s.Terminal() })
}

func TestResultStateClassification(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		state    ResultState
		valid    bool
		terminal bool
	}{
		{ResultSealed, true, true},
		{ResultFailed, true, true},
		{ResultState("collecting"), false, false},
	} {
		if test.state.Valid() != test.valid || test.state.Terminal() != test.terminal {
			t.Errorf("%s valid=%t terminal=%t", test.state, test.state.Valid(), test.state.Terminal())
		}
	}
}

// The approval, result-transition, verification-transition, and
// publication-transition machines live in taskstore behind SQL triggers and
// are intentionally not modeled here.

func testTransitions[T ~string](t *testing.T, states []T, want map[T][]T, allow func(T, T) error) {
	t.Helper()
	for _, from := range states {
		for _, to := range states {
			expected := false
			for _, candidate := range want[from] {
				expected = expected || candidate == to
			}
			err := allow(from, to)
			if expected && err != nil {
				t.Errorf("%s -> %s: unexpected error: %v", from, to, err)
			}
			if !expected && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: got %v, want invalid transition", from, to, err)
			}
		}
	}
	var invalid T = "not_a_state"
	if !errors.Is(allow(invalid, states[0]), ErrInvalidState) || !errors.Is(allow(states[0], invalid), ErrInvalidState) {
		t.Error("unknown states must be invalid")
	}
}

func testTerminal[T comparable](t *testing.T, states []T, terminal map[T]bool, classify func(T) bool) {
	t.Helper()
	for _, state := range states {
		if classify(state) != terminal[state] {
			t.Errorf("terminal(%v) = %v, want %v", state, classify(state), terminal[state])
		}
	}
}
