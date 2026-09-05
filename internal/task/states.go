package task

import "fmt"

type TaskState string

const (
	TaskQueued           TaskState = "queued"
	TaskRunning          TaskState = "running"
	TaskInputRequired    TaskState = "input_required"
	TaskCancelRequested  TaskState = "cancel_requested"
	TaskUncertain        TaskState = "uncertain"
	TaskRecoveryRequired TaskState = "recovery_required"
	TaskCompleted        TaskState = "completed"
	TaskFailed           TaskState = "failed"
	TaskCanceled         TaskState = "canceled"
)

var allTaskStates = []TaskState{TaskQueued, TaskRunning, TaskInputRequired, TaskCancelRequested, TaskUncertain, TaskRecoveryRequired, TaskCompleted, TaskFailed, TaskCanceled}

type AttemptState string

const (
	AttemptPrepared         AttemptState = "prepared"
	AttemptDelivering       AttemptState = "delivering"
	AttemptAdmitted         AttemptState = "admitted"
	AttemptRunning          AttemptState = "running"
	AttemptInputRequired    AttemptState = "input_required"
	AttemptCancelRequested  AttemptState = "cancel_requested"
	AttemptUncertain        AttemptState = "uncertain"
	AttemptRecoveryRequired AttemptState = "recovery_required"
	AttemptSucceeded        AttemptState = "succeeded"
	AttemptFailed           AttemptState = "failed"
	AttemptCanceled         AttemptState = "canceled"
	AttemptSuperseded       AttemptState = "superseded"
)

var allAttemptStates = []AttemptState{AttemptPrepared, AttemptDelivering, AttemptAdmitted, AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptUncertain, AttemptRecoveryRequired, AttemptSucceeded, AttemptFailed, AttemptCanceled, AttemptSuperseded}

// ResultState is the task-model view of result lifecycle values. Values
// arrive as casts from taskstore's authoritative persisted lifecycle.
type ResultState string

const (
	ResultSealed ResultState = "sealed"
	ResultFailed ResultState = "failed"
)

var allResultStates = []ResultState{ResultSealed, ResultFailed}

func validState[T ~string](s T, all []T) bool {
	for _, v := range all {
		if s == v {
			return true
		}
	}
	return false
}
func allowed[T comparable](from, to T, transitions map[T][]T) bool {
	for _, v := range transitions[from] {
		if to == v {
			return true
		}
	}
	return false
}
func transitionError(from, to any) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
}

func (s TaskState) Valid() bool    { return validState(s, allTaskStates) }
func (s TaskState) Terminal() bool { return s == TaskCompleted || s == TaskFailed || s == TaskCanceled }
func (s AttemptState) Valid() bool { return validState(s, allAttemptStates) }
func (s AttemptState) Terminal() bool {
	return s == AttemptSucceeded || s == AttemptFailed || s == AttemptCanceled || s == AttemptSuperseded
}
func (s ResultState) Valid() bool    { return validState(s, allResultStates) }
func (s ResultState) Terminal() bool { return s == ResultSealed || s == ResultFailed }

var taskTransitions = map[TaskState][]TaskState{
	TaskQueued: {TaskRunning, TaskCancelRequested, TaskUncertain, TaskRecoveryRequired, TaskFailed}, TaskRunning: {TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskUncertain, TaskRecoveryRequired},
	TaskInputRequired: {TaskRunning, TaskCancelRequested, TaskFailed, TaskUncertain, TaskRecoveryRequired}, TaskCancelRequested: {TaskCanceled, TaskUncertain, TaskRecoveryRequired},
	TaskUncertain: {TaskQueued, TaskRunning, TaskInputRequired, TaskCancelRequested, TaskCompleted, TaskFailed, TaskCanceled, TaskRecoveryRequired}, TaskRecoveryRequired: {TaskQueued, TaskRunning, TaskCancelRequested, TaskFailed, TaskCanceled},
}
var attemptTransitions = map[AttemptState][]AttemptState{
	AttemptPrepared: {AttemptDelivering, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed}, AttemptDelivering: {AttemptAdmitted, AttemptUncertain, AttemptCancelRequested, AttemptRecoveryRequired, AttemptFailed},
	AttemptAdmitted: {AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired}, AttemptRunning: {AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired},
	AttemptInputRequired: {AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptUncertain, AttemptRecoveryRequired}, AttemptCancelRequested: {AttemptCanceled, AttemptUncertain, AttemptRecoveryRequired},
	AttemptUncertain: {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptInputRequired, AttemptCancelRequested, AttemptSucceeded, AttemptFailed, AttemptCanceled, AttemptRecoveryRequired}, AttemptRecoveryRequired: {AttemptPrepared, AttemptAdmitted, AttemptRunning, AttemptCancelRequested, AttemptFailed, AttemptCanceled, AttemptSuperseded},
}

// The authoritative approval and result state
// machines live in taskstore behind SQL triggers; this package deliberately
// carries only the value types it needs for tuples and casts.

func AllowTaskTransition(from, to TaskState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, taskTransitions) {
		return transitionError(from, to)
	}
	return nil
}
func AllowAttemptTransition(from, to AttemptState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, attemptTransitions) {
		return transitionError(from, to)
	}
	return nil
}
