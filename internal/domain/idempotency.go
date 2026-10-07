package domain

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

type IdempotencyRequest struct {
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

// ClassifyIdempotency compares an accepted request with a new command. Both
// requests were validated where they entered Fern (the HTTP boundary, or the
// receipt written from one). Actor mismatch takes precedence so hash equality
// does not disclose ownership information.
func ClassifyIdempotency(existing *IdempotencyRequest, incoming IdempotencyRequest) IdempotencyDisposition {
	switch {
	case existing == nil:
		return IdempotencyFirstUse
	case existing.Scope != incoming.Scope || existing.Key != incoming.Key:
		return IdempotencyIndependent
	case !existing.Actor.SameAuthority(incoming.Actor):
		return IdempotencyOwnerMismatch
	case existing.RequestHash != incoming.RequestHash:
		return IdempotencyConflict
	default:
		return IdempotencyReplay
	}
}
