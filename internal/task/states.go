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

// VerificationState is the task-model view of verification lifecycle values.
// Values arrive as casts from taskstore's authoritative persisted lifecycle
// (see VerificationTuple construction in internal/taskstore/publication.go and
// the coordinator's tuple assembly in internal/taskpublicationcoord/
// coordinator.go). taskstore owns the persisted state machines with an
// overlapping-but-different value set; those conversions assume the overlap,
// so any taskstore enum edit must be mirrored here.
//
// Current values: requested, running, cancel_requested, uncertain,
// recovery_required, succeeded, failed, canceled.
type VerificationState string

const (
	VerificationRequested        VerificationState = "requested"
	VerificationRunning          VerificationState = "running"
	VerificationCancelRequested  VerificationState = "cancel_requested"
	VerificationUncertain        VerificationState = "uncertain"
	VerificationRecoveryRequired VerificationState = "recovery_required"
	VerificationSucceeded        VerificationState = "succeeded"
	VerificationFailed           VerificationState = "failed"
	VerificationCanceled         VerificationState = "canceled"
)

var allVerificationStates = []VerificationState{VerificationRequested, VerificationRunning, VerificationCancelRequested, VerificationUncertain, VerificationRecoveryRequired, VerificationSucceeded, VerificationFailed, VerificationCanceled}

// PublicationState is the task-model view of publication lifecycle values.
// Values arrive as casts from taskstore's authoritative persisted lifecycle
// (see the tuple conversions in internal/taskstore/publication.go and
// internal/taskpublicationcoord/coordinator.go). taskstore owns the persisted
// state machine with an overlapping-but-different value set; those
// conversions assume the overlap, so any taskstore enum edit must be mirrored
// here.
//
// Current values: requested, preparing, ready, pushing, opening_pr,
// reconciling, cancel_requested, uncertain, recovery_required, published,
// failed, conflict, canceled.
type PublicationState string

const (
	PublicationRequested        PublicationState = "requested"
	PublicationPreparing        PublicationState = "preparing"
	PublicationReady            PublicationState = "ready"
	PublicationPushing          PublicationState = "pushing"
	PublicationOpeningPR        PublicationState = "opening_pr"
	PublicationReconciling      PublicationState = "reconciling"
	PublicationCancelRequested  PublicationState = "cancel_requested"
	PublicationUncertain        PublicationState = "uncertain"
	PublicationRecoveryRequired PublicationState = "recovery_required"
	PublicationPublished        PublicationState = "published"
	PublicationFailed           PublicationState = "failed"
	PublicationConflict         PublicationState = "conflict"
	PublicationCanceled         PublicationState = "canceled"
)

var allPublicationStates = []PublicationState{PublicationRequested, PublicationPreparing, PublicationReady, PublicationPushing, PublicationOpeningPR, PublicationReconciling, PublicationCancelRequested, PublicationUncertain, PublicationRecoveryRequired, PublicationPublished, PublicationFailed, PublicationConflict, PublicationCanceled}

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
func (s ResultState) Valid() bool       { return validState(s, allResultStates) }
func (s ResultState) Terminal() bool    { return s == ResultSealed || s == ResultFailed }
func (s VerificationState) Valid() bool { return validState(s, allVerificationStates) }
func (s VerificationState) Terminal() bool {
	return s == VerificationSucceeded || s == VerificationFailed || s == VerificationCanceled
}
func (s PublicationState) Valid() bool { return validState(s, allPublicationStates) }
func (s PublicationState) Terminal() bool {
	return s == PublicationPublished || s == PublicationFailed || s == PublicationConflict || s == PublicationCanceled
}

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

// The authoritative approval, result, verification, and publication state
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
