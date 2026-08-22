package taskstore

import (
	"errors"
	"fmt"

	"github.com/nebler/fern/internal/task"
)

var (
	ErrUnsafePath                   = errors.New("unsafe database path")
	ErrUnsupportedSchema            = errors.New("unsupported database schema")
	ErrMigrationDrift               = errors.New("database migration drift")
	ErrCorruptStore                 = errors.New("corrupt task store")
	ErrNotFound                     = errors.New("task store record not found")
	ErrInvalidInput                 = errors.New("invalid task store input")
	ErrWorkspaceUnavailable         = errors.New("workspace is not active")
	ErrRepositoryMismatch           = errors.New("workspace repository mismatch")
	ErrIdempotencyConflict          = errors.New("idempotency key conflict")
	ErrIdempotencyOwnerMismatch     = errors.New("idempotency key owner mismatch")
	ErrInvalidState                 = errors.New("invalid task store state")
	ErrLeaseConflict                = errors.New("delivery lease conflict")
	ErrStaleRevision                = errors.New("stale task store revision")
	ErrWorkspaceBusy                = errors.New("workspace already has an effecting attempt")
	ErrCancellationAlreadyRequested = errors.New("task cancellation already requested")
	ErrTaskAlreadyTerminal          = errors.New("task is already terminal")
)

type CancellationAlreadyRequestedError struct {
	TaskID    task.TaskID
	ReceiptID task.ReceiptID
}

func (e *CancellationAlreadyRequestedError) Error() string {
	return fmt.Sprintf("%v: task %s receipt %s", ErrCancellationAlreadyRequested, e.TaskID, e.ReceiptID)
}

func (e *CancellationAlreadyRequestedError) Unwrap() error { return ErrCancellationAlreadyRequested }

type TerminalTaskError struct {
	TaskID task.TaskID
	State  task.TaskState
}

func (e *TerminalTaskError) Error() string {
	return fmt.Sprintf("%v: task %s is %s", ErrTaskAlreadyTerminal, e.TaskID, e.State)
}

func (e *TerminalTaskError) Unwrap() error { return ErrTaskAlreadyTerminal }

type StateError struct {
	AttemptID task.AttemptID
	State     task.AttemptState
	Required  task.AttemptState
}

func (e *StateError) Error() string {
	return fmt.Sprintf("%v: attempt %s is %s, requires %s", ErrInvalidState, e.AttemptID, e.State, e.Required)
}

func (e *StateError) Unwrap() error { return ErrInvalidState }

type LeaseConflictError struct{ AttemptID task.AttemptID }

func (e *LeaseConflictError) Error() string {
	return fmt.Sprintf("%v: attempt %s", ErrLeaseConflict, e.AttemptID)
}

func (e *LeaseConflictError) Unwrap() error { return ErrLeaseConflict }

type StaleRevisionError struct {
	AttemptID task.AttemptID
	Expected  int64
	Actual    int64
}

func (e *StaleRevisionError) Error() string {
	return fmt.Sprintf("%v: attempt %s expected %d, actual %d", ErrStaleRevision, e.AttemptID, e.Expected, e.Actual)
}

func (e *StaleRevisionError) Unwrap() error { return ErrStaleRevision }

type StaleTaskRevisionError struct {
	TaskID   task.TaskID
	Expected int64
	Actual   int64
}

func (e *StaleTaskRevisionError) Error() string {
	return fmt.Sprintf("%v: task %s expected %d, actual %d", ErrStaleRevision, e.TaskID, e.Expected, e.Actual)
}

func (e *StaleTaskRevisionError) Unwrap() error { return ErrStaleRevision }

type StaleJournalRevisionError struct {
	Kind     string
	ID       string
	Expected int64
	Actual   int64
}

func (e *StaleJournalRevisionError) Error() string {
	return fmt.Sprintf("%v: %s %s expected %d, actual %d", ErrStaleRevision, e.Kind, e.ID, e.Expected, e.Actual)
}

func (e *StaleJournalRevisionError) Unwrap() error { return ErrStaleRevision }

type WorkspaceBusyError struct {
	WorkspaceID task.WorkspaceID
	AttemptID   task.AttemptID
}

func (e *WorkspaceBusyError) Error() string {
	return fmt.Sprintf("%v: workspace %s attempt %s", ErrWorkspaceBusy, e.WorkspaceID, e.AttemptID)
}

func (e *WorkspaceBusyError) Unwrap() error { return ErrWorkspaceBusy }

type NotFoundError struct {
	Kind string
	ID   string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("%v: %s %s", ErrNotFound, e.Kind, e.ID) }
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

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
