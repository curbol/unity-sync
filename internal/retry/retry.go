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
	// Marking an already-marked error again would leave Do returning the inner wrapper
	// rather than the sentinel underneath it, so a caller comparing with == or reading
	// Error() on the concrete type sees an unexported wrapper it cannot name. Layers
	// both classify: the store marks what a status settles, and the syncer marks what the
	// body settles, so the same error reaches Permanent twice by design.
	if _, already := err.(permanent); already {
		return err
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
	// Floored here rather than left to each caller, because Attempts already is and a
	// half-normalised zero value is the trap. Policy is exported and so is
	// syncer.Options.Retry, which guards only Attempts — so retry.Policy{Attempts: 3},
	// the obvious way to write "try three times", would back off for zero and issue three
	// download requests back to back, per asset, across the whole pool.
	if p.Base <= 0 {
		p.Base = DefaultPolicy().Base
	}
	// Sleep is a test seam on an exported field, and it is not context-aware: an injected
	// sleeper cannot be cut short, so a caller that sets one in production silently loses
	// the property backoff exists to provide — a Ctrl-C during a 30-second wait is waited
	// out instead of honoured. Nothing in the tree sets it outside a test, and nothing
	// should.
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
			// Unwrapped on the way out, so a caller sees the cause rather than this
			// package's marker type. That also means permanence does not survive a
			// nested Do: an error marked inside an inner schedule comes back bare, and an
			// outer Do around it retries what the inner one had already settled. Live
			// today only through store.Lookup, which runs its own Do and is called from
			// inside the download schedule by the syncer's republish check — which
			// discards the error, so nothing currently depends on the marker crossing.
			// A caller that starts propagating one has to re-mark it.
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
