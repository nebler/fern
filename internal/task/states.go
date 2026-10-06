package task

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

func (s TaskState) Valid() bool    { return validState(s, allTaskStates) }
func (s AttemptState) Valid() bool { return validState(s, allAttemptStates) }
func (s ResultState) Valid() bool  { return validState(s, allResultStates) }
