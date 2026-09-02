package store

import "time"

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

// ClientTimeout reads the shared http.Client's whole-request timeout, which must stay
// zero. A download legitimately runs for hours, and no test server can demonstrate the
// absence of a bound that long — so this one is checked structurally instead.
func ClientTimeout(c *Client) time.Duration { return c.http.Timeout }
