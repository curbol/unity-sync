package releaseyml

import (
	"os"
	"path/filepath"
	"testing"
)

func workflow(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", ".github", "workflows", "release.yml")
}

// The parser is read against the committed workflow rather than a fixture, because a
// fixture is a copy of the list the installer and the updater exist to agree with. A
// release.yml whose shape moved would parse a fixture perfectly and hand both guards a
// list that no longer describes what gets published.
func TestPlatformsReadsTheRealWorkflow(t *testing.T) {
	got, err := Platforms(workflow(t))
	if err != nil {
		t.Fatalf("Platforms: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range got {
		if p.GOOS == "" || p.GOARCH == "" || p.Label == "" {
			t.Errorf("incomplete entry %+v", p)
		}
		seen[p.Label] = true
	}
	// The labels the installer and the updater both resolve to. Naming them here would
	// be a second source of truth if this were the only check, but it is not: both
	// guards read the same file, and this one only asserts the parser reaches them.
	for _, label := range []string{"mac-intel", "mac-apple", "linux-intel", "linux-arm64", "win"} {
		if !seen[label] {
			t.Errorf("the workflow's platform list no longer yields %q", label)
		}
	}
}

// The shape guards are the whole value of the parser: a workflow it cannot read has to
// fail its callers rather than hand them an empty list, which every guard built on it
// would pass vacuously.
func TestAnUnreadableShapeIsAnErrorNotAnEmptyList(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no platforms array", "jobs:\n  release:\n    steps: []\n"},
		{"unterminated array", "platforms=(\n  \"linux/amd64/linux-intel\"\n"},
		{"array with no entries", "platforms=(\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "release.yml")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := Platforms(p)
			if err == nil {
				t.Fatalf("parsed %d platforms out of an unreadable shape, want an error: %+v", len(got), got)
			}
		})
	}
}

// The same reason as the shape guards, one step earlier: a workflow that is not there
// at all — moved, renamed, or read from the wrong relative path — must fail rather than
// parse as no platforms, which every guard built on this would pass vacuously.
func TestAMissingWorkflowIsAnError(t *testing.T) {
	if _, err := Platforms(filepath.Join(t.TempDir(), "absent.yml")); err == nil {
		t.Fatal("a missing workflow parsed without error")
	}
}

// An entry the regex cannot read has to fail the parse, not vanish from it. Erroring
// only at zero leaves a platform that can be added to the workflow, published as an
// asset, and quietly dropped by every guard built on this at once.
func TestAnEntryThatDoesNotParseIsAnError(t *testing.T) {
	for _, body := range []string{
		"platforms=(\n  \"linux/amd64/linux-intel\"\n  \"linux/riscv64/linux_riscv\"\n)\n",
		"platforms=(\n  \"linux/amd64/linux-intel\"\n  \"windows/arm64/Win-ARM\"\n)\n",
	} {
		p := filepath.Join(t.TempDir(), "release.yml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := Platforms(p)
		if err == nil {
			t.Errorf("parsed %+v out of a list holding an entry this cannot read", got)
		}
	}
}
