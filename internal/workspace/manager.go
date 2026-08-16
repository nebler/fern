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
)

type EndpointObserver func(context.Context, runtime.Endpoint, bool) error
type IdleChecker func(context.Context, runtime.Endpoint) (bool, error)
type RequestObserver func()

type RequestTarget struct {
	Endpoint   runtime.Endpoint
	Generation uint64
}

type RequestIntent uint8

const (
	RequestObserve RequestIntent = iota
	RequestRead
	RequestWork
)

type lifecycleRuntime interface {
	EnsureRunning(context.Context, runtime.Spec) (runtime.Endpoint, bool, error)
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
	endpointGeneration   uint64
	nextGeneration       uint64
	hasEndpoint          bool
	closing              bool
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
		wakeOperationTimeout: 90 * time.Second,
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
		return RequestTarget{}, func() {}, errors.New("workspace manager is shutting down")
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

func (m *Manager) ensureTarget(ctx context.Context) (RequestTarget, error) {
	m.wakeMu.Lock()
	if m.closing {
		m.wakeMu.Unlock()
		return RequestTarget{}, errors.New("workspace manager is shutting down")
	}
	if m.hasEndpoint {
		target := RequestTarget{Endpoint: m.endpoint, Generation: m.endpointGeneration}
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
	call.target, call.err = m.ensureRunning(wakeCtx)
	cancel()
	m.wakeMu.Lock()
	if m.wake == call {
		m.wake = nil
	}
	close(call.done)
	m.wakeMu.Unlock()
}

func (m *Manager) runningTarget() (RequestTarget, error) {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.closing {
		return RequestTarget{}, errors.New("workspace manager is shutting down")
	}
	if !m.hasEndpoint {
		return RequestTarget{}, ErrNotRunning
	}
	return RequestTarget{Endpoint: m.endpoint, Generation: m.endpointGeneration}, nil
}

// InvalidateEndpoint discards a failed endpoint without disturbing a newer
// generation that may already have replaced it.
func (m *Manager) InvalidateEndpoint(target RequestTarget) {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.hasEndpoint && m.endpoint == target.Endpoint && m.endpointGeneration == target.Generation {
		m.endpoint = runtime.Endpoint{}
		m.endpointGeneration = 0
		m.hasEndpoint = false
	}
}

func (m *Manager) ensureRunning(ctx context.Context) (RequestTarget, error) {
	if err := m.acquireLifecycle(ctx); err != nil {
		return RequestTarget{}, err
	}
	defer m.releaseLifecycle()
	if m.isClosing() {
		return RequestTarget{}, errors.New("workspace manager is shutting down")
	}
	ep, transitioned, err := m.runtime.EnsureRunning(ctx, m.spec)
	if err != nil {
		return RequestTarget{}, err
	}
	if m.observe != nil {
		if err := m.observe(ctx, ep, transitioned); err != nil {
			if transitioned {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				pauseErr := m.runtime.Pause(cleanupCtx, m.spec.Name)
				cancel()
				return RequestTarget{}, errors.Join(fmt.Errorf("attach activity observer: %w", err), pauseErr)
			}
			return RequestTarget{}, fmt.Errorf("attach activity observer: %w", err)
		}
	}
	return m.publishEndpoint(ep), nil
}

func (m *Manager) publishEndpoint(endpoint runtime.Endpoint) RequestTarget {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	if m.closing {
		return RequestTarget{Endpoint: endpoint}
	}
	m.nextGeneration++
	m.endpoint = endpoint
	m.endpointGeneration = m.nextGeneration
	m.hasEndpoint = true
	return RequestTarget{Endpoint: endpoint, Generation: m.endpointGeneration}
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
	// Holding admission prevents a new request from entering between
	// the authoritative status read and Docker stop.
	if err := m.beginPause(ctx); err != nil {
		return err
	}
	defer m.endPause()
	if m.isClosing() {
		return errors.New("workspace manager is shutting down")
	}
	if err := m.acquireLifecycle(ctx); err != nil {
		return err
	}
	defer m.releaseLifecycle()

	observation, err := m.runtime.Status(ctx, m.spec.Name)
	if err != nil {
		return err
	}
	if observation.State == runtime.StateProvisioning {
		return m.pauseRuntime(ctx)
	}
	if observation.State != runtime.StateRunning {
		m.clearEndpoint()
		return nil
	}
	if !observation.HasEndpoint {
		return errors.New("running workspace has no endpoint")
	}
	m.wakeMu.Lock()
	if m.hasEndpoint && m.endpoint.Host == observation.Endpoint.Host && m.endpoint.Port == observation.Endpoint.Port {
		observation.Endpoint.Protocol = m.endpoint.Protocol
	}
	m.wakeMu.Unlock()
	if observation.Endpoint.Protocol == "" {
		if m.spec.Protocol.Normalize() == runtime.ProtocolAuto {
			return errors.New("running workspace protocol has not been negotiated")
		}
		observation.Endpoint.Protocol = m.spec.Protocol.Normalize()
	}
	if m.allIdle == nil {
		return errors.New("authoritative idle checker is required")
	}
	idle, err := m.allIdle(ctx, observation.Endpoint)
	if err != nil {
		return err
	}
	if !idle {
		return ErrSessionsActive
	}
	return m.pauseRuntime(ctx)
}

func (m *Manager) pauseRuntime(ctx context.Context) error {
	m.wakeMu.Lock()
	target := RequestTarget{Endpoint: m.endpoint, Generation: m.endpointGeneration}
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
				return errors.New("workspace manager is shutting down")
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
	m.endpointGeneration = 0
	m.hasEndpoint = false
	m.wakeMu.Unlock()
}
