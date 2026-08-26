package watch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

type ObservationKind string

const (
	ObservationConnected    ObservationKind = "connected"
	ObservationDisconnected ObservationKind = "disconnected"
	ObservationStatus       ObservationKind = "status"
	ObservationRequest      ObservationKind = "request"
	ObservationInvalidated  ObservationKind = "invalidated"
)

// Observation is one lifecycle-relevant activity fact for a workspace,
// tagged with the epoch of the stream generation that produced it.
type Observation struct {
	Epoch     uint64
	Kind      ObservationKind
	SessionID string
	Status    string
	Err       string
	// Handled is closed exactly once by the supervisor after the observation
	// is applied; producers must not close it.
	Handled chan struct{}
}

type streamState struct {
	epoch     uint64
	baseURL   string
	connected bool
	cancel    context.CancelFunc
	done      chan struct{}
	ready     chan struct{}
}

// StreamController owns one endpoint generation. Every activity observation
// carries that generation so stale events cannot authorize a later pause.
//
// Concurrency: mu guards state and nextEpoch. operations is a capacity-1
// token serializing connect/reconnect/stop against each other; it does not
// stand in for the mutex, which runGeneration callbacks also take.
type StreamController struct {
	parent  context.Context
	options StreamOptions
	out     chan<- Observation
	log     *slog.Logger

	operations chan struct{}
	mu         sync.Mutex
	nextEpoch  uint64
	state      streamState
}

func NewStreamController(parent context.Context, options StreamOptions, out chan<- Observation, log *slog.Logger) *StreamController {
	if log == nil {
		log = slog.Default()
	}
	operations := make(chan struct{}, 1)
	operations <- struct{}{}
	return &StreamController{parent: parent, options: options, out: out, log: log, operations: operations}
}

// Connect returns only when this exact endpoint generation is connected now.
func (c *StreamController) Connect(ctx context.Context, baseURL string) error {
	return c.connect(ctx, baseURL, false)
}

func (c *StreamController) ConnectEndpoint(ctx context.Context, endpoint runtime.Endpoint) error {
	return c.connect(ctx, endpoint.URL(), false)
}

func (c *StreamController) connect(ctx context.Context, baseURL string, force bool) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()

	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if !force && state.baseURL == baseURL && state.connected {
		return nil
	}
	return c.replace(ctx, baseURL)
}

func (c *StreamController) Reconnect(ctx context.Context, baseURL string) error {
	return c.connect(ctx, baseURL, true)
}

func (c *StreamController) ReconnectEndpoint(ctx context.Context, endpoint runtime.Endpoint) error {
	return c.connect(ctx, endpoint.URL(), true)
}

func (c *StreamController) replace(ctx context.Context, baseURL string) error {
	if err := c.stopCurrent(ctx, true); err != nil {
		return err
	}
	if err := c.parent.Err(); err != nil {
		return err
	}

	streamCtx, cancel := context.WithCancel(c.parent)
	done := make(chan struct{})
	ready := make(chan struct{})
	c.mu.Lock()
	c.nextEpoch++
	epoch := c.nextEpoch
	c.state = streamState{epoch: epoch, baseURL: baseURL, cancel: cancel, done: done, ready: ready}
	c.mu.Unlock()
	go c.runGeneration(streamCtx, epoch, baseURL, ready, done)

	if err := waitForConnection(ctx, ready); err != nil {
		cancel()
		if stopErr := waitDoneAfterCancel(done); stopErr != nil {
			return errors.Join(fmt.Errorf("connect activity stream: %w", err), fmt.Errorf("stop failed activity stream: %w", stopErr))
		}
		return fmt.Errorf("connect activity stream: %w", err)
	}
	c.mu.Lock()
	connected := c.state.epoch == epoch && c.state.connected
	c.mu.Unlock()
	if !connected {
		cancel()
		if err := waitDoneAfterCancel(done); err != nil {
			return fmt.Errorf("activity stream disconnected during connection setup: %w", err)
		}
		c.clearState(epoch)
		return errors.New("activity stream disconnected during connection setup")
	}
	return nil
}

