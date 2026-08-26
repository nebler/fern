package taskstore

import (
	"errors"
	"testing"

	"github.com/nebler/fern/internal/task"
)

func TestTypedErrorsDescribeAndUnwrapCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		cause   error
		message string
	}{
		{"cancellation requested", &CancellationAlreadyRequestedError{TaskID: task.TaskID("task-1"), ReceiptID: task.ReceiptID("receipt-1")}, ErrCancellationAlreadyRequested, "task cancellation already requested: task task-1 receipt receipt-1"},
		{"terminal task", &TerminalTaskError{TaskID: task.TaskID("task-1"), State: task.TaskFailed}, ErrTaskAlreadyTerminal, "task is already terminal: task task-1 is failed"},
		{"attempt state", &StateError{AttemptID: task.AttemptID("attempt-1"), State: task.AttemptPrepared, Required: task.AttemptRunning}, ErrInvalidState, "invalid task store state: attempt attempt-1 is prepared, requires running"},
		{"lease conflict", &LeaseConflictError{AttemptID: task.AttemptID("attempt-1")}, ErrLeaseConflict, "delivery lease conflict: attempt attempt-1"},
		{"attempt revision", &StaleRevisionError{AttemptID: task.AttemptID("attempt-1"), Expected: 2, Actual: 3}, ErrStaleRevision, "stale task store revision: attempt attempt-1 expected 2, actual 3"},
		{"task revision", &StaleTaskRevisionError{TaskID: task.TaskID("task-1"), Expected: 2, Actual: 3}, ErrStaleRevision, "stale task store revision: task task-1 expected 2, actual 3"},
		{"journal revision", &StaleJournalRevisionError{Kind: "publication", ID: "pub-1", Expected: 2, Actual: 3}, ErrStaleRevision, "stale task store revision: publication pub-1 expected 2, actual 3"},
		{"workspace busy", &WorkspaceBusyError{WorkspaceID: task.WorkspaceID("workspace-1"), AttemptID: task.AttemptID("attempt-1")}, ErrWorkspaceBusy, "workspace already has an effecting attempt: workspace workspace-1 attempt attempt-1"},
		{"not found", &NotFoundError{Kind: "task", ID: "task-1"}, ErrNotFound, "task store record not found: task task-1"},
		{"idempotency conflict", &ConflictError{ReceiptID: task.ReceiptID("receipt-1"), TargetID: task.TaskID("task-1")}, ErrIdempotencyConflict, "idempotency key conflict: receipt receipt-1 targets task-1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.message {
				t.Fatalf("Error() = %q, want %q", got, test.message)
			}
			if !errors.Is(test.err, test.cause) {
				t.Fatalf("errors.Is(%v, %v) = false", test.err, test.cause)
			}
		})
	}
}
