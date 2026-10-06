package taskstore

import (
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

var (
	ErrUnsafePath               = errors.New("unsafe database path")
	ErrUnsupportedSchema        = errors.New("unsupported database schema")
	ErrMigrationDrift           = errors.New("database migration drift")
	ErrCorruptStore             = errors.New("corrupt task store")
	ErrNotFound                 = errors.New("task store record not found")
	ErrInvalidInput             = errors.New("invalid task store input")
	ErrWorkspaceUnavailable     = errors.New("workspace is not active")
	ErrRepositoryMismatch       = errors.New("workspace repository mismatch")
	ErrIdempotencyConflict      = errors.New("idempotency key conflict")
	ErrIdempotencyOwnerMismatch = errors.New("idempotency key owner mismatch")
	ErrInvalidState             = errors.New("invalid task store state")
	ErrStaleRevision            = errors.New("stale task store revision")
)

// ConflictError identifies the original accepted command without disclosing
// request content.
type ConflictError struct {
	ReceiptID task.ReceiptID
	TargetID  task.TaskID
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v: receipt %s targets %s", ErrIdempotencyConflict, e.ReceiptID, e.TargetID)
}

func (e *ConflictError) Unwrap() error { return ErrIdempotencyConflict }
