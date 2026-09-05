// Package run owns the background execution lifecycle, independent of storage.
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
	Absent            Phase = "absent"
	ProvisionIntent   Phase = "provision_intent"
	CloneObserved     Phase = "clone_observed"
	VolumeObserved    Phase = "volume_observed"
	ContainerObserved Phase = "container_observed"
	HealthObserved    Phase = "health_observed"
	Ready             Phase = "ready"
	SessionObserved   Phase = "session_observed"
	PromptIntent      Phase = "prompt_intent"
	PromptAdmitted    Phase = "prompt_admitted"
	SealIntent        Phase = "seal_intent"
	StopIntent        Phase = "stop_intent"
	WriterInactive    Phase = "writer_inactive"
	Exporting         Phase = "exporting"
	ArtifactCommitted Phase = "artifact_committed"
	RouteRemoved      Phase = "route_removed"
	ContainerRemoved  Phase = "container_removed"
	VolumeRemoved     Phase = "volume_removed"
	CloneRemoved      Phase = "clone_removed"
	CleanupComplete   Phase = "cleanup_complete"
	PreEffectFailed   Phase = "pre_effect_failed"
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
	case ProvisionIntent, CloneObserved, VolumeObserved, ContainerObserved, HealthObserved, Ready, SessionObserved:
		valid, executing = state == SettingUp || state == Uncertain, true
	case PromptIntent:
		valid, executing = state == Uncertain, true
	case PromptAdmitted:
		valid, executing = state == Working || state == NeedsYou || state == Uncertain, true
	case StopIntent:
		cleanupStep = true
		valid = state == Canceling || state == Uncertain || state == ResultReady || state == CleanupRequired
	case WriterInactive, RouteRemoved, ContainerRemoved, VolumeRemoved, CloneRemoved:
		cleanupStep = true
		valid = state == Canceling || state == ResultReady || state == CleanupRequired
	case SealIntent:
		valid = state == Canceling
	case Exporting:
		valid = state == Canceling || state == CleanupRequired
	case ArtifactCommitted:
		valid = state == ResultReady
	case CleanupComplete:
		valid = state == ResultReady || state == Failed
	case PreEffectFailed:
		valid = state == Failed
	}
	return Lifecycle{
		Valid:                  valid,
		EnforceAttemptDeadline: valid && executing,
		EnforceExecutionConfig: valid && executing,
		TimeoutEligible:        valid && executing && state != Queued,
		CleanupStep:            valid && cleanupStep,
	}
}
