package docker

import (
	"errors"

	"github.com/nebler/fern/internal/domain"
)

// WriterFenceKind identifies the lifecycle assertion carried by a WriterFence.
type WriterFenceKind uint8

const (
	WriterFenceInvalid WriterFenceKind = iota
	WriterFenceNeverCreated
	WriterFenceCreatedNeverStarted
	WriterFenceStoppedRuntime
)

// WriterFence binds cleanup to one lifecycle assertion. Construction validates
// the identity, not the assertion: cleanup must freshly inspect the writer and
// its resources before acting. Its zero value is invalid.
type WriterFence struct {
	kind        WriterFenceKind
	containerID string
	runtime     domain.Runtime
}

func NeverCreatedAuthority() WriterFence {
	return WriterFence{kind: WriterFenceNeverCreated}
}

// CreatedContainerAuthority returns an invalid zero fence for an empty ID.
func CreatedContainerAuthority(containerID string) WriterFence {
	if containerID == "" {
		return WriterFence{}
	}
	return WriterFence{kind: WriterFenceCreatedNeverStarted, containerID: containerID}
}

// RuntimeCleanupAuthority returns an invalid zero fence for an invalid runtime.
// The caller asserts that this exact runtime has stopped; construction is not
// proof that the writer is inactive.
func RuntimeCleanupAuthority(runtime RuntimeIdentity) WriterFence {
	identity, err := domain.ParseRuntime(runtime.ContainerID, runtime.StartedAt, runtime.Token)
	if err != nil {
		return WriterFence{}
	}
	return WriterFence{kind: WriterFenceStoppedRuntime, runtime: identity}
}

func (a WriterFence) Kind() WriterFenceKind { return a.kind }
func (a WriterFence) ContainerID() string {
	if a.kind == WriterFenceStoppedRuntime {
		return a.runtime.ContainerID()
	}
	return a.containerID
}
func (a WriterFence) StartedAt() string                { return a.runtime.StartedAt() }
func (a WriterFence) Token() string                    { return a.runtime.Token() }
func (a WriterFence) runtimeIdentity() RuntimeIdentity { return runtimeFromIdentity(a.runtime) }

func validateCleanupAuthority(authority WriterFence) (WriterFenceKind, error) {
	emptyRuntime := authority.runtime == (domain.Runtime{})
	switch authority.kind {
	case WriterFenceNeverCreated:
		if authority.containerID == "" && emptyRuntime {
			return authority.kind, nil
		}
	case WriterFenceCreatedNeverStarted:
		if authority.containerID != "" && emptyRuntime {
			return authority.kind, nil
		}
	case WriterFenceStoppedRuntime:
		if authority.containerID == "" && validateCommittedRuntime(authority.runtimeIdentity()) == nil {
			return authority.kind, nil
		}
	}
	return WriterFenceInvalid, errors.New("cleanup authority must be never-created, an exact created container ID, or a full stopped runtime")
}
