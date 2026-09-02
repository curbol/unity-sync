package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// install.sh is the half a user pipes into a shell, and it is the only part of this repo
// no Go code covers: it derives the release asset's name from uname in its own language,
// and it overwrites the binary on PATH. Both are pinned here.

// requireShell skips on the platform install.sh is not for. CI runs this suite on Windows
// too, where the release zip is used directly and the script never runs.
func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is a POSIX shell script")
	}
}

// installerZip builds a release archive holding one file named unity-sync with the given
// bytes, so a test can ship either a real-looking binary or something that is not one.
func installerZip(t *testing.T, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("unity-sync")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// nativeMagic is the leading signature install.sh checks for on this platform.
func nativeMagic(t *testing.T) []byte {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return []byte("\x7fELF")
	case "darwin":
		return []byte{0xcf, 0xfa, 0xed, 0xfe}
	}
	t.Skipf("install.sh supports linux and darwin, not %s", runtime.GOOS)
	return nil
}

// platformLabel is what this host's release asset is called. Held to release.yml by
// TestInstallerPlatformLabelsMatchTheRelease, so the stub below cannot drift from it.
func platformLabel(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux-intel"
	case "linux/arm64":
		return "linux-arm64"
	case "darwin/amd64":
		return "mac-intel"
	case "darwin/arm64":
		return "mac-apple"
	}
	t.Skipf("no release label for %s/%s", runtime.GOOS, runtime.GOARCH)
	return ""
}

// stubRelease serves the release API and the asset download, so the script runs end to
// end with no network.
func stubRelease(t *testing.T, asset []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/curbol/unity-sync/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name": "v9.9.9"}`)
	})
	mux.HandleFunc("/curbol/unity-sync/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		want := "unity-sync-9.9.9-" + platformLabel(t) + ".zip"
		if filepath.Base(r.URL.Path) != want {
			http.Error(w, "no such asset", http.StatusNotFound)
			return
		}
		w.Write(asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runInstaller runs install.sh with a throwaway HOME and no credential in the
// environment, so the gh fallback on a developer machine cannot decide what it finds.
func runInstaller(t *testing.T, home string, env ...string) (string, error) {
	t.Helper()
	requireShell(t)
	if _, err := exec.LookPath("unzip"); err != nil {
		t.Skip("install.sh needs unzip")
	}
	cmd := exec.Command("bash", "install.sh")
	// The installer falls back to the gh CLI, which on a developer machine is signed in;
	// clear the whole credential path so the test decides what it finds.
	cmd.Env = append([]string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"GITHUB_TOKEN=", "GH_TOKEN=",
	}, env...)
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

// curlStub puts a curl on PATH that records its arguments and its stdin separately, then
// fails, so the installer stops at its first fetch.
const curlStub = `#!/bin/sh
echo "$@" >> ARGV_PATH
cat >> STDIN_PATH
exit 22
`

// A credential on a command line is readable out of `ps` by every other local user, so
// the installer writes it into a curl config on stdin instead. Nothing about that shows
// up in the script's own output — curl -s prints nothing either way — so the only way to
// hold it is to be curl and look at which channel the token arrived on. Rewriting fetch()
// to pass -H "Authorization: token $token" leaves every other test in this file green.
func TestTheGitHubTokenNeverReachesACommandLine(t *testing.T) {
	requireShell(t)
	const sentinel = "ghp-sentinel-not-in-ps"

	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	stdinPath := filepath.Join(dir, "stdin")
	stub := strings.NewReplacer("ARGV_PATH", argvPath, "STDIN_PATH", stdinPath).Replace(curlStub)
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "install.sh")
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + dir + ":/usr/bin:/bin",
		"GITHUB_TOKEN=" + sentinel,
	}
	cmd.CombinedOutput() // fails at the first fetch by design: the stub exits 22

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("the installer never invoked curl, so nothing was observed: %v", err)
	}
	if strings.Contains(string(argv), sentinel) {
		t.Errorf("the token reached curl's argv, where any local user can read it out of ps:\n%s", argv)
	}
	body, err := os.ReadFile(stdinPath)
	if err != nil || !strings.Contains(string(body), sentinel) {
		t.Errorf("the token did not reach curl over stdin, so this test is not watching the "+
			"channel it claims: stdin=%q err=%v", body, err)
	}
}

