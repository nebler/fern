package observability

import (
	"errors"
	"sync"
	"testing"
)

func TestRegistryRejectsUnknownComponentsAndTracksReadiness(t *testing.T) {
	registry := NewRegistry()
	if !registry.Ready() {
		t.Fatal("new registry is unready")
	}
	if registry.Healthy(Component("task-publication")) || registry.Failed(Component("attacker"), errors.New("x")) {
		t.Fatal("unknown component update was accepted")
	}
	if !registry.Degraded(ComponentBackgroundRunSerial, errors.New("transient")) || !registry.Ready() {
		t.Fatal("degraded component made service unready")
	}
	registry.Blocked(ComponentGitHubDependency, errors.New("credentials unavailable"))
	if registry.Ready() {
		t.Fatal("blocked dependency left service ready")
	}
	registry.Healthy(ComponentGitHubDependency)
	registry.Failed(ComponentBackgroundRunSerial, errors.New("fatal"))
	if registry.Ready() {
		t.Fatal("failed component left service ready")
	}
	registry.Healthy(ComponentBackgroundRunSerial)
	if !registry.Ready() {
		t.Fatal("healthy recovery left service unready")
	}
}

func TestRegistryConcurrentUpdatesAndReads(t *testing.T) {
	registry := NewRegistry()
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(offset int) {
			defer wait.Done()
			for iteration := 0; iteration < 1000; iteration++ {
				component := components[(offset+iteration)%len(components)]
				registry.Blocked(component, nil)
				_ = registry.Ready()
				registry.Healthy(component)
			}
		}(i)
	}
	wait.Wait()
	if !registry.Ready() {
		t.Fatal("registry unready after every component recovered")
	}
}
