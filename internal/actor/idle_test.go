package actor

import (
	"testing"
	"time"
)

// When a box sleeps is one function of what its actor knows. This is the
// whole policy, a row per rule; the package's other tests reach it through
// Exec, Job and Prewake.
func TestSleepAt(t *testing.T) {
	var (
		t0     = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		idle   = Idle{Agent: 30 * time.Second, Human: 5 * time.Minute, Prewake: time.Minute}
		opened = t0
		closed = t0.Add(10 * time.Second)
		until  = t0.Add(12 * time.Second)
	)
	tests := []struct {
		name  string
		phase boxPhase
		execs int
		jobs  int
		lease lease
		idle  Idle
		want  time.Time // zero: it may not sleep
	}{
		{name: "asleep already", phase: boxAsleep, idle: idle},
		{name: "a command runs", phase: boxUp, execs: 1, idle: idle},
		{name: "a command runs, whatever the lease", phase: boxUp, execs: 1, lease: lease{leaseRetry, until}, idle: idle},
		{name: "idle sleep is off", phase: boxUp, idle: Idle{}},
		{name: "idle sleep is off for people", phase: boxUp, idle: Idle{Agent: time.Second}},

		// With no lease, the later of the two idle times.
		{name: "idle: a person's time is the later", phase: boxUp, idle: idle, want: opened.Add(5 * time.Minute)},
		{name: "idle: an agent's time is the later", phase: boxUp, idle: Idle{Agent: 30 * time.Second, Human: 5 * time.Second}, want: closed.Add(30 * time.Second)},
		{name: "a box that may be up sleeps like one that is", phase: boxMaybeUp, idle: idle, want: opened.Add(5 * time.Minute)},
		{name: "a job doesn't keep it up past the idle times", phase: boxUp, jobs: 1, idle: idle, want: opened.Add(5 * time.Minute)},

		// A lease replaces the idle times, for a box with nothing left to do.
		{name: "linger", phase: boxUp, lease: lease{leaseLinger, until}, idle: idle, want: until},
		{name: "a pre-wake under way", phase: boxMaybeUp, lease: lease{leasePrewaking, until}, idle: idle, want: until},
		{name: "woken ahead", phase: boxUp, lease: lease{leasePrewoken, until}, idle: idle, want: until},
		{name: "a job outlasts a linger", phase: boxUp, jobs: 1, lease: lease{leaseLinger, until}, idle: idle, want: opened.Add(5 * time.Minute)},
		{name: "a job outlasts a pre-wake", phase: boxUp, jobs: 1, lease: lease{leasePrewoken, until}, idle: idle, want: opened.Add(5 * time.Minute)},

		// A stop that failed is tried again then, whatever runs in the background.
		{name: "retry", phase: boxMaybeUp, lease: lease{leaseRetry, until}, idle: idle, want: until},
		{name: "retry, with a job", phase: boxMaybeUp, jobs: 1, lease: lease{leaseRetry, until}, idle: idle, want: until},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sleepAt(tt.phase, tt.execs, tt.jobs, tt.lease, opened, closed, tt.idle)
			if ok != !tt.want.IsZero() || !got.Equal(tt.want) {
				t.Errorf("sleepAt() = %s, %t; want %s, %t", got.Format(time.TimeOnly), ok, tt.want.Format(time.TimeOnly), !tt.want.IsZero())
			}
		})
	}
}
