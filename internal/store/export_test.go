package store

import (
	"net/http"
	"time"

	"github.com/curbol/unity-sync/internal/retry"
)

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

// The options that only ever shorten a wait. Every one of them exists so a test need not
// sit through the real value — a two-minute silence budget, a sixty-second header
// deadline, a backoff measured in seconds — and none has a caller outside this test
// binary, so keeping them here rather than on the package surface means the shipped API
// claims only what production uses.
var (
	WithStallTimeout   = func(d time.Duration) Option { return func(c *Client) { c.stallTimeout = d } }
	WithRequestTimeout = func(d time.Duration) Option { return func(c *Client) { c.requestTimeout = d } }

	// Does not govern downloads: Fetch makes one attempt and the syncer owns the retry,
	// because a retried download has to reopen the cache's temp file and hasher with it
	// rather than resume into a partial one.
	WithRetryPolicy = func(p retry.Policy) Option { return func(c *Client) { c.retries = p } }

	// Shortens the header deadline, so a test can prove the difference between bounding
	// the headers and bounding the whole transfer.
	WithResponseHeaderTimeout = func(d time.Duration) Option {
		return func(c *Client) { c.http.Transport.(*http.Transport).ResponseHeaderTimeout = d }
	}
)

// ClientTimeout reads the shared http.Client's whole-request timeout, which must stay
// zero. A download legitimately runs for hours, and no test server can demonstrate the
// absence of a bound that long — so this one is checked structurally instead.
func ClientTimeout(c *Client) time.Duration { return c.http.Timeout }

// ResponseHeaderTimeout reads the transport's header deadline, which is the only bound on
// a download before the first byte arrives: Fetch builds its request context without one,
// Client.Timeout is deliberately zero, and the stall guard is not installed until after
// the response headers are back. Checked structurally for the same reason ClientTimeout
// is — no test server can wait out the real value.
func ResponseHeaderTimeout(c *Client) time.Duration {
	return c.http.Transport.(*http.Transport).ResponseHeaderTimeout
}
