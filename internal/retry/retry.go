// Package retry provides the backoff policy for store requests: retry what a later
// attempt might fix, stop immediately on what it cannot.
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Policy configures Do. A zero Sleep backs off on a timer that a cancelled context cuts
// short; tests replace it to avoid waiting at all.
type Policy struct {
	Attempts int
	Base     time.Duration
	Sleep    func(time.Duration)
}

// DefaultPolicy is used for store API calls. Downloads pass their own, because
// re-transferring a multi-gigabyte body is not the same kind of cheap as re-issuing a
// 2 KB query.
func DefaultPolicy() Policy { return Policy{Attempts: 4, Base: 500 * time.Millisecond} }

// permanent marks an error that no further attempt can fix.
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent wraps err so Do returns it without retrying. The caller uses this when the
// *body* of a response settles the question that its status code left open — an expired
// session arrives as a 500, which the status alone would say to retry.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// Retryable reports whether an HTTP status is worth another attempt: rate limiting,
// request timeout, and server errors. Every other 4xx is the caller's fault and will
// not improve.
func Retryable(status int) bool {
	switch {
	case status == 429, status == 408:
		return true
	case status >= 500:
		return true
	default:
		return false
	}
}

// Do calls fn until it returns nil, returns a Permanent error, or the attempts run out,
// backing off base<<(n-1) between tries. A cancelled context stops it: before an attempt,
// and during the backoff itself.
func Do(ctx context.Context, p Policy, fn func(attempt int) error) error {
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	sleep := backoff
	if p.Sleep != nil {
		sleep = func(_ context.Context, d time.Duration) error { p.Sleep(d); return nil }
	}
	var last error
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = fn(attempt)
		if last == nil {
			return nil
		}
		var perm permanent
		if errors.As(last, &perm) {
			return perm.err
		}
		if attempt == p.Attempts {
			break
		}
		if err := sleep(ctx, p.Base<<(attempt-1)); err != nil {
			return err
		}
	}
	return fmt.Errorf("gave up after %d attempts: %w", p.Attempts, last)
}

// backoff waits, or gives up the wait when the context ends. Waiting it out regardless
// would make a cancelled run pay one full backoff per goroutine still in flight before it
// could exit, and downloads back off in seconds.
func backoff(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