// The ordinary no-credential case must say what went wrong. Every version lookup here
// assigns from a pipeline, and under `set -euo pipefail` an unguarded one kills the
// script before the message written for this case can print.
func TestInstallerReportsAnUnreachableRelease(t *testing.T) {
	out, err := runInstaller(t, t.TempDir(),
		"UNITY_SYNC_INSTALL_API=http://127.0.0.1:1",
		"UNITY_SYNC_INSTALL_DOWNLOAD=http://127.0.0.1:1")
	if err == nil {
		t.Fatalf("the installer succeeded against an unreachable release:\n%s", out)
	}
	if !strings.Contains(out, "could not resolve the latest version") {
		t.Errorf("the installer failed with no explanation:\n%s", out)
	}
}

// A release that shipped something which is not a binary must not be chmod 0755'd over a
// working install. The smoke test at the end of the script notices a broken install, but
// only once the working one has already been overwritten with no way back.
func TestInstallerRefusesANonExecutableAsset(t *testing.T) {
	home := t.TempDir()
	installed := filepath.Join(home, ".local", "bin", "unity-sync")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	working := append(nativeMagic(t), []byte("the working one")...)
	if err := os.WriteFile(installed, working, 0o755); err != nil {
		t.Fatal(err)
	}

	srv := stubRelease(t, installerZip(t, []byte("<!doctype html><title>Not found</title>")))
	out, err := runInstaller(t, home,
		"UNITY_SYNC_INSTALL_API="+srv.URL, "UNITY_SYNC_INSTALL_DOWNLOAD="+srv.URL)
	if err == nil {
		t.Fatalf("a non-executable asset installed successfully:\n%s", out)
	}
	if !strings.Contains(out, "not a") || !strings.Contains(out, "executable") {
		t.Errorf("the asset was refused for some reason other than not being a binary:\n%s", out)
	}
	got, readErr := os.ReadFile(installed)
	if readErr != nil {
		t.Fatalf("the working binary is gone: %v", readErr)
	}
	if !bytes.Equal(got, working) {
		t.Errorf("the working binary was replaced with the refused asset:\n%s", out)
	}
}

// The staged file lives inside the install directory so the last step is a
// same-filesystem rename; across filesystems mv degrades to copy-then-unlink, where an
// interrupted upgrade leaves a truncated binary at the live path. It must not survive.
func TestInstallerInstallsAndLeavesNoStagingBehind(t *testing.T) {
	want := append(nativeMagic(t), []byte("a real enough binary")...)
	home := t.TempDir()
	srv := stubRelease(t, installerZip(t, want))

	// The script ends by running what it installed, which here is not a real binary, so a
	// non-zero exit is expected. The install itself still has to be right.
	out, _ := runInstaller(t, home,
		"UNITY_SYNC_INSTALL_API="+srv.URL, "UNITY_SYNC_INSTALL_DOWNLOAD="+srv.URL)

	binDir := filepath.Join(home, ".local", "bin")
	got, err := os.ReadFile(filepath.Join(binDir, "unity-sync"))
	if err != nil {
		t.Fatalf("nothing was installed: %v\n%s", err, out)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("installed %d bytes, want the %d from the asset", len(got), len(want))
	}
	info, err := os.Stat(filepath.Join(binDir, "unity-sync"))
	if err != nil {
		t.Fatal(err)
	}
	// 0755 rather than +x: mktemp makes the file 0600, so adding the execute bits alone
	// would install 0711 and strip read access from group and other.
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("installed mode %v, want 0755", perm)
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unity-sync.") {
			t.Errorf("a staged file was left behind: %s", e.Name())
		}
	}
}

// releasePlatform is one entry of release.yml's platforms list: the pair it builds for
// and the label the published asset carries.
type releasePlatform struct{ goos, goarch, label string }

var platformEntryRe = regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)

