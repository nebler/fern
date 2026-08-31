//go:build !unix

package taskenvdocker

import (
	"context"
	"errors"
)

func (p *Provider) acquireCloneLock(context.Context, string) (func() error, error) {
	return nil, errors.New("background run Docker state requires Unix")
}
