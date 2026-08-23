package workspace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nebler/fern/internal/runtime"
)

var (
	ErrRequestsActive = errors.New("workspace has in-flight HTTP requests")
	ErrSessionsActive = errors.New("workspace sessions are not all idle")
	ErrNotRunning     = errors.New("workspace is not running")
	// ErrManagerClosed is returned once Close has begun. Callers treat it as a
	// terminal admission refusal, distinct from transient wake failures.
	ErrManagerClosed = errors.New("workspace manager is shutting down")
)

const (
	// defaultWakeTimeout bounds one coalesced wake: Docker start/resume, the
	// 60s health budget, and activity-observer attach must all fit inside it.
	defaultWakeTimeout = 90 * time.Second
)

type EndpointObserver func(context.Context, runtime.Endpoint, bool) error
type IdleChecker func(context.Context, runtime.Endpoint) (bool, error)
type RequestObserver func()

type RequestTarget struct {
	Endpoint   runtime.Endpoint
	ImageID    string
	Generation uint64
}

type RequestIntent uint8

const (
	RequestObserve RequestIntent = iota
	RequestRead
	RequestWork
)

type lifecycleRuntime interface {
	EnsureRunningObserved(context.Context, runtime.Spec) (runtime.RunningResult, error)
	ReconcileStartup(context.Context, runtime.Spec) (runtime.StartupResult, error)
	Pause(context.Context, string) error
	Status(context.Context, string) (runtime.Observation, error)
}

type wakeCall struct {
	done   chan struct{}
	target RequestTarget
	err    error
}

// Manager owns lifecycle policy for exactly one workspace. Request admission
// and pause share a gate, so no mutating request can cross an all-idle check.
//
// Concurrency model:
//
//   - wakeMu guards the cached endpoint, its generation, closing, and the
//     in-flight wake call.
//   - admissionMu guards pausing, inFlight, and the requestsDone/pauseDone
//     broadcast channels.
//   - Nesting rule: admissionMu may be held while acquiring wakeMu (admitRequest
//     → isClosing). No path acquires wakeMu and then admissionMu; Close takes
//     them sequentially, never nested.
//   - The lifecycle channel is a capacity-1 token serializing wake, pause, and
//     quiesce mutations against each other and against Close.
//   - Endpoint generations increase monotonically under wakeMu; invalidation
//     only ever discards the exact (endpoint, generation) pair observed by the
//     failed request, so a newer generation is never clobbered by stale news.
//   - Every blocking wait selects on ctx.Done() and serviceCtx.Done(), so
//     caller cancellation and process shutdown always release waiters.
type Manager struct {
	serviceCtx context.Context
	runtime    lifecycleRuntime
	spec       runtime.Spec
	observe    EndpointObserver
	allIdle    IdleChecker
	onRequest  RequestObserver

	wakeOperationTimeout time.Duration
	wakeMu               sync.Mutex
	wake                 *wakeCall
	endpoint             runtime.Endpoint
	imageID              string
	endpointGeneration   uint64
	nextGeneration       uint64
	hasEndpoint          bool
	closing              bool
	lastWake             WakeTrace
	hasLastWake          bool
	lifecycle            chan struct{}
	admissionMu          sync.Mutex
	pausing              bool
	pauseDone            chan struct{}
	inFlight             int
	requestsDone         chan struct{}
}

func NewManager(serviceCtx context.Context, rt lifecycleRuntime, spec runtime.Spec, observe EndpointObserver, allIdle IdleChecker, onRequest RequestObserver) *Manager {
	manager := &Manager{
		serviceCtx:           serviceCtx,
		runtime:              rt,
		spec:                 spec,
		observe:              observe,
		allIdle:              allIdle,
		onRequest:            onRequest,
		wakeOperationTimeout: defaultWakeTimeout,
		lifecycle:            make(chan struct{}, 1),
		requestsDone:         make(chan struct{}),
		pauseDone:            make(chan struct{}),
	}
	close(manager.requestsDone)
	close(manager.pauseDone)
	manager.lifecycle <- struct{}{}
	return manager
}

// AcquireRequest records request policy before wake begins. Read and work
// requests release after proxying; work requests also invalidate idle policy.
func (m *Manager) AcquireRequest(ctx context.Context, intent RequestIntent) (RequestTarget, func(), error) {
	if intent > RequestWork {
		return RequestTarget{}, func() {}, errors.New("invalid request intent")
	}
	if m.isClosing() {
		return RequestTarget{}, func() {}, ErrManagerClosed
	}
	release := func() {}
	if intent != RequestObserve {
		if err := m.admitRequest(ctx); err != nil {
			return RequestTarget{}, func() {}, err
		}
		var once sync.Once
		release = func() {
			once.Do(func() {
				m.admissionMu.Lock()
				m.inFlight--
				if m.inFlight == 0 {
					close(m.requestsDone)
				}
				m.admissionMu.Unlock()
			})
		}
	}
	var target RequestTarget
	var err error
	if intent != RequestObserve {
		target, err = m.ensureTarget(ctx)
	} else {
		target, err = m.runningTarget()
	}
	if err != nil {
		release()
		return RequestTarget{}, func() {}, err
	}
	if intent == RequestWork && m.onRequest != nil {
		m.onRequest()
	}
	return target, release, nil
}