// releasePlatforms reads what the release workflow actually builds, which is the only
// place the labels are really decided.
func releasePlatforms(t *testing.T) []releasePlatform {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	_, rest, ok := strings.Cut(string(raw), "platforms=(")
	if !ok {
		t.Fatal("release.yml has no platforms=( ... ) list; this guard no longer reads what the workflow builds")
	}
	block, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatal("release.yml's platforms list is unterminated")
	}
	var out []releasePlatform
	for _, m := range platformEntryRe.FindAllStringSubmatch(block, -1) {
		out = append(out, releasePlatform{goos: m[1], goarch: m[2], label: m[3]})
	}
	if len(out) == 0 {
		t.Fatal("no platforms parsed from release.yml")
	}
	return out
}

// unameStub puts a uname on PATH that answers for the given platform, so the installer's
// real detect_platform can be run for a machine this one is not.
func unameStub(t *testing.T, sysname, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  -s) echo %q ;;\n  -m) echo %q ;;\nesac\n", sysname, machine)
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The installer composes the asset label from uname in shell, the updater composes it in
// Go, and release.yml decides what is actually published. Nothing compiles the three
// together: TestPlatformAssetNamesMatchWhatTheReleaseWorkflowPublishes holds the Go half
// to the workflow, and this holds the shell half to it. Without this a label renamed in
// release.yml leaves every check green and breaks `curl | bash` on every platform.
func TestInstallerPlatformLabelsMatchTheRelease(t *testing.T) {
	nativeMagic(t)                   // install.sh runs on linux and darwin only
	unameFor := map[string][]string{ // goos/goarch -> the uname -s, -m spellings to try
		"darwin/amd64": {"Darwin", "x86_64"}, "darwin/arm64": {"Darwin", "arm64"},
		"linux/amd64": {"Linux", "x86_64"}, "linux/arm64": {"Linux", "aarch64"},
	}
	// The other spelling of each arch, so both arms of the installer's case are held to
	// the same label rather than only the one this machine happens to report.
	alias := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "aarch64"}

	covered := 0
	for _, p := range releasePlatforms(t) {
		if p.goos == "windows" {
			continue // install.sh refuses Windows by design; the release zip is used directly
		}
		un, ok := unameFor[p.goos+"/"+p.goarch]
		if !ok {
			t.Errorf("release.yml builds %s/%s but this guard does not know its uname output", p.goos, p.goarch)
			continue
		}
		covered++
		for _, machine := range []string{un[1], alias[un[1]]} {
			t.Run(p.label+"/"+machine, func(t *testing.T) {
				out := runInstallerAs(t, unameStub(t, un[0], machine))
				// The whole line, terminator included: a label that merely starts with
				// the expected one ("mac-applesilicon" for "mac-apple") names an asset no
				// release publishes and must not read as a match.
				want := "INFO: platform: " + p.label + "\n"
				if !strings.Contains(out, want) {
					t.Errorf("install.sh reported no %q for uname -s %q -m %q; release.yml publishes %s.zip\n%s",
						want, un[0], machine, p.label, out)
				}
			})
		}
	}
	if covered == 0 {
		t.Fatal("no installable platform found in release.yml")
	}
	// The label this file's own stub serves has to be the same one, or the end-to-end
	// tests above would be passing against an asset no release publishes.
	for _, p := range releasePlatforms(t) {
		if p.goos == runtime.GOOS && p.goarch == runtime.GOARCH && platformLabel(t) != p.label {
			t.Errorf("platformLabel = %q, want the %q release.yml publishes for this host",
				platformLabel(t), p.label)
		}
	}
}

// runInstallerAs runs install.sh with pathPrefix ahead of the system directories and an
// unreachable release, so it gets as far as announcing the platform and no further. The
// non-zero exit is the expected outcome, not a failure.
func runInstallerAs(t *testing.T, pathPrefix string) string {
	t.Helper()
	requireShell(t)
	cmd := exec.Command("bash", "install.sh")
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + pathPrefix + ":/usr/bin:/bin",
		"GITHUB_TOKEN=", "GH_TOKEN=",
		"UNITY_SYNC_INSTALL_API=http://127.0.0.1:1",
		"UNITY_SYNC_INSTALL_DOWNLOAD=http://127.0.0.1:1",
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}
