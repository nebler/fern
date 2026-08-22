package opencodeapi

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfiguration = errors.New("invalid OpenCode client configuration")
	ErrDeadlineRequired     = errors.New("OpenCode request requires a deadline")
	ErrRequestTooLarge      = errors.New("OpenCode request exceeds the size limit")
	ErrResponseTooLarge     = errors.New("OpenCode response exceeds the size limit")
	ErrInvalidResponse      = errors.New("OpenCode returned an invalid response")
	ErrRequestFailed        = errors.New("OpenCode request failed")
	ErrNotFound             = errors.New("OpenCode object not found")
	ErrConflict             = errors.New("OpenCode request conflicted")
	ErrProtocolConflict     = errors.New("OpenCode protocol conflict")
	ErrScanLimit            = errors.New("OpenCode finite scan exceeded its bound")
)

// StatusError deliberately omits the response body, URL, and credentials.
type StatusError struct {
	statusCode int
}

func (err *StatusError) Error() string {
	return fmt.Sprintf("OpenCode request failed with HTTP status %d", err.statusCode)
}

func (err *StatusError) StatusCode() int { return err.statusCode }

func (err *StatusError) Is(target error) bool {
	switch target {
	case ErrRequestFailed:
		return true
	case ErrNotFound:
		return err.statusCode == 404
	case ErrConflict:
		return err.statusCode == 409
	default:
		return false
	}
}

// ProtocolError reports a local contract violation without including upstream
// data, IDs, URLs, prompts, or credentials.
type ProtocolError struct {
	kind string
}

func (err *ProtocolError) Error() string {
	return "OpenCode protocol conflict: " + err.kind
}

func (err *ProtocolError) Is(target error) bool { return target == ErrProtocolConflict }

func protocolError(kind string) error { return &ProtocolError{kind: kind} }