func (m *Manager) EnsureRunning(ctx context.Context) (runtime.Endpoint, error) {
	target, err := m.ensureTarget(ctx)
	return target.Endpoint, err
}

// ReconcileStartup adopts compute that was running before service startup but
// preserves absent and intentionally paused workspaces as dormant.
func (m *Manager) ReconcileStartup(ctx context.Context) error {
	if err := m.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer m.releaseLifecycle()
	if m.isClosing() {
		return ErrManagerClosed
	}
	result, err := m.runtime.ReconcileStartup(ctx, m.spec)
	if err != nil {
		return err
	}
	if !result.Running {
		m.clearEndpoint()
		return nil
	}
	observation := runtime.Observation{
		State:       runtime.StateRunning,
		Running:     true,
		Endpoint:    result.Endpoint,
		HasEndpoint: result.Endpoint != (runtime.Endpoint{}),
		ImageID:     result.ImageID,
	}
	_, err = m.observeAndPublish(ctx, observation, result.Transitioned, nil)
	return err
}

func (m *Manager) ensureTarget(ctx context.Context) (RequestTarget, error) {
	m.wakeMu.Lock()
	if m.closing {
		m.wakeMu.Unlock()
		return RequestTarget{}, ErrManagerClosed
	}
	if m.hasEndpoint {
		target := RequestTarget{Endpoint: m.endpoint, ImageID: m.imageID, Generation: m.endpointGeneration}
		m.wakeMu.Unlock()
		return target, nil
	}
	call := m.wake
	if call == nil {
		call = &wakeCall{done: make(chan struct{})}
		m.wake = call
		go m.runWake(call)
	}
	m.wakeMu.Unlock()
	select {
	case <-ctx.Done():
		return RequestTarget{}, ctx.Err()
	case <-call.done:
		return call.target, call.err
	}
}

func (m *Manager) runWake(call *wakeCall) {
	wakeCtx, cancel := context.WithTimeout(m.serviceCtx, m.wakeOperationTimeout)
	collector := newTraceCollector(time.Now())
	traceCtx := runtime.WithSpanRecorder(wakeCtx, collector.spanRecorder())
	call.target, call.err = m.ensureRunning(traceCtx, collector)
	cancel()
	m.wakeMu.Lock()
	if m.wake == call {
		m.wake = nil
	}
	trace := collector.finish(time.Now())
	trace.Workspace = m.spec.Name
	m.hasLastWake = true
	m.lastWake = trace
	m.wakeMu.Unlock()
	close(call.done)
}

// LastWakeTrace returns the most recently completed coalesced wake. It reports
// false before the first wake and after process start with an already-running
// workspace (no wake occurred).
func (m *Manager) LastWakeTrace() (WakeTrace, bool) {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if !m.hasLastWake {
		return WakeTrace{}, false
	}
	return m.lastWake, true
}

func (m *Manager) runningTarget() (RequestTarget, error) {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.closing {
		return RequestTarget{}, ErrManagerClosed
	}
	if !m.hasEndpoint {
		return RequestTarget{}, ErrNotRunning
	}
	return RequestTarget{Endpoint: m.endpoint, ImageID: m.imageID, Generation: m.endpointGeneration}, nil
}

// InvalidateEndpoint discards a failed endpoint without disturbing a newer
// generation that may already have replaced it.
func (m *Manager) InvalidateEndpoint(target RequestTarget) {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.hasEndpoint && m.endpoint == target.Endpoint && m.endpointGeneration == target.Generation {
		m.endpoint = runtime.Endpoint{}
		m.imageID = ""
		m.endpointGeneration = 0
		m.hasEndpoint = false
	}
}

func (m *Manager) ensureRunning(ctx context.Context, collector *traceCollector) (RequestTarget, error) {
	tokenStart := time.Now()
	if err := m.acquireLifecycle(ctx); err != nil {
		return RequestTarget{}, err
	}
	collector.append("lifecycle_token", tokenStart)
	defer m.releaseLifecycle()
	if m.isClosing() {
		return RequestTarget{}, ErrManagerClosed
	}
	runtimeStart := time.Now()
	result, err := m.runtime.EnsureRunningObserved(ctx, m.spec)
	collector.append("runtime_total", runtimeStart)
	if err != nil {
		return RequestTarget{}, err
	}
	return m.observeAndPublish(ctx, result.Observation, result.Transitioned, collector)
}

