package domain

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

// Phases are the durable steps of the reconcile loop. Each names only what
// cannot be re-derived by inspecting deterministic resources: the provisioning
// slot, the one-way prompt fence, an admitted prompt, a seal, and teardown.
const (
	Absent          Phase = "absent"           // queued; no effect has started
	Provisioning    Phase = "provisioning"     // reconciling clone through session; the runtime, once started, is recorded
	PromptPending   Phase = "prompt_pending"   // the prompt fence is set: dispatched at most once, then only reconciled
	Admitted        Phase = "admitted"         // the prompt is in the session; work is observed
	Sealing         Phase = "sealing"          // seal won; writer fence then export, resources retained
	Cleaning        Phase = "cleaning"         // stop, timeout, failure, or committed result: tear down
	CleanupComplete Phase = "cleanup_complete" // terminal; every resource proven absent
)

// Lifecycle is a classification, not a transition engine. Fencing, evidence,
// and persistence remain the caller's responsibility. Invalid pairs grant no
// execution authority.
type Lifecycle struct {
	Valid bool
	// Executing phases enforce the run deadline and the configured
	// execution identity. Sealing and cleanup deliberately outlive both.
	Executing bool
	// TimeoutEligible is an executing phase past admission's queue.
	TimeoutEligible bool
}

// Classify is the single owner of valid state/phase pairs and execution policy.
func Classify(state State, phase Phase) Lifecycle {
	var valid, executing bool
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
		valid = state == Canceling || state == CleanupRequired || state == ResultReady
	case CleanupComplete:
		valid = state == ResultReady || state == Failed
	}
	return Lifecycle{Valid: valid, Executing: valid && executing, TimeoutEligible: valid && executing && phase != Absent}
}
