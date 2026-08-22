package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
)

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := stdout
	stdout = buf
	t.Cleanup(func() { stdout = old })
	return buf
}

// isolate keeps a developer's real config, session and library out of the tests.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	for _, k := range []string{"UNITY_SYNC_CONFIG_DIR", "UNITY_SYNC_LIBRARY", "UNITY_SYNC_SESSION"} {
		os.Unsetenv(k)
	}
	wd := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
	return wd
}

func TestVersionAndHelpSucceed(t *testing.T) {
	isolate(t)
	out := capture(t)
	code, err := run([]string{"version"})
	if code != 0 || err != nil {
		t.Fatalf("version = %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "unity-sync") {
		t.Errorf("version printed %q", out)
	}
	if code, err := run([]string{"help"}); code != 0 || err != nil {
		t.Errorf("help = %d, %v", code, err)
	}
}

func TestUnknownSubcommandFails(t *testing.T) {
	isolate(t)
	code, err := run([]string{"frobnicate"})
	if code == 0 || err == nil {
		t.Fatalf("unknown subcommand = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error %q does not name the subcommand", err)
	}
}

// update is the one subcommand that takes a positional.
func TestUpdateAcceptsOneVersionAndRejectsTwo(t *testing.T) {
	isolate(t)
	code, err := run([]string{"update", "1.2.3", "4.5.6"})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("update with two versions = %d, %v", code, err)
	}
	// One argument gets past dispatch and fails later, on the dev-build guard.
	_, err = run([]string{"update", "1.2.3"})
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Errorf("update on a dev build = %v, want the dev-build guard", err)
	}
}

func TestCommandsThatNeedAManifestSayHowToMakeOne(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"status", "sync"} {
		code, err := run([]string{cmd})
		if code == 0 || err == nil {
			t.Fatalf("%s with no manifest = %d, %v; want a failure", cmd, code, err)
		}
		if !strings.Contains(err.Error(), "select") {
			t.Errorf("%s: error %q does not tell the user how to create one", cmd, err)
		}
	}
}

// list reads only the lockfile, so it must work with no session and no network.
func TestListNeedsNoSession(t *testing.T) {
	wd := isolate(t)
	out := capture(t)

	manifestPath := filepath.Join(wd, manifest.FileName)
	if err := os.WriteFile(manifestPath, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Assets["quick-outline-115488"] = lockfile.Entry{
		AssetID: "115488", Name: "Quick Outline", Tracked: true,
		Version: lockfile.Version{ID: "683375", Name: "1.1"},
	}
	lf.Assets["owned-only-999"] = lockfile.Entry{
		AssetID: "999", Name: "Owned But Not Mirrored",
		Version: lockfile.Version{ID: "1", Name: "1.0"},
	}
	if err := lockfile.Save(manifest.LockPath(manifestPath), lf); err != nil {
		t.Fatal(err)
	}

	code, err := run([]string{"list"})
	if code != 0 || err != nil {
		t.Fatalf("list = %d, %v", code, err)
	}
	body := out.String()
	for _, want := range []string{"Quick Outline", "Owned But Not Mirrored", "2 owned, 1 mirrored"} {
		if !strings.Contains(body, want) {
			t.Errorf("list output is missing %q:\n%s", want, body)
		}
	}
}