func (m *Manager) observeAndPublish(ctx context.Context, observation runtime.Observation, transitioned bool, collector *traceCollector) (RequestTarget, error) {
	if observation.State != runtime.StateRunning || !observation.Running {
		return RequestTarget{}, errors.New("runtime did not attest a running workspace")
	}
	if !observation.HasEndpoint || observation.Endpoint == (runtime.Endpoint{}) {
		return RequestTarget{}, errors.New("running workspace has no attested endpoint")
	}
	if !runtime.ValidImageID(observation.ImageID) {
		return RequestTarget{}, errors.New("running workspace has no valid actual image ID")
	}
	if m.observe != nil {
		attachStart := time.Now()
		err := m.observe(ctx, observation.Endpoint, transitioned)
		collector.append("observer_attach", attachStart)
		if err != nil {
			if transitioned {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				pauseErr := m.runtime.Pause(cleanupCtx, m.spec.Name)
				cancel()
				return RequestTarget{}, errors.Join(fmt.Errorf("attach activity observer: %w", err), pauseErr)
			}
			return RequestTarget{}, fmt.Errorf("attach activity observer: %w", err)
		}
	}
	return m.publishEndpoint(observation.Endpoint, observation.ImageID), nil
}

func (m *Manager) publishEndpoint(endpoint runtime.Endpoint, imageID string) RequestTarget {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.closing {
		return RequestTarget{Endpoint: endpoint, ImageID: imageID}
	}
	m.nextGeneration++
	m.endpoint = endpoint
	m.imageID = imageID
	m.endpointGeneration = m.nextGeneration
	m.hasEndpoint = true
	return RequestTarget{Endpoint: endpoint, ImageID: imageID, Generation: m.endpointGeneration}
}

func (m *Manager) acquireLifecycleForClose(ctx context.Context) error {
	select {
	case <-m.lifecycle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Pause(ctx context.Context) error {
	release, err := m.AcquirePaused(ctx)
	if err != nil {
		return err
	}
	release()
	return nil
}

// AcquirePaused stops compute and keeps request admission and lifecycle wake
// serialization closed until release. It fences host-side repository changes
// from all manager-controlled container writers.
func (m *Manager) AcquirePaused(ctx context.Context) (func(), error) {
	// Holding admission prevents a new request from entering between
	// the authoritative status read and Docker stop.
	if err := m.beginPause(ctx); err != nil {
		return nil, err
	}
	if m.isClosing() {
		m.endPause()
		return nil, ErrManagerClosed
	}
	if err := m.acquireLifecycle(ctx); err != nil {
		m.endPause()
		return nil, err
	}
	if err := m.pauseWhileHeld(ctx); err != nil {
		m.releaseLifecycle()
		m.endPause()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.releaseLifecycle()
			m.endPause()
		})
	}, nil
}

// AcquireQuiesced closes request admission, performs two caller-owned exact
// observations against one attested running target, stops compute, and retains
// the lifecycle/repository fence until release. It is intended for result
// capture that must synchronize final OpenCode evidence with host Git state.
func (m *Manager) AcquireQuiesced(ctx context.Context, observe func(context.Context, RequestTarget) error) (func(), error) {
	if observe == nil {
		return nil, errors.New("quiesced observation is required")
	}
	if err := m.beginPause(ctx); err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		m.endPause()
		return nil, err
	}
	if m.isClosing() {
		return fail(ErrManagerClosed)
	}
	if err := m.acquireLifecycle(ctx); err != nil {
		return fail(err)
	}
	failHeld := func(err error) (func(), error) {
		m.releaseLifecycle()
		return fail(err)
	}
	observation, err := m.runtime.Status(ctx, m.spec.Name)
	if err != nil {
		return failHeld(err)
	}
	if observation.State != runtime.StateRunning || !observation.HasEndpoint || !runtime.ValidImageID(observation.ImageID) {
		return failHeld(errors.New("quiesced observation requires an attested running workspace"))
	}
	target, err := m.runningTarget()
	if err != nil || target.Endpoint != observation.Endpoint || target.ImageID != observation.ImageID {
		return failHeld(errors.New("quiesced target differs from current runtime observation"))
	}
	for range 2 {
		if err := observe(ctx, target); err != nil {
			return failHeld(err)
		}
		if m.allIdle == nil {
			return failHeld(errors.New("authoritative idle checker is required"))
		}
		idle, err := m.allIdle(ctx, target.Endpoint)
		if err != nil {
			return failHeld(err)
		}
		if !idle {
			return failHeld(ErrSessionsActive)
		}
	}
	if err := m.pauseRuntime(ctx); err != nil {
		return failHeld(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			m.releaseLifecycle()
			m.endPause()
		})
	}, nil
}

