package taskstore

import (
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

var (
	ErrUnsafePath               = errors.New("unsafe database path")
	ErrUnsupportedSchema        = errors.New("unsupported database schema")
	ErrCorruptStore             = errors.New("corrupt task store")
	ErrNotFound                 = errors.New("task store record not found")
	ErrInvalidInput             = errors.New("invalid task store input")
	ErrWorkspaceUnavailable     = errors.New("workspace is not active")
	ErrRepositoryMismatch       = errors.New("workspace repository mismatch")
	ErrIdempotencyConflict      = errors.New("idempotency key conflict")
	ErrIdempotencyOwnerMismatch = errors.New("idempotency key owner mismatch")
	ErrInvalidState             = errors.New("invalid task store state")
)

// ConflictError identifies the original accepted command without disclosing
// request content.
type ConflictError struct {
	ReceiptID int64
	RunID     task.RunID
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v: receipt %d targets %s", ErrIdempotencyConflict, e.ReceiptID, e.RunID)
}

func (e *ConflictError) Unwrap() error { return ErrIdempotencyConflict }
