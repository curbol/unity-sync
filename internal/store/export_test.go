package store

import (
	"net/http"
	"time"
)

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

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
