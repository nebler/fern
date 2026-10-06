package run

import "testing"

func TestLifecycleClassification(t *testing.T) {
	states := []State{Queued, SettingUp, Working, NeedsYou, Canceling, Uncertain, ResultReady, Failed, CleanupRequired, "unknown"}
	// Specify the complete accepted matrix independently, grouped by state.
	valid := map[State][]Phase{
		Queued:          {Absent},
		SettingUp:       {Provisioning, PromptPending},
		Working:         {Admitted},
		NeedsYou:        {Admitted},
		Canceling:       {Sealing, Cleaning},
		Uncertain:       {PromptPending, Admitted},
		ResultReady:     {Cleaning, CleanupComplete},
		Failed:          {CleanupComplete},
		CleanupRequired: {Cleaning},
	}
	phases := []Phase{Absent, Provisioning, PromptPending, Admitted, Sealing, Cleaning, CleanupComplete, "unknown",
		"stop_intent", "writer_inactive", "exporting", "artifact_committed", "pre_effect_failed"}
	recovery := map[Phase]bool{Sealing: true, Cleaning: true, CleanupComplete: true}
	for _, state := range states {
		for _, phase := range phases {
			t.Run(string(state)+"/"+string(phase), func(t *testing.T) {
				wantValid := false
				for _, accepted := range valid[state] {
					wantValid = wantValid || accepted == phase
				}
				want := Lifecycle{
					Valid:           wantValid,
					Executing:       wantValid && !recovery[phase],
					TimeoutEligible: wantValid && !recovery[phase] && state != Queued,
				}
				if got := Classify(state, phase); got != want {
					t.Fatalf("Classify = %+v, want %+v", got, want)
				}
			})
		}
	}
}
