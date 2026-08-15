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
	done chan struct{}
	ep   runtime.Endpoint
	err  error
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
	closing              bool
	lifecycle            chan struct{}
	admission            chan struct{}
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
		admission:            make(chan struct{}, 1),
		requestsDone:         make(chan struct{}),
	}
	close(manager.requestsDone)
	manager.admission <- struct{}{}
	manager.lifecycle <- struct{}{}
	return manager
}

// AcquireRequest records request policy before wake begins. Read and work
// requests release after proxying; work requests also invalidate idle policy.
func (m *Manager) AcquireRequest(ctx context.Context, intent RequestIntent) (runtime.Endpoint, func(), error) {
	if intent > RequestWork {
		return runtime.Endpoint{}, func() {}, errors.New("invalid request intent")
	}
	if m.isClosing() {
		return runtime.Endpoint{}, func() {}, errors.New("workspace manager is shutting down")
	}
	release := func() {}
	if intent != RequestObserve {
		if err := m.acquireAdmission(ctx); err != nil {
			return runtime.Endpoint{}, func() {}, err
		}
		if m.isClosing() {
			m.releaseAdmission()
			return runtime.Endpoint{}, func() {}, errors.New("workspace manager is shutting down")
		}
		if m.inFlight == 0 {
			m.requestsDone = make(chan struct{})
		}
		m.inFlight++
		m.releaseAdmission()
		var once sync.Once
		release = func() {
			once.Do(func() {
				<-m.admission
				m.inFlight--
				if m.inFlight == 0 {
					close(m.requestsDone)
				}
				m.releaseAdmission()
			})
		}
	}
	var ep runtime.Endpoint
	var err error
	if intent != RequestObserve {
		ep, err = m.EnsureRunning(ctx)
	} else {
		ep, err = m.runningEndpoint(ctx)
	}
	if err != nil {
		release()
		return runtime.Endpoint{}, func() {}, err
	}
	if intent == RequestWork && m.onRequest != nil {
		m.onRequest()
	}
	return ep, release, nil
}

func (m *Manager) EnsureRunning(ctx context.Context) (runtime.Endpoint, error) {
	m.wakeMu.Lock()
	if m.closing {
		m.wakeMu.Unlock()
		return runtime.Endpoint{}, errors.New("workspace manager is shutting down")
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
		return runtime.Endpoint{}, ctx.Err()
	case <-call.done:
		return call.ep, call.err
	}
}

func (m *Manager) runWake(call *wakeCall) {
	wakeCtx, cancel := context.WithTimeout(m.serviceCtx, m.wakeOperationTimeout)
	call.ep, call.err = m.ensureRunning(wakeCtx)
	cancel()
	m.wakeMu.Lock()
	if m.wake == call {
		m.wake = nil
	}
	close(call.done)
	m.wakeMu.Unlock()
}

func (m *Manager) runningEndpoint(ctx context.Context) (runtime.Endpoint, error) {
	if err := m.acquireLifecycle(ctx); err != nil {
		return runtime.Endpoint{}, err
	}
	defer m.releaseLifecycle()
	if m.isClosing() {
		return runtime.Endpoint{}, errors.New("workspace manager is shutting down")
	}
	observation, err := m.runtime.Status(ctx, m.spec.Name)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	if observation.State != runtime.StateRunning || !observation.HasEndpoint {
		return runtime.Endpoint{}, ErrNotRunning
	}
	return observation.Endpoint, nil
}

func (m *Manager) ensureRunning(ctx context.Context) (runtime.Endpoint, error) {
	if err := m.acquireLifecycle(ctx); err != nil {
		return runtime.Endpoint{}, err
	}
	defer m.releaseLifecycle()
	if m.isClosing() {
		return runtime.Endpoint{}, errors.New("workspace manager is shutting down")
	}
	ep, transitioned, err := m.runtime.EnsureRunning(ctx, m.spec)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	if m.observe != nil {
		if err := m.observe(ctx, ep, transitioned); err != nil {
			if transitioned {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				pauseErr := m.runtime.Pause(cleanupCtx, m.spec.Name)
				cancel()
				return runtime.Endpoint{}, errors.Join(fmt.Errorf("attach activity observer: %w", err), pauseErr)
			}
			return runtime.Endpoint{}, fmt.Errorf("attach activity observer: %w", err)
		}
	}
	return ep, nil
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
	if err := m.acquireAdmission(ctx); err != nil {
		return err
	}
	defer m.releaseAdmission()
	if m.isClosing() {
		return errors.New("workspace manager is shutting down")
	}
	if m.inFlight != 0 {
		return ErrRequestsActive
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
		return m.runtime.Pause(ctx, m.spec.Name)
	}
	if observation.State != runtime.StateRunning {
		return nil
	}
	if !observation.HasEndpoint {
		return errors.New("running workspace has no endpoint")
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
	return m.runtime.Pause(ctx, m.spec.Name)
}

func (m *Manager) Close(ctx context.Context) error {
	m.wakeMu.Lock()
	m.closing = true
	wake := m.wake
	m.wakeMu.Unlock()
	select {
	case <-m.admission:
	case <-ctx.Done():
		return ctx.Err()
	}
	done := m.requestsDone
	active := m.inFlight != 0
	m.releaseAdmission()
	if active {
		select {
		case <-done:
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

func (m *Manager) acquireAdmission(ctx context.Context) error {
	select {
	case <-m.admission:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-m.serviceCtx.Done():
		return m.serviceCtx.Err()
	}
}

func (m *Manager) releaseAdmission() {
	m.admission <- struct{}{}
}

func (m *Manager) isClosing() bool {
	m.wakeMu.Lock()
	defer m.wakeMu.Unlock()
	return m.closing
}
