package backgroundopencode

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfig = errors.New("invalid Background Run OpenCode client configuration")
	ErrDeadline      = errors.New("Background Run OpenCode operation requires a deadline")
	ErrNotFound      = errors.New("Background Run OpenCode object not found")
	ErrConflict      = errors.New("Background Run OpenCode object conflicts")
	ErrTransport     = errors.New("Background Run OpenCode transport failed")
	ErrProtocol      = errors.New("Background Run OpenCode protocol violation")
	ErrScanBound     = errors.New("Background Run OpenCode history scan exceeded its bound")
)

// NotFoundError omits the endpoint and external identity deliberately.
type NotFoundError struct{ operation string }

func (e *NotFoundError) Error() string {
	return "Background Run OpenCode " + e.operation + ": object not found"
}
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// ConflictError is returned only for an HTTP 409 from a one-shot mutation.
type ConflictError struct{ operation string }

func (e *ConflictError) Error() string {
	return "Background Run OpenCode " + e.operation + ": conflict"
}
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// TransportError contains only a local bounded category. The underlying
// transport error is deliberately not retained or unwrapped.
type TransportError struct {
	operation string
	kind      string
}

func (e *TransportError) Error() string {
	return "Background Run OpenCode " + e.operation + ": transport failed"
}
func (e *TransportError) Is(target error) bool { return target == ErrTransport }

// ProtocolError describes only the local invariant that failed. It never embeds
// upstream bytes, identities, origins, prompt text, or credentials.
type ProtocolError struct {
	operation string
	reason    string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("Background Run OpenCode %s: protocol violation (%s)", e.operation, e.reason)
}
func (e *ProtocolError) Is(target error) bool { return target == ErrProtocol }

func protocol(operation, reason string) error {
	return &ProtocolError{operation: operation, reason: reason}
}
