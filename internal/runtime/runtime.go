package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

type State string

const (
	StateAbsent       State = "absent"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StatePaused       State = "paused"
	StateFailed       State = "failed"
)

var (
	ErrUnmanaged = errors.New("resource is not managed by fern")
	ErrSpecDrift = errors.New("workspace configuration differs from the existing container")
	ErrFailed    = errors.New("workspace process failed")
)

type ServerAuth struct {
	Username string
	Password string
}

type IntentStore interface {
	BeginPause(workspace, containerID string) error
	CommitPause(workspace, containerID string) error
	IsPaused(workspace, containerID string) (bool, error)
	Clear(workspace string) error
}

func (a ServerAuth) Apply(req interface{ SetBasicAuth(string, string) }) {
	if a.Password == "" {
		return
	}
	username := a.Username
	if username == "" {
		username = "opencode"
	}
	req.SetBasicAuth(username, a.Password)
}

type Spec struct {
	Name        string
	Image       string
	RepoPath    string
	MemoryBytes int64
	Env         map[string]string
}

func (s Spec) ServerAuth() ServerAuth {
	return ServerAuth{Username: s.Env["OPENCODE_SERVER_USERNAME"], Password: s.Env["OPENCODE_SERVER_PASSWORD"]}
}

// Endpoint is resolved after every start or resume. Callers must not persist it.
type Endpoint struct {
	Host string
	Port int
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
	return nil
}

// Observation preserves runtime facts so policy can distinguish an intentional
// pause from a crash, OOM, restart, or foreign resource.
type Observation struct {
	State           State
	ContainerID     string
	DockerStatus    string
	Running         bool
	Frozen          bool
	OOMKilled       bool
	ExitCode        int
	Endpoint        Endpoint
	HasEndpoint     bool
	SpecFingerprint string
}

type Runtime interface {
	Create(ctx context.Context, spec Spec) (Endpoint, error)
	Pause(ctx context.Context, name string) error
	Resume(ctx context.Context, spec Spec) (Endpoint, error)
	Destroy(ctx context.Context, name string) error
	Status(ctx context.Context, name string) (Observation, error)
}
