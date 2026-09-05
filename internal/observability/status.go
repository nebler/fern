package observability

import (
	"sync"
	"time"
)

type Component string

const (
	ComponentTaskPublication      Component = "task-publication"
	ComponentTaskVerification     Component = "task-verification"
	ComponentGitHubTaskDependency Component = "github-task-dependency"
	ComponentLegacyPublication    Component = "legacy-publication"
	ComponentBackgroundRunProfile Component = "background-run-profile"
	ComponentBackgroundRunSerial  Component = "background-run-serial"
)

var components = [...]Component{
	ComponentTaskPublication,
	ComponentTaskVerification,
	ComponentGitHubTaskDependency,
	ComponentLegacyPublication,
	ComponentBackgroundRunProfile,
	ComponentBackgroundRunSerial,
}

type State string

const (
	StateDisabled  State = "disabled"
	StateHealthy   State = "healthy"
	StateQualified State = "qualified"
	StateDegraded  State = "degraded"
	StateBlocked   State = "blocked"
	StateFailed    State = "failed"
)

type ComponentStatus struct {
	Component           Component `json:"component"`
	State               State     `json:"state"`
	Ready               bool      `json:"ready"`
	ConsecutiveFailures uint64    `json:"consecutiveFailures"`
	FailuresTotal       uint64    `json:"failuresTotal"`
	LastTransition      time.Time `json:"lastTransition"`
	Detail              string    `json:"detail,omitempty"`
}

type Snapshot struct {
	Ready      bool              `json:"ready"`
	Components []ComponentStatus `json:"components"`
}

// Registry stores one slot for each compile-time component. Callers cannot add
// labels or error text, keeping memory and exported telemetry cardinality fixed.
type Registry struct {
	mu     sync.RWMutex
	status [len(components)]ComponentStatus
	now    func() time.Time
}

func NewRegistry() *Registry {
	now := time.Now
	registry := &Registry{now: now}
	created := now().UTC()
	for i, component := range components {
		registry.status[i] = ComponentStatus{Component: component, State: StateDisabled, Ready: true, LastTransition: created}
	}
	return registry
}

func (registry *Registry) Healthy(component Component) bool {
	return registry.update(component, StateHealthy, nil)
}

func (registry *Registry) Qualified(component Component) bool {
	return registry.update(component, StateQualified, nil)
}

func (registry *Registry) Degraded(component Component, _ error) bool {
	return registry.update(component, StateDegraded, nil)
}

func (registry *Registry) Blocked(component Component, _ error) bool {
	return registry.update(component, StateBlocked, nil)
}

func (registry *Registry) Failed(component Component, _ error) bool {
	return registry.update(component, StateFailed, nil)
}

func (registry *Registry) update(component Component, state State, _ error) bool {
	index, ok := componentIndex(component)
	if !ok {
		return false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	status := &registry.status[index]
	if state == StateDegraded || state == StateBlocked || state == StateFailed {
		status.ConsecutiveFailures++
		status.FailuresTotal++
	} else {
		status.ConsecutiveFailures = 0
	}
	detail := ""
	if state == StateDegraded {
		detail = "transient operation failure"
	} else if state == StateBlocked {
		detail = "required dependency unavailable"
	} else if state == StateFailed {
		detail = "fatal component failure"
	}
	if status.State != state || status.Detail != detail {
		status.LastTransition = registry.now().UTC()
	}
	status.State = state
	status.Ready = state != StateBlocked && state != StateFailed
	status.Detail = detail
	return true
}

func (registry *Registry) Snapshot() Snapshot {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	snapshot := Snapshot{Ready: true, Components: make([]ComponentStatus, len(registry.status))}
	copy(snapshot.Components, registry.status[:])
	for _, status := range snapshot.Components {
		if !status.Ready {
			snapshot.Ready = false
		}
	}
	return snapshot
}

func componentIndex(component Component) (int, bool) {
	for i, known := range components {
		if component == known {
			return i, true
		}
	}
	return 0, false
}
