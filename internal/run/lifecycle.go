package run

type State string
type Phase string

const (
	Queued          State = "queued"
	SettingUp       State = "setting_up"
	Working         State = "working"
	NeedsYou        State = "needs_you"
	Canceling       State = "canceling"
	Uncertain       State = "uncertain"
	ResultReady     State = "result_ready"
	Failed          State = "failed"
	CleanupRequired State = "cleanup_required"
)

const (
	Absent          Phase = "absent"
	Provisioning    Phase = "provisioning"
	PromptPending   Phase = "prompt_pending"
	Admitted        Phase = "admitted"
	Sealing         Phase = "sealing"
	Cleaning        Phase = "cleaning"
	CleanupComplete Phase = "cleanup_complete"
)

// Lifecycle is a classification, not a transition engine. Fencing, evidence,
// and persistence remain the caller's responsibility. Invalid pairs grant no
// execution authority.
type Lifecycle struct {
	Valid                  bool
	EnforceAttemptDeadline bool
	EnforceExecutionConfig bool
	TimeoutEligible        bool
	// CleanupStep identifies the retryable destructive cleanup sequence, not
	// sealing, exporting, or terminal cleanup evidence.
	CleanupStep bool
}

// Classify is the single owner of valid state/phase pairs and execution policy.
// Recovery and cleanup phases deliberately outlive the attempt deadline and
// the currently configured execution identity.
func Classify(state State, phase Phase) Lifecycle {
	var valid, executing, cleanupStep bool
	switch phase {
	case Absent:
		valid, executing = state == Queued, true
	case Provisioning:
		valid, executing = state == SettingUp, true
	case PromptPending:
		valid, executing = state == SettingUp || state == Uncertain, true
	case Admitted:
		valid, executing = state == Working || state == NeedsYou || state == Uncertain, true
	case Sealing:
		valid = state == Canceling
	case Cleaning:
		cleanupStep = true
		valid = state == Canceling || state == CleanupRequired || state == ResultReady
	case CleanupComplete:
		valid = state == ResultReady || state == Failed
	}
	return Lifecycle{
		Valid:                  valid,
		EnforceAttemptDeadline: valid && executing,
		EnforceExecutionConfig: valid && executing,
		TimeoutEligible:        valid && executing && state != Queued,
		CleanupStep:            valid && cleanupStep,
	}
}
