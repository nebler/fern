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
