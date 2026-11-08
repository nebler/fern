package runsc

import (
	"context"
	"sync"
)

// Fake is an in-memory driver for tests. Every awake box is reachable at the
// address it was made with. It is safe for concurrent use.
type Fake struct {
	addr string

	mu        sync.Mutex
	boxes     map[string]Box  // the last wake of each box
	up        map[string]bool // the boxes that are up
	fail      map[string]error
	sleepFail map[string]error
	wakes     int
	sleeps    int
}

// NewFake returns a Fake whose boxes serve exec at addr.
func NewFake(addr string) *Fake {
	return &Fake{addr: addr, boxes: make(map[string]Box), up: make(map[string]bool), fail: make(map[string]error), sleepFail: make(map[string]error)}
}

// FailWakes makes box id's wakes fail with err; nil makes them succeed.
func (f *Fake) FailWakes(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[id] = err
}

// Wake starts box and returns the Fake's address, or the error FailWakes
// gave it.
func (f *Fake) Wake(_ context.Context, box Box) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[box.ID]; err != nil {
		return "", err
	}
	if !f.up[box.ID] {
		f.wakes++
	}
	f.boxes[box.ID], f.up[box.ID] = box, true
	return f.addr, nil
}

// FailSleeps makes box id's sleeps fail with err, leaving it up; nil makes
// them succeed.
func (f *Fake) FailSleeps(id string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleepFail[id] = err
}

// Sleep stops box id, or returns the error FailSleeps gave it.
func (f *Fake) Sleep(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps++
	if err := f.sleepFail[id]; err != nil {
		return err
	}
	delete(f.up, id)
	return nil
}

// Sleeps returns how many times a box has been put to sleep, or tried to be.
func (f *Fake) Sleeps() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sleeps
}

// States returns the boxes that are up, as running: an asleep box isn't
// listed, as with Driver.States.
func (f *Fake) States(context.Context) (map[string]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states := make(map[string]State, len(f.up))
	for id := range f.up {
		states[id] = Running
	}
	return states, nil
}

// Wakes returns how many times a box has been woken.
func (f *Fake) Wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
}

// Woken returns box id as it was last woken.
func (f *Fake) Woken(id string) Box {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boxes[id]
}
