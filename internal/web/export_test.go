package web

import (
	"os"
	"sync/atomic"
	"testing"
)

// Serve launches a browser at the address it binds, so left alone every test that drives
// it opens a real tab at a URL that dies with the test. Stubbed for the whole test binary
// rather than per test: a stub each test has to remember is one a test will eventually
// forget, and the cost of forgetting is a tab per call on whoever runs the suite.
func TestMain(m *testing.M) {
	OpenBrowser = func(string) { browserLaunches.Add(1) }
	os.Exit(m.Run())
}

var browserLaunches atomic.Int64

// BrowserLaunches is how many tabs Serve has asked to open. A test driving Serve asserts
// on the delta, which is what proves the stub sits on the path that would really launch
// rather than somewhere the code no longer reaches.
func BrowserLaunches() int64 { return browserLaunches.Load() }