func (c *StreamController) runGeneration(ctx context.Context, epoch uint64, baseURL string, ready chan struct{}, done chan struct{}) {
	defer close(done)
	defer c.clearState(epoch)
	backoff := initialBackoff
	var readyOnce sync.Once
	for ctx.Err() == nil {
		start := time.Now()
		c.streamEvents(ctx, epoch, baseURL, &readyOnce, ready)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxBackoffReset {
			backoff = initialBackoff
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// streamEvents runs one connection attempt: it streams session.status frames
// as observations until the stream ends or ctx is canceled. On a stream end
// while ctx is still live it publishes the disconnected observation for the
// generation; the caller decides whether to reconnect. ready is closed once,
// on the first successful connect of this generation.
func (c *StreamController) streamEvents(ctx context.Context, epoch uint64, baseURL string, readyOnce *sync.Once, ready chan struct{}) {
	start := time.Now()
	attemptCtx, attemptCancel := context.WithCancel(ctx)
	defer attemptCancel()
	options := c.options
	options.BaseURL = baseURL
	onConnect := options.OnConnect
	options.OnConnect = func() {
		if !c.setConnected(epoch, true) {
			return
		}
		if onConnect != nil {
			onConnect()
		}
		if c.send(attemptCtx, Observation{Epoch: epoch, Kind: ObservationConnected}) {
			readyOnce.Do(func() { close(ready) })
		}
	}
	events := make(chan Event)
	streamDone := make(chan error, 1)
	go func() { streamDone <- Stream(attemptCtx, options, events) }()
	for {
		select {
		case event := <-events:
			if observation, ok := statusObservation(epoch, event); ok {
				c.send(ctx, observation)
			}
		case err := <-streamDone:
			attemptCancel()
			if ctx.Err() != nil {
				return
			}
			c.setConnected(epoch, false)
			c.send(ctx, Observation{Epoch: epoch, Kind: ObservationDisconnected, Err: err.Error()})
			c.log.Warn("event stream ended", "epoch", epoch, "err", err, "connected_for", time.Since(start).Round(time.Millisecond))
			return
		case <-ctx.Done():
			attemptCancel()
			waitStream(streamDone)
			return
		}
	}
}

func (c *StreamController) Stop(ctx context.Context) error {
	if err := c.acquireForStop(ctx); err != nil {
		return err
	}
	defer c.release()
	return c.stopCurrent(ctx, false)
}

func (c *StreamController) acquireForStop(ctx context.Context) error {
	select {
	case <-c.operations:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *StreamController) stopCurrent(ctx context.Context, publish bool) error {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	if state.cancel == nil {
		return nil
	}
	state.cancel()
	if publish {
		c.send(ctx, Observation{Epoch: state.epoch, Kind: ObservationDisconnected, Err: "stream generation replaced"})
	}
	if err := waitDone(ctx, state.done); err != nil {
		return err
	}
	c.clearState(state.epoch)
	return nil
}

func (c *StreamController) clearState(epoch uint64) {
	c.mu.Lock()
	if c.state.epoch == epoch {
		c.state = streamState{}
	}
	c.mu.Unlock()
}

func (c *StreamController) acquire(ctx context.Context) error {
	select {
	case <-c.operations:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.parent.Done():
		return c.parent.Err()
	}
}

func (c *StreamController) release() {
	c.operations <- struct{}{}
}

func (c *StreamController) setConnected(epoch uint64, connected bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.epoch == epoch {
		c.state.connected = connected
		return true
	}
	return false
}

func (c *StreamController) send(ctx context.Context, observation Observation) bool {
	select {
	case c.out <- observation:
		return true
	case <-ctx.Done():
		return false
	}
}

func statusObservation(epoch uint64, event Event) (Observation, bool) {
	if event.Type != "session.status" {
		return Observation{}, false
	}
	sessionID, status, ok := parseStatus(event)
	if !ok {
		return Observation{Epoch: epoch, Kind: ObservationInvalidated, Err: "malformed session.status event"}, true
	}
	if status != "idle" && status != "busy" && status != "retry" {
		return Observation{Epoch: epoch, Kind: ObservationInvalidated, Err: fmt.Sprintf("unknown session status %q", status)}, true
	}
	return Observation{Epoch: epoch, Kind: ObservationStatus, SessionID: sessionID, Status: status}, true
}

func waitForConnection(ctx context.Context, ready <-chan struct{}) error {
	if ready == nil {
		return errors.New("activity stream is not running")
	}
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("activity stream did not connect within %s", connectTimeout)
	}
}

func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitDoneAfterCancel(done <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	return waitDone(ctx, done)
}

// waitStream bounds the post-cancellation drain of a finished stream attempt
// with the same window as waitDoneAfterCancel, so a wedged decoder cannot
// stall generation teardown indefinitely.
func waitStream(done <-chan error) {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