func (m *Manager) pauseWhileHeld(ctx context.Context) error {
	observation, err := m.runtime.Status(ctx, m.spec.Name)
	if err != nil {
		return err
	}
	if observation.State == runtime.StateProvisioning {
		if observation.Running || observation.Frozen {
			return errors.New("workspace is still provisioning; refusing pause without an idle snapshot")
		}
		return m.pauseRuntime(ctx)
	}
	if observation.State != runtime.StateRunning {
		m.clearEndpoint()
		return nil
	}
	if !observation.HasEndpoint {
		return errors.New("running workspace has no endpoint")
	}
	if m.allIdle == nil {
		return errors.New("authoritative idle checker is required")
	}
	// OpenCode exposes activity through separate endpoints rather than one
	// atomic snapshot. Require two clean passes while admission remains closed
	// so activity beginning during the first pass defers the stop.
	for range 2 {
		idle, err := m.allIdle(ctx, observation.Endpoint)
		if err != nil {
			return err
		}
		if !idle {
			return ErrSessionsActive
		}
	}
	return m.pauseRuntime(ctx)
}

func (m *Manager) pauseRuntime(ctx context.Context) error {
	m.wakeMu.Lock()
	target := RequestTarget{Endpoint: m.endpoint, ImageID: m.imageID, Generation: m.endpointGeneration}
	hasEndpoint := m.hasEndpoint
	m.wakeMu.Unlock()

	err := m.runtime.Pause(ctx, m.spec.Name)
	if hasEndpoint {
		m.InvalidateEndpoint(target)
	}
	return err
}

func (m *Manager) Close(ctx context.Context) error {
	m.wakeMu.Lock()
	m.closing = true
	m.endpoint = runtime.Endpoint{}
	m.imageID = ""
	m.endpointGeneration = 0
	m.hasEndpoint = false
	wake := m.wake
	m.wakeMu.Unlock()
	m.admissionMu.Lock()
	done := m.requestsDone
	active := m.inFlight != 0
	pauseDone := m.pauseDone
	pausing := m.pausing
	m.admissionMu.Unlock()
	if active {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if pausing {
		select {
		case <-pauseDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := m.acquireLifecycleForClose(ctx); err != nil {
		return err
	}
	m.releaseLifecycle()
	if wake == nil {
		return nil
	}
	select {
	case <-wake.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) PrepareShutdown(ctx context.Context) error {
	if preparer, ok := m.runtime.(interface {
		PrepareShutdown(context.Context, string) error
	}); ok {
		return preparer.PrepareShutdown(ctx, m.spec.Name)
	}
	return nil
}

func (m *Manager) acquireLifecycle(ctx context.Context) error {
	select {
	case <-m.lifecycle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.serviceCtx.Done():
		return m.serviceCtx.Err()
	}
}

func (m *Manager) releaseLifecycle() {
	m.lifecycle <- struct{}{}
}

func (m *Manager) isClosing() bool {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	return m.closing
}

func (m *Manager) admitRequest(ctx context.Context) error {
	for {
		m.admissionMu.Lock()
		if !m.pausing {
			if m.isClosing() {
				m.admissionMu.Unlock()
				return ErrManagerClosed
			}
			if m.inFlight == 0 {
				m.requestsDone = make(chan struct{})
			}
			m.inFlight++
			m.admissionMu.Unlock()
			return nil
		}
		done := m.pauseDone
		m.admissionMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.serviceCtx.Done():
			return m.serviceCtx.Err()
		}
	}
}

func (m *Manager) beginPause(ctx context.Context) error {
	for {
		m.admissionMu.Lock()
		if !m.pausing {
			if m.inFlight != 0 {
				m.admissionMu.Unlock()
				return ErrRequestsActive
			}
			m.pausing = true
			m.pauseDone = make(chan struct{})
			m.admissionMu.Unlock()
			return nil
		}
		done := m.pauseDone
		m.admissionMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.serviceCtx.Done():
			return m.serviceCtx.Err()
		}
	}
}

func (m *Manager) endPause() {
	m.admissionMu.Lock()
	m.pausing = false
	close(m.pauseDone)
	m.admissionMu.Unlock()
}

func (m *Manager) clearEndpoint() {
	m.wakeMu.Lock()
	m.endpoint = runtime.Endpoint{}
	m.imageID = ""
	m.endpointGeneration = 0
	m.hasEndpoint = false
	m.wakeMu.Unlock()
}
