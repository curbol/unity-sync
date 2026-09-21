package lockfile

import "os"

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

// StubSync replaces the flush Save performs before its rename and returns a function that
// puts the real one back. The flush has no observable effect on a machine that does not
// lose power, so this is the only way to hold Save to it.
func StubSync(f func(*os.File) error) func() {
	prev := syncFile
	syncFile = f
	return func() { syncFile = prev }
}
