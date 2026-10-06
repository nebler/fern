package task

type TaskState string

// Task states are the parent-record projection of a background run: queued
// until the run is sealed (completed) or terminalized without a result (failed).
const (
	TaskQueued    TaskState = "queued"
	TaskFailed    TaskState = "failed"
	TaskCompleted TaskState = "completed"
)

var allTaskStates = []TaskState{TaskQueued, TaskFailed, TaskCompleted}

type AttemptState string

// Attempt states: the single attempt stays prepared while the background run
// executes, then becomes superseded by a sealed result or failed.
const (
	AttemptPrepared   AttemptState = "prepared"
	AttemptFailed     AttemptState = "failed"
	AttemptSuperseded AttemptState = "superseded"
)

var allAttemptStates = []AttemptState{AttemptPrepared, AttemptFailed, AttemptSuperseded}

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
