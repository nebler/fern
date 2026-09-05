package run

import "testing"

func TestLifecycleClassification(t *testing.T) {
	states := []State{Queued, SettingUp, Working, NeedsYou, Canceling, Uncertain, ResultReady, Failed, CleanupRequired, "unknown"}
	// Specify the complete accepted matrix independently, grouped by state.
	valid := map[State][]Phase{
		Queued:          {Absent},
		SettingUp:       {ProvisionIntent, CloneObserved, VolumeObserved, ContainerObserved, HealthObserved, Ready, SessionObserved},
		Working:         {PromptAdmitted},
		NeedsYou:        {PromptAdmitted},
		Canceling:       {StopIntent, WriterInactive, RouteRemoved, ContainerRemoved, VolumeRemoved, CloneRemoved, SealIntent, Exporting},
		Uncertain:       {ProvisionIntent, CloneObserved, VolumeObserved, ContainerObserved, HealthObserved, Ready, SessionObserved, PromptIntent, PromptAdmitted, StopIntent},
		ResultReady:     {ArtifactCommitted, StopIntent, WriterInactive, RouteRemoved, ContainerRemoved, VolumeRemoved, CloneRemoved, CleanupComplete},
		Failed:          {PreEffectFailed, CleanupComplete},
		CleanupRequired: {StopIntent, WriterInactive, RouteRemoved, ContainerRemoved, VolumeRemoved, CloneRemoved, Exporting},
	}
	phases := []Phase{Absent, ProvisionIntent, CloneObserved, VolumeObserved, ContainerObserved, HealthObserved, Ready, SessionObserved,
		PromptIntent, PromptAdmitted, SealIntent, StopIntent, WriterInactive, Exporting, ArtifactCommitted, RouteRemoved, ContainerRemoved,
		VolumeRemoved, CloneRemoved, CleanupComplete, PreEffectFailed, "unknown", "provision_started", "prompt_started", "stop_started", "export_started", "cleanup_started"}
	cleanup := map[Phase]bool{StopIntent: true, WriterInactive: true, RouteRemoved: true, ContainerRemoved: true, VolumeRemoved: true, CloneRemoved: true}
	recovery := map[Phase]bool{SealIntent: true, StopIntent: true, WriterInactive: true, Exporting: true, ArtifactCommitted: true,
		RouteRemoved: true, ContainerRemoved: true, VolumeRemoved: true, CloneRemoved: true, CleanupComplete: true, PreEffectFailed: true}
	for _, state := range states {
		for _, phase := range phases {
			t.Run(string(state)+"/"+string(phase), func(t *testing.T) {
				wantValid := false
				for _, accepted := range valid[state] {
					wantValid = wantValid || accepted == phase
				}
				want := Lifecycle{
					Valid:                  wantValid,
					EnforceAttemptDeadline: wantValid && !recovery[phase],
					EnforceExecutionConfig: wantValid && !recovery[phase],
					TimeoutEligible:        wantValid && !recovery[phase] && state != Queued,
					CleanupStep:            wantValid && cleanup[phase],
				}
				if got := Classify(state, phase); got != want {
					t.Fatalf("Classify = %+v, want %+v", got, want)
				}
			})
		}
	}
}
