package cache_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
)

// Failure models this package must keep pinned. Every case is a way the cache could record
// the wrong bytes as verified.

// The window between writing bytes and accepting them is where a rejected body would
// otherwise sit at a real cache path.
func TestStoreLeavesNothingAtTheRealPathUntilCommit(t *testing.T) {
	root := t.TempDir()
	body := pkg(t, "115488", "683375", 500)
	p, err := cache.Store(root, "chris-nolet", "quick-outline-115488", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	final := filepath.Join(root, filepath.FromSlash(p.RelPath))
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatal("Store put bytes at the real path before they were checked")
	}
	if p.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", p.Size, len(body))
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("Commit did not place the file: %v", err)
	}
}

// The caller records the digest of whatever lands at the destination, so an overwrite
// here would certify the wrong bytes — with nothing else in the design watching.
func TestRelocateRefusesAnOccupiedDestination(t *testing.T) {
	root := t.TempDir()
	from := cache.RelPath("pub", "stray-111")
	to := cache.RelPath("pub", "asset-111")
	storeCommitted(t, root, "pub", "stray-111", pkg(t, "111", "9", 400))
	storeCommitted(t, root, "pub", "asset-111", pkg(t, "111", "9", 900))

	if err := cache.Relocate(root, from, to); err == nil {
		t.Fatal("Relocate silently overwrote an occupied destination")
	}
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(to)))
	if err != nil || fi.Size() != 900 {
		t.Errorf("the destination file was disturbed: size %v, err %v", fi.Size(), err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(from))); err != nil {
		t.Error("the source file was lost to a refused move")
	}
}

func TestSweepWalksTheTreeAndSparesInFlightTemps(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "pub", "asset-1")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(leaf, ".unity-sync-dl-old")
	fresh := filepath.Join(leaf, ".unity-sync-dl-live")
	os.WriteFile(stale, bytes.Repeat([]byte("x"), 100), 0o644)
	os.WriteFile(fresh, bytes.Repeat([]byte("x"), 50), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stale, old, old)

	cutoff := time.Now().Add(-time.Hour)
	n, bytesFreed := cache.SweepTemps(root, cutoff)
	// A root-only scan would report zero here while leaving a multi-gigabyte orphan.
	if n != 1 || bytesFreed != 100 {
		t.Errorf("swept %d files / %d bytes, want 1 / 100", n, bytesFreed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("the sweep deleted a temp newer than the cutoff, i.e. another run's transfer")
	}
}

func TestUnsafePathsAreRefused(t *testing.T) {
	root := t.TempDir()
	for _, seg := range []string{"", ".", "..", "a/b", `a\b`, ".hidden", "with\x00null"} {
		if _, err := cache.Store(root, seg, "asset", bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Store accepted publisher slug %q", seg)
		}
		if _, err := cache.Store(root, "pub", seg, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Store accepted asset slug %q", seg)
		}
	}
	for _, rel := range []string{"", "/etc/passwd", "../escape.unitypackage", "a/../../escape"} {
		if cache.Verify(root, rel, 1, "") {
			t.Errorf("Verify accepted path %q", rel)
		}
		if _, _, err := cache.Hash(root, rel); err == nil {
			t.Errorf("Hash accepted path %q", rel)
		}
	}
}

// A library that does not exist yet is the first-run case, not an error: the sweep runs
// before anything has been written.
func TestSweepingAMissingRootIsNotAnError(t *testing.T) {
	n, bytes := cache.SweepTemps(filepath.Join(t.TempDir(), "never-created"), time.Now())
	if n != 0 || bytes != 0 {
		t.Errorf("swept %d files / %d bytes from a missing root", n, bytes)
	}
}

// The exclude list comes from the lockfile, which is hand-editable and travels between
// machines. Comparing it as a raw string would let "./pub/a/a.unitypackage" fail to skip
// the file "pub/a/a.unitypackage" names, re-offering a file that just failed verification
// as something to adopt.
func TestLocateSkipsAnExcludedFileWrittenNonCanonically(t *testing.T) {
	root := t.TempDir()
	rel := cache.RelPath("pub", "asset-1")
	storeCommitted(t, root, "pub", "asset-1", pkg(t, "111", "9", 400))

	if _, ok := cache.Locate(root, "111", "", rel); ok {
		t.Fatal("the canonical exclude did not skip the file")
	}
	for _, spelling := range []string{"./" + rel, "pub/./asset-1/asset-1.unitypackage"} {
		if _, ok := cache.Locate(root, "111", "", spelling); ok {
			t.Errorf("exclude %q did not skip the same file", spelling)
		}
	}
}

// The library root comes from a flag or a config file, so it arrives however the user
// typed it. Comparing it raw against paths that have been cleaned makes pruning a no-op
// for the ordinary "./lib" spelling, and nothing else notices.
func TestPruningSurvivesHoweverTheRootWasSpelled(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	spellings := map[string]func(string) string{
		"absolute":       func(base string) string { return base },
		"trailing slash": func(base string) string { return base + string(filepath.Separator) },
		"dot relative": func(base string) string {
			rel, err := filepath.Rel(wd, base)
			if err != nil {
				t.Skip("temp dir is not reachable relatively from the working directory")
			}
			return "." + string(filepath.Separator) + rel
		},
	}
	for name, spell := range spellings {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := spell(base)
			storeCommitted(t, root, "pub", "old-slug-1", pkg(t, "1", "9", 400))
			if err := cache.Relocate(root, cache.RelPath("pub", "old-slug-1"),
				cache.RelPath("pub", "new-slug-1")); err != nil {
				t.Fatalf("Relocate: %v", err)
			}
			if _, err := os.Stat(filepath.Join(base, "pub", "old-slug-1")); !os.IsNotExist(err) {
				t.Errorf("root spelled %q left the emptied directory behind", root)
			}
		})
	}
}

// An exclusion names a file that must not be adopted. If it cannot be resolved, the scan
// cannot tell which file that is, and offering a candidate anyway would let the excluded
// one back in through the door that skips the download guards.
func TestAnUnresolvableExclusionRefusesEveryCandidate(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "pub", "asset-1", pkg(t, "111", "9", 400))

	if _, ok := cache.Locate(root, "111", ""); !ok {
		t.Fatal("the candidate is not findable at all")
	}
	for _, bad := range []string{"/etc/passwd", "../outside/x.unitypackage"} {
		if _, ok := cache.Locate(root, "111", "", bad); ok {
			t.Errorf("exclusion %q was dropped and a candidate offered anyway", bad)
		}
	}
}
