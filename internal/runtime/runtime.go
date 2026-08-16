package runtime

import (
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
	Protocol   Protocol
	Username   string
	Password   string
	V2Password string
}

type IntentStore interface {
	BeginPause(workspace, containerID string) error
	CommitPause(workspace, containerID string) error
	PauseStatus(workspace, containerID string) (PauseIntentStatus, error)
	Clear(workspace string) error
}

type PauseIntentStatus uint8

const (
	PauseIntentNone PauseIntentStatus = iota
	PauseIntentPending
	PauseIntentCommitted
)

func (a ServerAuth) Apply(req interface{ SetBasicAuth(string, string) }) {
	a.ApplyFor(req, a.Protocol.Normalize())
}

func (a ServerAuth) ApplyFor(req interface{ SetBasicAuth(string, string) }, protocol Protocol) {
	username, password := a.Credentials(protocol)
	if password == "" {
		return
	}
	req.SetBasicAuth(username, password)
}

func (a ServerAuth) Credentials(protocol Protocol) (string, string) {
	if protocol.Normalize() == ProtocolV2 {
		return "opencode", a.V2Password
	}
	username := a.Username
	if username == "" {
		username = "opencode"
	}
	return username, a.Password
}

type Spec struct {
	Name        string
	Image       string
	RepoPath    string
	MemoryBytes int64
	Protocol    Protocol
	Env         map[string]string
}

func (s Spec) ServerAuth() ServerAuth {
	return ServerAuth{
		Protocol:   s.Protocol.Normalize(),
		Username:   s.Env["OPENCODE_SERVER_USERNAME"],
		Password:   s.Env["OPENCODE_SERVER_PASSWORD"],
		V2Password: s.Env["OPENCODE_PASSWORD"],
	}
}

// Endpoint is resolved after every start or resume. Callers must not persist it.
type Endpoint struct {
	Host     string
	Port     int
	Protocol Protocol
}

func (e Endpoint) URL() string {
	return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

func (s Spec) Validate() error {
	if s.Name == "" || s.Image == "" || s.RepoPath == "" || s.MemoryBytes <= 0 {
		return errors.New("name, image, repository path, and positive memory are required")
	}
	if err := s.Protocol.Validate(); err != nil {
		return err
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
