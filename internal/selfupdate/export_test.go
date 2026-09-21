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

// Token is the credential lookup, so a test can hold it to asking gh for github.com by
// name. The anonymous fallback rescues a wrong-host token, so nothing else notices.
var Token = token

// ExecutableMagicFor reports whether this platform has a signature update checks for, so
// a test can skip rather than assert nothing on a platform where the check is a no-op.
func ExecutableMagicFor(goos string) ([][]byte, bool) {
	m, ok := executableMagic[goos]
	return m, ok
}

// New, Replace and PlatformAsset are reached only from the test binary. They are hooks
// rather than exported API because replace renames arbitrary bytes over a path with no
// gate of its own: the magic-byte check that makes that safe lives in update, one call
// site above it, so keeping the whole package behind Run is what makes the
// check-then-replace ordering unbypassable by construction rather than by convention.
var (
	New           = newClient
	Replace       = replace
	PlatformAsset = platformAssetForHost
)

// Method expressions, so the test binary can drive the two steps of an update separately
// without client or release being part of the package's API. A caller outside the package
// cannot name either type, but type inference lets a test hold values of both.
var (
	Resolve        = (*client).resolve
	DownloadBinary = (*client).downloadBinary
)
