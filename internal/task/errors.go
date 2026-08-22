package task

import "errors"

var (
	ErrInvalidID             = errors.New("invalid ID")
	ErrInvalidHash           = errors.New("invalid hash")
	ErrInvalidState          = errors.New("invalid state")
	ErrInvalidTransition     = errors.New("invalid transition")
	ErrInvalidTuple          = errors.New("invalid immutable tuple")
	ErrInvalidActor          = errors.New("invalid actor snapshot")
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrInvalidCursor         = errors.New("invalid cursor")
	ErrIDGeneration          = errors.New("ID generation failed")
)
