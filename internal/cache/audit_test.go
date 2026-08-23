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
	n, bytesFreed, err := cache.SweepTemps(root, cutoff)
	if err != nil {
		t.Fatalf("SweepTemps: %v", err)
	}
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
	n, bytes, err := cache.SweepTemps(filepath.Join(t.TempDir(), "never-created"), time.Now())
	if err != nil {
		t.Errorf("SweepTemps on a missing root = %v, want nil", err)
	}
	if n != 0 || bytes != 0 {
		t.Errorf("swept %d files / %d bytes from a missing root", n, bytes)
	}
}
