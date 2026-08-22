package task

import (
	"fmt"
	"unicode/utf8"
)

type ActorType string

const (
	ActorDevice    ActorType = "device"
	ActorOperator  ActorType = "operator"
	ActorSystem    ActorType = "system"
	ActorOpenCode  ActorType = "opencode"
	ActorGitHubApp ActorType = "github_app"
	ActorRecovery  ActorType = "recovery"
)

const (
	MaxActorIDBytes             = 256
	MaxActorDisplayNameBytes    = 200
	MaxActorCredentialIDBytes   = 256
	MaxActorAuthenticationBytes = 128
	MaxActorRequestIDBytes      = 128
)

// ActorSnapshot is immutable attribution captured at an authentication
// boundary. TASK_MODEL.md does not assign field-specific bounds; these limits
// are contract assumptions intended to keep every snapshot predictably bounded.
type ActorSnapshot struct {
	Type           ActorType
	ID             string
	DisplayName    string
	CredentialID   string
	Authentication string
	RequestID      string
}

func (a ActorSnapshot) Validate() error {
	if !a.Type.Valid() {
		return fmt.Errorf("%w: type", ErrInvalidActor)
	}
	fields := []struct {
		name, value string
		max         int
		optional    bool
	}{
		{"id", a.ID, MaxActorIDBytes, false},
		{"display name", a.DisplayName, MaxActorDisplayNameBytes, true},
		{"credential ID", a.CredentialID, MaxActorCredentialIDBytes, false},
		{"authentication", a.Authentication, MaxActorAuthenticationBytes, false},
		{"request ID", a.RequestID, MaxActorRequestIDBytes, false},
	}
	for _, f := range fields {
		if (!f.optional && f.value == "") || len(f.value) > f.max || !utf8.ValidString(f.value) || hasControl(f.value) {
			return fmt.Errorf("%w: %s", ErrInvalidActor, f.name)
		}
	}
	return nil
}

func (t ActorType) Valid() bool {
	switch t {
	case ActorDevice, ActorOperator, ActorSystem, ActorOpenCode, ActorGitHubApp, ActorRecovery:
		return true
	default:
		return false
	}
}

func hasControl(v string) bool {
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// SameAuthority compares authenticated authority, excluding mutable display
// text and the per-request request ID. TASK_MODEL.md does not define authority
// equivalence across credential rotation; this contract conservatively treats
// credential ID or authentication-method changes as a different authority.
func (a ActorSnapshot) SameAuthority(b ActorSnapshot) bool {
	return a.Type == b.Type && a.ID == b.ID && a.CredentialID == b.CredentialID && a.Authentication == b.Authentication
}
