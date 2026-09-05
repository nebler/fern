package run

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// Runtime is an exact process identity. The original canonical timestamp bytes
// are retained: normalizing offsets or precision would change persisted tokens.
// The zero value is invalid.
type Runtime struct {
	containerID, startedAt, token string
	epoch                         int64
}

func NewRuntime(containerID, startedAt string) (Runtime, error) {
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if containerID == "" || err != nil || started.IsZero() || started.Format(time.RFC3339Nano) != startedAt {
		return Runtime{}, errors.New("exact canonical runtime identity is required")
	}
	sum := sha256.Sum256([]byte(containerID + "\x00" + startedAt))
	return Runtime{containerID: containerID, startedAt: startedAt, token: hex.EncodeToString(sum[:]), epoch: started.UnixNano()}, nil
}

// ParseRuntime validates a token received at a persistence or wire boundary.
func ParseRuntime(containerID, startedAt, token string) (Runtime, error) {
	runtime, err := NewRuntime(containerID, startedAt)
	if err != nil {
		return Runtime{}, err
	}
	if token != runtime.token {
		return Runtime{}, errors.New("runtime token differs from exact process identity")
	}
	return runtime, nil
}

func (r Runtime) ContainerID() string { return r.containerID }
func (r Runtime) StartedAt() string   { return r.startedAt }
func (r Runtime) Token() string       { return r.token }

// Epoch is the canonical start timestamp expressed as Unix nanoseconds, not a
// process generation counter. Token also binds the original timestamp bytes.
func (r Runtime) Epoch() int64 { return r.epoch }
