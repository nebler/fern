// Package observability provides bounded in-process service telemetry and retry scheduling.
package observability

import (
	"context"
	"math/rand/v2"
	"time"
)

// Retry produces equal-jitter exponential delays. It is owned by one service
// goroutine and is not safe for concurrent use.
type Retry struct {
	base     time.Duration
	maximum  time.Duration
	failures uint
	random   func(uint64) uint64
}

func NewRetry(base, maximum time.Duration) *Retry {
	if base <= 0 {
		base = time.Second
	}
	if maximum < base {
		maximum = base
	}
	return &Retry{base: base, maximum: maximum, random: rand.Uint64N}
}

// Next returns the next retry delay in [exponential/2, exponential], capped at
// the configured maximum.
func (retry *Retry) Next() time.Duration {
	ceiling := retry.base
	for i := uint(0); i < retry.failures && ceiling < retry.maximum; i++ {
		if ceiling > retry.maximum/2 {
			ceiling = retry.maximum
			break
		}
		ceiling *= 2
	}
	if retry.failures < 63 {
		retry.failures++
	}
	floor := ceiling / 2
	span := ceiling - floor
	if span <= 0 {
		return ceiling
	}
	return floor + time.Duration(retry.random(uint64(span)+1))
}

func (retry *Retry) Reset() {
	retry.failures = 0
}

// Wait blocks until the delay expires, a wake is requested, or the service is
// canceled. A nil wake channel disables explicit wakeups.
func Wait(ctx context.Context, wake <-chan struct{}, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wake:
		return nil
	case <-timer.C:
		return nil
	}
}
