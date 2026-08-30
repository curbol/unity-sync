package retry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/retry"
)

// stubClock records what Do would have slept instead of sleeping.
type stubClock struct{ slept []time.Duration }

func (s *stubClock) sleep(d time.Duration) { s.slept = append(s.slept, d) }

func policy(attempts int, c *stubClock) retry.Policy {
	return retry.Policy{Attempts: attempts, Base: 100 * time.Millisecond, Sleep: c.sleep}
}

func TestBackoffDoublesAndStopsAtTheAttemptLimit(t *testing.T) {
	clock := &stubClock{}
	calls := 0
	err := retry.Do(context.Background(), policy(4, clock), func(int) error {
		calls++
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("Do returned nil after exhausting attempts")
	}
	if calls != 4 {
		t.Errorf("fn called %d times, want 4", calls)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(clock.slept) != len(want) {
		t.Fatalf("slept %v, want %v (no sleep after the final attempt)", clock.slept, want)
	}
	for i := range want {
		if clock.slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, clock.slept[i], want[i])
		}
	}
}

func TestSuccessOnASecondAttemptSleepsOnce(t *testing.T) {
	clock := &stubClock{}
	err := retry.Do(context.Background(), policy(4, clock), func(attempt int) error {
		if attempt == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do = %v, want nil", err)
	}
	if len(clock.slept) != 1 {
		t.Errorf("slept %v, want exactly one backoff", clock.slept)
	}
}

// The expired-session case: the status says 500, which Retryable would retry, but the
// body settles it. Permanent must short-circuit so the user is told immediately instead
// of after the full schedule.
func TestPermanentStopsImmediatelyAndUnwraps(t *testing.T) {
	clock := &stubClock{}
	sentinel := errors.New("session expired")
	calls := 0
	err := retry.Do(context.Background(), policy(4, clock), func(int) error {
		calls++
		return retry.Permanent(sentinel)
	})
	if calls != 1 {
		t.Errorf("fn called %d times, want 1", calls)
	}
	if len(clock.slept) != 0 {
		t.Errorf("slept %v, want none", clock.slept)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Do = %v, want the wrapped sentinel to survive errors.Is", err)
	}
}

func TestCancelledContextStopsBeforeCalling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retry.Do(ctx, policy(4, &stubClock{}), func(int) error {
		calls++
		return errors.New("should not run")
	})
	if calls != 0 {
		t.Errorf("fn called %d times on a cancelled context, want 0", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v, want context.Canceled", err)
	}
}

func TestRetryableCoversRateLimitsAndServerErrorsOnly(t *testing.T) {
	for status, want := range map[int]bool{
		200: false, 302: false, 400: false, 401: false, 403: false, 404: false,
		408: true, 429: true, 500: true, 502: true, 503: true,
	} {
		if got := retry.Retryable(status); got != want {
			t.Errorf("Retryable(%d) = %v, want %v", status, got, want)
		}
	}
}

// A run cancelled mid-backoff must not wait the backoff out first. Downloads back off in
// seconds and the pool holds one of these per goroutine still in flight, so waiting would
// be the delay a user sees between Ctrl-C and the process exiting.
func TestCancellationCutsTheBackoffShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	start := time.Now()
	err := retry.Do(ctx, retry.Policy{Attempts: 3, Base: 30 * time.Second}, func(int) error {
		attempts++
		cancel() // the user hits Ctrl-C while this attempt is running
		return errors.New("boom")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("ran %d attempts after cancellation, want 1", attempts)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Do waited %v before noticing the cancellation", elapsed)
	}
}
