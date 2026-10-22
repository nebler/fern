package driver

import (
	"context"
	"maps"
	"sync"

	"github.com/nebler/fern/internal/store"
)

// Fake is an in-memory driver for tests. Every awake box is reachable at the
// address it was made with. It is safe for concurrent use.
type Fake struct {
	addr string

	mu    sync.Mutex
	boxes map[string]State // by environment ID, from its first wake
	wakes int
}

// NewFake returns a Fake whose boxes serve exec at addr.
func NewFake(addr string) *Fake {
	return &Fake{addr: addr, boxes: make(map[string]State)}
}

// Wake marks env's box running and returns the Fake's address.
func (f *Fake) Wake(_ context.Context, env store.Env) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boxes[env.ID] != Running {
		f.wakes++
	}
	f.boxes[env.ID] = Running
	return f.addr, nil
}

// Sleep marks box id asleep.
func (f *Fake) Sleep(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.boxes[id]; ok {
		f.boxes[id] = Asleep
	}
	return nil
}

// Delete forgets box id.
func (f *Fake) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.boxes, id)
	return nil
}

// States returns the state of every box that has been woken.
func (f *Fake) States(context.Context) (map[string]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.boxes), nil
}

// Wakes returns how many times a box has been woken.
func (f *Fake) Wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
}
