package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/syncer"
)

// Failure models the CLI must keep pinned: ways a command could do something other than
// what the user typed.

// flag.Parse stops at the first positional, so an unchecked one swallows the flags after
// it: `sync foo --dry-run` would download.
func TestStrayPositionalIsRejectedAndSuggestsOnly(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"sync", "status", "list", "select"} {
		code, err := run([]string{cmd, "some-asset", "--dry-run"})
		if code == 0 || err == nil {
			t.Errorf("%s with a positional = %d, %v; want a failure", cmd, code, err)
			continue
		}
		if !strings.Contains(err.Error(), "--only") {
			t.Errorf("%s: error %q does not point at --only", cmd, err)
		}
	}
}

// These return before flag parsing, so their positionals are checked in their own branch
// or not at all. `unity-sync version foo` succeeding quietly is how a typo turns into a
// command that did nothing and said nothing.
func TestTheCommandsThatTakeNoArgumentsRejectOne(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"version", "-v", "--version", "help", "-h", "--help"} {
		code, err := run([]string{cmd, "stray"})
		if code == 0 || err == nil {
			t.Errorf("%s with a positional = %d, %v; want a failure", cmd, code, err)
		}
	}
}

func TestSessionWithoutTheCredentialIsNamedBeforeAnyRequest(t *testing.T) {
	wd := isolate(t)
	if err := os.WriteFile(filepath.Join(wd, manifest.FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(t.TempDir(), "session.curl")
	body := "curl 'https://assetstore.unity.com/' -H 'Cookie: DS=abc; _csrf=zzz'"
	if err := os.WriteFile(sessionPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := run([]string{"status", "--session", sessionPath})
	if code == 0 || err == nil {
		t.Fatalf("status = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "LS") {
		t.Errorf("error %q does not name the missing credential cookie", err)
	}
}

// Without a session every networked command must fail with advice, not a stack trace or
// an opaque HTTP error.
func TestMissingSessionIsExplained(t *testing.T) {
	wd := isolate(t)
	if err := os.WriteFile(filepath.Join(wd, manifest.FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := run([]string{"status"})
	if code == 0 || err == nil {
		t.Fatalf("status with no session = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("error %q does not mention the session", err)
	}
}

// The class tally counts a delisted asset but cannot say which one, and which one is the
// only part the user can act on.
func TestTheSummaryNamesEachDelistedAsset(t *testing.T) {
	buf := &bytes.Buffer{}
	printReport(buf, syncer.Report{
		Owned: 2,
		Results: []syncer.Result{
			{Asset: model.Asset{ID: "1", Name: "Still Fine", State: model.StatePublished}, Class: syncer.Unchanged},
			{Asset: model.Asset{ID: "193760", Name: "Fantasy Sounds Bundle", State: model.StateDisabled}, Class: syncer.Undownloadable},
		},
	}, false, "/lib")
	out := buf.String()
	for _, want := range []string{"Fantasy Sounds Bundle", "193760", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not name %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Still Fine") {
		t.Errorf("the summary named an asset that is not delisted:\n%s", out)
	}
}
