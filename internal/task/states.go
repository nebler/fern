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

type ApprovalState string

const (
	ApprovalPending          ApprovalState = "pending"
	ApprovalDecisionRecorded ApprovalState = "decision_recorded"
	ApprovalDelivering       ApprovalState = "delivering"
	ApprovalUncertain        ApprovalState = "uncertain"
	ApprovalRecoveryRequired ApprovalState = "recovery_required"
	ApprovalApplied          ApprovalState = "applied"
	ApprovalRejected         ApprovalState = "rejected"
	ApprovalExpired          ApprovalState = "expired"
	ApprovalCanceled         ApprovalState = "canceled"
)

var allApprovalStates = []ApprovalState{ApprovalPending, ApprovalDecisionRecorded, ApprovalDelivering, ApprovalUncertain, ApprovalRecoveryRequired, ApprovalApplied, ApprovalRejected, ApprovalExpired, ApprovalCanceled}

type ResultState string

const (
	ResultCollecting       ResultState = "collecting"
	ResultUncertain        ResultState = "uncertain"
	ResultRecoveryRequired ResultState = "recovery_required"
	ResultSealed           ResultState = "sealed"
	ResultFailed           ResultState = "failed"
)

var allResultStates = []ResultState{ResultCollecting, ResultUncertain, ResultRecoveryRequired, ResultSealed, ResultFailed}

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
func (s ApprovalState) Valid() bool { return validState(s, allApprovalStates) }
func (s ApprovalState) Terminal() bool {
	return s == ApprovalApplied || s == ApprovalRejected || s == ApprovalExpired || s == ApprovalCanceled
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
var approvalTransitions = map[ApprovalState][]ApprovalState{
	ApprovalPending: {ApprovalDecisionRecorded, ApprovalExpired, ApprovalCanceled, ApprovalRecoveryRequired}, ApprovalDecisionRecorded: {ApprovalDelivering, ApprovalCanceled, ApprovalRecoveryRequired},
	ApprovalDelivering: {ApprovalApplied, ApprovalRejected, ApprovalUncertain, ApprovalRecoveryRequired}, ApprovalUncertain: {ApprovalDelivering, ApprovalApplied, ApprovalRejected, ApprovalCanceled, ApprovalRecoveryRequired},
	ApprovalRecoveryRequired: {ApprovalDelivering, ApprovalRejected, ApprovalCanceled},
}
var resultTransitions = map[ResultState][]ResultState{ResultCollecting: {ResultSealed, ResultFailed, ResultUncertain, ResultRecoveryRequired}, ResultUncertain: {ResultCollecting, ResultSealed, ResultFailed, ResultRecoveryRequired}, ResultRecoveryRequired: {ResultCollecting, ResultFailed}}
var verificationTransitions = map[VerificationState][]VerificationState{
	VerificationRequested: {VerificationRunning, VerificationCanceled, VerificationRecoveryRequired}, VerificationRunning: {VerificationSucceeded, VerificationFailed, VerificationCancelRequested, VerificationUncertain, VerificationRecoveryRequired},
	VerificationCancelRequested: {VerificationCanceled, VerificationFailed, VerificationUncertain, VerificationRecoveryRequired}, VerificationUncertain: {VerificationRunning, VerificationSucceeded, VerificationFailed, VerificationCanceled, VerificationRecoveryRequired},
	VerificationRecoveryRequired: {VerificationRequested, VerificationFailed, VerificationCanceled},
}
var publicationTransitions = map[PublicationState][]PublicationState{
	PublicationRequested: {PublicationPreparing, PublicationCancelRequested, PublicationFailed, PublicationRecoveryRequired}, PublicationPreparing: {PublicationReady, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired},
	PublicationReady: {PublicationPushing, PublicationCancelRequested, PublicationConflict, PublicationRecoveryRequired}, PublicationPushing: {PublicationOpeningPR, PublicationReconciling, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired},
	PublicationOpeningPR: {PublicationReconciling, PublicationPublished, PublicationFailed, PublicationConflict, PublicationUncertain, PublicationCancelRequested, PublicationRecoveryRequired}, PublicationReconciling: {PublicationPushing, PublicationOpeningPR, PublicationPublished, PublicationFailed, PublicationConflict, PublicationCancelRequested, PublicationRecoveryRequired},
	PublicationCancelRequested: {PublicationCanceled, PublicationReconciling, PublicationPublished, PublicationConflict, PublicationUncertain, PublicationRecoveryRequired}, PublicationUncertain: {PublicationReconciling, PublicationPublished, PublicationFailed, PublicationConflict, PublicationCanceled, PublicationRecoveryRequired},
	PublicationRecoveryRequired: {PublicationReconciling, PublicationFailed, PublicationConflict, PublicationCanceled},
}

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
func AllowApprovalTransition(from, to ApprovalState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, approvalTransitions) {
		return transitionError(from, to)
	}
	return nil
}
func AllowResultTransition(from, to ResultState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, resultTransitions) {
		return transitionError(from, to)
	}
	return nil
}
func AllowVerificationTransition(from, to VerificationState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, verificationTransitions) {
		return transitionError(from, to)
	}
	return nil
}
func AllowPublicationTransition(from, to PublicationState) error {
	if !from.Valid() || !to.Valid() {
		return ErrInvalidState
	}
	if !allowed(from, to, publicationTransitions) {
		return transitionError(from, to)
	}
	return nil
}
