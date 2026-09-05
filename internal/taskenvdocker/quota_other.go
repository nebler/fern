//go:build !linux || (!amd64 && !arm64)

package taskenvdocker

import (
	"context"
	"errors"
)

func inspectProjectTree(context.Context, string, quotaIdentity) error {
	return errors.New("quota-backed execution requires Linux amd64 or arm64")
}

func inspectProjectQuota(string) (quotaIdentity, error) {
	return quotaIdentity{}, errors.New("quota-backed execution requires Linux; Docker Desktop and other hosts are unsupported")
}
