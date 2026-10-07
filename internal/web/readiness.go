package web

import "sync/atomic"

type Component string

const (
	ComponentGitHubDependency     Component = "github-dependency"
	ComponentBackgroundRunProfile Component = "background-run-profile"
	ComponentBackgroundRunSerial  Component = "background-run-serial"
)

var components = [...]Component{
	ComponentGitHubDependency,
	ComponentBackgroundRunProfile,
	ComponentBackgroundRunSerial,
}

// Registry holds one readiness flag for each compile-time component. Every
// component starts ready.
type Registry struct {
	unready [len(components)]atomic.Bool
}

func NewRegistry() *Registry {
	return &Registry{}
}

func (registry *Registry) Healthy(component Component) bool {
	return registry.set(component, true)
}

// Degraded records a transient failure; the component stays ready.
func (registry *Registry) Degraded(component Component, _ error) bool {
	return registry.set(component, true)
}

// Blocked records a missing required dependency; the component is unready.
func (registry *Registry) Blocked(component Component, _ error) bool {
	return registry.set(component, false)
}

// Failed records a fatal component failure; the component is unready.
func (registry *Registry) Failed(component Component, _ error) bool {
	return registry.set(component, false)
}

func (registry *Registry) set(component Component, ready bool) bool {
	for i, known := range components {
		if component == known {
			registry.unready[i].Store(!ready)
			return true
		}
	}
	return false
}

// Ready reports whether every component is ready.
func (registry *Registry) Ready() bool {
	for i := range registry.unready {
		if registry.unready[i].Load() {
			return false
		}
	}
	return true
}
