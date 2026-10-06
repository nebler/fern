package task

import "errors"

var (
	ErrInvalidID             = errors.New("invalid ID")
	ErrInvalidTuple          = errors.New("invalid immutable tuple")
	ErrInvalidActor          = errors.New("invalid actor snapshot")
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrInvalidCursor         = errors.New("invalid cursor")
	ErrIDGeneration          = errors.New("ID generation failed")
)
