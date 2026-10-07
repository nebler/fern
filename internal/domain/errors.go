package domain

import "errors"

var (
	ErrInvalidID             = errors.New("invalid ID")
	ErrInvalidActor          = errors.New("invalid actor snapshot")
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	ErrIDGeneration          = errors.New("ID generation failed")
)
