package observability

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestRegistryIsFixedCardinalityAndSanitizesFailures(t *testing.T) {
	registry := NewRegistry()
	secret := "remote token must-not-escape\nsecond line"
	if !registry.Degraded(ComponentTaskDelivery, errors.New(secret)) {
		t.Fatal("known component update was rejected")
	}
	if registry.Failed(Component("attacker="+secret), errors.New(secret)) {
		t.Fatal("unknown component update was accepted")
	}
	payload, err := json.Marshal(registry.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) || len(registry.Snapshot().Components) != len(components) {
		t.Fatalf("unsafe or variable snapshot: %s", payload)
	}
	if !registry.Snapshot().Ready {
		t.Fatal("degraded component made service unready")
	}
	registry.Blocked(ComponentGitHubTaskDependency, errors.New(secret))
	blocked := registry.Snapshot()
	if blocked.Ready || blocked.Components[componentIndexForTest(ComponentGitHubTaskDependency)].State != StateBlocked {
		t.Fatalf("blocked dependency snapshot = %+v", blocked)
	}
	registry.Healthy(ComponentGitHubTaskDependency)
	registry.Failed(ComponentTaskDelivery, errors.New(secret))
	if registry.Snapshot().Ready {
		t.Fatal("failed component left service ready")
	}
	registry.Healthy(ComponentTaskDelivery)
	if !registry.Snapshot().Ready {
		t.Fatal("healthy recovery left service unready")
	}
}

func componentIndexForTest(component Component) int {
	index, _ := componentIndex(component)
	return index
}

func TestRegistryConcurrentUpdatesAndSnapshots(t *testing.T) {
	registry := NewRegistry()
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(offset int) {
			defer wait.Done()
			for iteration := 0; iteration < 1000; iteration++ {
				component := components[(offset+iteration)%len(components)]
				registry.Degraded(component, errors.New("not exported"))
				registry.Healthy(component)
				if got := len(registry.Snapshot().Components); got != len(components) {
					t.Errorf("component count = %d", got)
					return
				}
			}
		}(i)
	}
	wait.Wait()
}
