package selfupdate

import "testing"

// Test hooks. Nothing here ships: export_test.go is compiled only into the test binary.

// PlatformAssetFor is platformAsset with the platform supplied, so a test can check every
// target .github/workflows/release.yml builds rather than only the one it runs on.
var PlatformAssetFor = platformAsset

// ReplaceAside is the Windows recovery path: the one branch where a mistake leaves
// nothing on PATH, on the one OS the Linux CI job never executes.
var ReplaceAside = replaceAside

// Update is Run with the client and target supplied, so an update can be driven end to
// end against a test server without replacing the test binary.
var Update = update

// ForceImageLocked makes Replace take that branch for the duration of a test, so it is
// reachable on a machine that would otherwise rename over the running image happily.
func ForceImageLocked(t *testing.T) {
	prev := runningImageIsLocked
	runningImageIsLocked = func() bool { return true }
	t.Cleanup(func() { runningImageIsLocked = prev })
}

// ExecutableMagicFor reports whether this platform has a signature update checks for, so
// a test can skip rather than assert nothing on a platform where the check is a no-op.
func ExecutableMagicFor(goos string) ([][]byte, bool) {
	m, ok := executableMagic[goos]
	return m, ok
}
