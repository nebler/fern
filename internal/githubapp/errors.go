package githubapp

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfiguration    = errors.New("invalid GitHub App configuration")
	ErrInvalidIdentity         = errors.New("invalid GitHub installation identity")
	ErrInsufficientPermissions = errors.New("GitHub installation lacks required permissions")
	ErrSigningFailed           = errors.New("GitHub App JWT signing failed")
	ErrDeadlineRequired        = errors.New("GitHub App request requires a deadline")
	ErrRequestFailed           = errors.New("GitHub App request failed")
	ErrInvalidResponse         = errors.New("GitHub returned an invalid response")
	ErrResponseTooLarge        = errors.New("GitHub response exceeds the size limit")
	ErrTokenExpired            = errors.New("GitHub installation token is expired")
)

// HTTPError reports only the response status. Response bodies may contain
// credentials or other sensitive details and are deliberately omitted.
type HTTPError struct {
	statusCode int
}

func (err *HTTPError) Error() string {
	return fmt.Sprintf("GitHub App request failed with HTTP status %d", err.statusCode)
}

func (err *HTTPError) StatusCode() int {
	return err.statusCode
}

func (err *HTTPError) Is(target error) bool {
	return target == ErrRequestFailed
}
