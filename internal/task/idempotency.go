package task

import "fmt"

type IdempotencyKey string

func ParseIdempotencyKey(v string) (IdempotencyKey, error) {
	if len(v) < 1 || len(v) > 128 {
		return "", ErrInvalidIdempotencyKey
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return "", ErrInvalidIdempotencyKey
		}
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' {
		return "", ErrInvalidIdempotencyKey
	}
	return IdempotencyKey(v), nil
}

type IdempotencyScope struct {
	WorkspaceID WorkspaceID
	CommandKind string
}

func (s IdempotencyScope) Validate() error {
	if err := validateFernID(string(s.WorkspaceID), "wsp_"); err != nil {
		return err
	}
	if s.CommandKind == "" || len(s.CommandKind) > 128 || hasControl(s.CommandKind) {
		return fmt.Errorf("%w: command kind", ErrInvalidIdempotencyKey)
	}
	return nil
}

type IdempotencyClaim struct {
	Scope       IdempotencyScope
	Key         IdempotencyKey
	RequestHash RequestHash
	Actor       ActorSnapshot
}

type IdempotencyDisposition uint8

const (
	IdempotencyFirstUse IdempotencyDisposition = iota
	IdempotencyIndependent
	IdempotencyReplay
	IdempotencyConflict
	IdempotencyOwnerMismatch
)

// ClassifyIdempotency compares an accepted claim with a new command. The
// caller supplies hashes computed after strict decoding, default expansion,
// and RFC 8785 canonicalization. Actor mismatch takes precedence so hash
// equality does not disclose ownership information.
func ClassifyIdempotency(existing *IdempotencyClaim, incoming IdempotencyClaim) (IdempotencyDisposition, error) {
	if err := incoming.Validate(); err != nil {
		return 0, err
	}
	if existing == nil {
		return IdempotencyFirstUse, nil
	}
	if err := existing.Validate(); err != nil {
		return 0, fmt.Errorf("existing claim: %w", err)
	}
	if existing.Scope != incoming.Scope || existing.Key != incoming.Key {
		return IdempotencyIndependent, nil
	}
	if !existing.Actor.SameAuthority(incoming.Actor) {
		return IdempotencyOwnerMismatch, nil
	}
	if existing.RequestHash != incoming.RequestHash {
		return IdempotencyConflict, nil
	}
	return IdempotencyReplay, nil
}

func (c IdempotencyClaim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if _, err := ParseIdempotencyKey(string(c.Key)); err != nil {
		return err
	}
	if err := c.Actor.Validate(); err != nil {
		return err
	}
	return nil
}
