package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run is the layer above fixtures.Scrub, and it is the layer that deletes. `go run
// ./cmd/scrubfixtures` needs raw captures that are git-excluded and cannot be regenerated
// without a signed-in session, so it never executes in CI and scrub_test.go covers only
// the projection inside it. What is untested here is the stale sweep, which globs
// <to>/*.json and removes every file the current capture set did not produce.
//
// run takes from and to precisely so this can be driven against two temp directories.

const capture = `[{"data":{"searchMyAssets":{"total":1,"results":[
  {"id":"20066949412942","product":{"id":"115488","name":"Quick Outline"}}
]}}}]`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestRunScrubsEveryCaptureAndDropsNothingElse(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	write(t, from, "page0.json", capture)
	write(t, from, "page1.json", capture)

	if err := run(from, to); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := names(t, to); len(got) != 2 {
		t.Fatalf("wrote %v, want both pages", got)
	}
	for _, n := range names(t, to) {
		raw, err := os.ReadFile(filepath.Join(to, n))
		if err != nil {
			t.Fatal(err)
		}
		// The whole reason this command exists: the entitlement id must not survive.
		if strings.Contains(string(raw), "20066949412942") {
			t.Errorf("%s carries the entitlement id", n)
		}
	}
	// The captures are the input and must be left alone; a sweep that pointed at the wrong
	// directory would take the only copy of data that cannot be regenerated.
	if got := names(t, from); len(got) != 2 {
		t.Errorf("the capture directory now holds %v", got)
	}
}

// A page the new capture does not have is a page the old one did, and left in place it is
// served as that page by the next test run — the failure the sweep exists to prevent.
func TestRunRemovesAFixtureTheCapturesNoLongerHave(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	write(t, from, "page0.json", capture)
	write(t, to, "page0.json", `{"stale":true}`)
	write(t, to, "page1.json", `{"stale":true}`)
	// Not a fixture, so not the sweep's business: it globs *.json for exactly this reason.
	write(t, to, "README.md", "how these are generated")

	if err := run(from, to); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := names(t, to)
	if len(got) != 2 {
		t.Fatalf("directory holds %v, want page0.json and README.md", got)
	}
	for _, n := range got {
		if n == "page1.json" {
			t.Error("the stale page survived and will be served as that page by the next run")
		}
	}
	raw, err := os.ReadFile(filepath.Join(to, "page0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "stale") {
		t.Error("page0.json was swept rather than overwritten")
	}
}

// The sweep runs after every write, so a capture that will not scrub stops the command
// before anything is removed. Ordered the other way, a malformed page 2 would leave the
// directory holding page 1's new fixture and nothing else, with no way back short of
// capturing again.
//
// What this ordering buys is that nothing is *destroyed*, not that the set stays coherent:
// the writes are page by page, so a failure on page 2 still leaves a new page 1 beside the
// old page 9. That set is recoverable with git checkout, which is the whole difference —
// the sweep-first order is not.
func TestAFailedScrubLeavesTheExistingFixturesAlone(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	write(t, from, "page0.json", capture)
	write(t, from, "page1.json", `{"not":"a capture batch"}`)
	write(t, to, "page0.json", `{"existing":"fixture"}`)
	write(t, to, "page9.json", `{"existing":"fixture"}`)

	err := run(from, to)
	if err == nil {
		t.Fatal("run accepted a capture that is not a SearchMyAssets batch")
	}
	if !strings.Contains(err.Error(), "page1.json") {
		t.Errorf("the error does not name the capture that failed: %v", err)
	}
	// page9 is the one that matters: it is what the sweep would have taken.
	if _, statErr := os.Stat(filepath.Join(to, "page9.json")); statErr != nil {
		t.Errorf("a fixture was swept despite the run failing: %v", statErr)
	}
}

// Without this the command is a one-keystroke way to empty a directory: -to is a flag, and
// the default is the real fixtures.
func TestRunRefusesWhenThereAreNoCaptures(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	write(t, to, "page0.json", `{"existing":"fixture"}`)

	err := run(from, to)
	if err == nil {
		t.Fatal("run accepted an empty capture directory")
	}
	if _, statErr := os.Stat(filepath.Join(to, "page0.json")); statErr != nil {
		t.Errorf("the existing fixtures were swept for an empty capture set: %v", statErr)
	}
}
