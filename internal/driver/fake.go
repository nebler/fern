package driver

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// Fake is an in-memory driver for tests. Every awake box is reachable at the
// address it was made with. It is safe for concurrent use.
type Fake struct {
	addr string

	mu    sync.Mutex
	boxes map[string]State
	wakes int
}

// NewFake returns a Fake whose boxes serve exec at addr.
func NewFake(addr string) *Fake {
	return &Fake{addr: addr, boxes: make(map[string]State)}
}

// Create makes environment env's box, asleep.
func (f *Fake) Create(_ context.Context, env, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[env]; !ok {
		f.boxes[env] = Asleep
	}
	return nil
}

// Wake marks env's box running and returns the Fake's address.
func (f *Fake) Wake(_ context.Context, env string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[env]; !ok {
		return "", fmt.Errorf("environment %s: %w", env, ErrNotFound)
	}
	if f.boxes[env] != Running {
		f.wakes++
	}
	f.boxes[env] = Running
	return f.addr, nil
}

// Sleep marks env's box asleep.
func (f *Fake) Sleep(_ context.Context, env string) error {
	return f.set(env, Asleep)
}

// Delete forgets env's box.
func (f *Fake) Delete(_ context.Context, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.boxes, env)
	return nil
}

// State reports env's box state.
func (f *Fake) State(_ context.Context, env string) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.boxes[env]
	if !ok {
		return "", fmt.Errorf("environment %s: %w", env, ErrNotFound)
	}
	return state, nil
}

// States reports every box's state.
func (f *Fake) States(context.Context) (map[string]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.boxes), nil
}

// Wakes returns how many times a box went from not running to running.
func (f *Fake) Wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
}

func (f *Fake) set(env string, state State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[env]; !ok {
		return fmt.Errorf("environment %s: %w", env, ErrNotFound)
	}
	f.boxes[env] = state
	return nil
}
