package runtime

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type State string

const (
	StateAbsent       State = "absent"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StatePaused       State = "paused"
	StateFailed       State = "failed"
)

// SuspendKind selects how a domain Pause halts workspace compute. Stop is the
// default: a graceful docker stop, ~seconds to wake. Freeze uses Docker's
// cgroup freezer: the OpenCode process stays resident, so wake is an unpause
// measured in milliseconds — at the cost of retaining full resident memory
// while suspended.
type SuspendKind string

const (
	SuspendStop   SuspendKind = "stop"
	SuspendFreeze SuspendKind = "freeze"
)

// ShutdownIntentTTL is how long an orderly-shutdown recovery intent stays
// trustworthy after runtime records it. The registry applies a small negative
// skew against this expiry when classifying a stopped container, so the two
// packages agree on the contract without registry importing a runtime clock:
// see IntentStore.PauseStatus in internal/registry/intent.go.
const ShutdownIntentTTL = 5 * time.Minute

var (
	ErrUnmanaged = errors.New("resource is not managed by fern")
	ErrSpecDrift = errors.New("workspace configuration differs from the existing container")
	ErrFailed    = errors.New("workspace process failed")
)

// CommandResult carries one completed workspace command.
// stdout/stderr are best-effort partial output when the result carries an error.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type ServerAuth struct {
	Password string
}

type IntentStore interface {
	BeginPause(workspace, containerID string) error
	CommitPause(workspace, containerID string) error
	CommitShutdown(workspace, containerID string, expiresAt time.Time) error
	PauseStatus(workspace, containerID string, stoppedAt time.Time) (PauseIntentStatus, error)
	Clear(workspace string) error
}

type PauseIntentStatus uint8

const (
	PauseIntentNone PauseIntentStatus = iota
	PauseIntentPending
	PauseIntentCommitted
	PauseIntentShutdown
)

func (a ServerAuth) Apply(req interface{ SetBasicAuth(string, string) }) {
	if a.Password == "" {
		return
	}
	req.SetBasicAuth("opencode", a.Password)
}

type Spec struct {
	Name        string
	Image       string
	RepoPath    string
	MemoryBytes int64
	Env         map[string]string
	WorkspaceGH bool
}

func (s Spec) ServerAuth() ServerAuth {
	return ServerAuth{Password: s.Env["OPENCODE_PASSWORD"]}
}

// Endpoint is resolved after every start or resume. Callers must not persist it.
type Endpoint struct {
	Host string
	Port int
}

// StartupResult distinguishes an adopted or reconciled running workspace from
// one that should remain dormant until its first waking request. On error the
// result is the zero value; callers must not read fields alongside failures.
type StartupResult struct {
	Endpoint     Endpoint
	ImageID      string
	Running      bool
	Transitioned bool
}

// RunningResult carries facts attested by the same inspection that completed
// a wake. This keeps the endpoint and immutable image identity coherent.
// On error the result is the zero value; partial results never accompany a
// failure.
type RunningResult struct {
	Observation  Observation
	Transitioned bool
}

func (e Endpoint) URL() string {
	return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

func (s Spec) Validate() error {
	if s.Name == "" || s.Image == "" || s.RepoPath == "" || s.MemoryBytes <= 0 {
		return errors.New("name, image, repository path, and positive memory are required")
	}
	for key, value := range s.Env {
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return fmt.Errorf("invalid environment key %q", key)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment value %q contains NUL", key)
		}
	}
	if _, exists := s.Env[githubConfigEnv]; exists {
		return fmt.Errorf("%s is managed by Fern and cannot be configured", githubConfigEnv)
	}
	return nil
}

// Observation preserves runtime facts so policy can distinguish an intentional
// pause from a crash, OOM, restart, or foreign resource.
type Observation struct {
	State           State
	ContainerID     string
	ImageID         string
	DockerStatus    string
	Running         bool
	Frozen          bool
	OOMKilled       bool
	ExitCode        int
	Endpoint        Endpoint
	HasEndpoint     bool
	SpecFingerprint string
}
