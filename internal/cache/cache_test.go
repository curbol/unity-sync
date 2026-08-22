package cache_test

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
)

// pkg builds a gzip stream carrying the store's descriptor, padded to size.
func pkg(t *testing.T, productID, versionID string, size int) []byte {
	t.Helper()
	descriptor := []byte(`{"id":"` + productID + `","version_id":"` + versionID + `"}`)
	extra := []byte{'A', '$', 0, 0}
	binary.LittleEndian.PutUint16(extra[2:4], uint16(len(descriptor)))
	extra = append(extra, descriptor...)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Header.Extra = extra
	zw.Write(bytes.Repeat([]byte("x"), 64))
	zw.Close()
	out := buf.Bytes()
	for len(out) < size {
		out = append(out, 0)
	}
	return out
}

func storeCommitted(t *testing.T, root, pub, asset string, body []byte) *cache.Pending {
	t.Helper()
	p, err := cache.Store(root, pub, asset, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return p
}

func TestLayoutIsThreeSegmentsSoQuarryGetsBothFacets(t *testing.T) {
	got := cache.RelPath("doublel", "quick-outline-115488")
	want := "doublel/quick-outline-115488/quick-outline-115488.unitypackage"
	if got != want {
		t.Errorf("RelPath = %q, want %q", got, want)
	}
	if n := strings.Count(got, "/"); n != 2 {
		t.Errorf("path has %d separators, want 2: quarry fills its pack facet only from a third segment", n+1)
	}
}

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

func TestDiscardRemovesTheTempAndLeavesNoFile(t *testing.T) {
	root := t.TempDir()
	p, err := cache.Store(root, "pub", "asset-1", bytes.NewReader(pkg(t, "1", "2", 300)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(p.TempPath()); !os.IsNotExist(err) {
		t.Error("Discard left the temp file behind")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p.RelPath))); !os.IsNotExist(err) {
		t.Error("Discard left a file at the real path")
	}
}

func TestVerifyUsesExactRecordedSizeAndMetadata(t *testing.T) {
	root := t.TempDir()
	body := pkg(t, "115488", "683375", 800)
	p := storeCommitted(t, root, "pub", "quick-outline-115488", body)

	if !cache.Verify(root, p.RelPath, p.Size, "683375") {
		t.Error("Verify rejected a file it just stored")
	}
	if cache.Verify(root, p.RelPath, p.Size+1, "683375") {
		t.Error("Verify accepted a size that does not match the record")
	}
	if cache.Verify(root, p.RelPath, p.Size, "999999") {
		t.Error("Verify accepted a file whose descriptor names another version")
	}

	// Truncation is what the exact-size rule exists for: the descriptor sits in the
	// leading bytes and survives it.
	full := filepath.Join(root, filepath.FromSlash(p.RelPath))
	if err := os.Truncate(full, p.Size-100); err != nil {
		t.Fatal(err)
	}
	if cache.Verify(root, p.RelPath, p.Size, "683375") {
		t.Error("Verify accepted a truncated file")
	}
}

// A package with no descriptor is verified on size alone. Demanding a metadata match
// would make it re-download on every run, forever.
func TestVerifyFallsBackToSizeWhenNoDeliveredIdWasRecorded(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("no descriptor here"))
	zw.Close()
	p := storeCommitted(t, root, "pub", "plain-1", buf.Bytes())

	if !cache.Verify(root, p.RelPath, p.Size, "") {
		t.Error("Verify rejected a descriptor-less package that matches its recorded size")
	}
	if cache.Verify(root, p.RelPath, p.Size+5, "") {
		t.Error("Verify accepted a size mismatch even with no delivered id")
	}
}

func TestOnlyDeepVerifySeesAMidFileFlip(t *testing.T) {
	root := t.TempDir()
	body := pkg(t, "1", "2", 900)
	p := storeCommitted(t, root, "pub", "asset-1", body)
	full := filepath.Join(root, filepath.FromSlash(p.RelPath))

	f, err := os.OpenFile(full, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{0xFF}, 700) // past the descriptor, size unchanged
	f.Close()

	if !cache.Verify(root, p.RelPath, p.Size, "2") {
		t.Error("cheap verify should not see a mid-file flip; that is what makes it cheap")
	}
	if cache.VerifyDeep(root, p.RelPath, p.SHA256) {
		t.Error("deep verify missed a mid-file flip")
	}
}

func TestLocateFindsAPackageByItsOwnIdAndIgnoresTemps(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "pub-a", "asset-1", pkg(t, "111", "9", 400))
	storeCommitted(t, root, "pub-b", "asset-2", pkg(t, "222", "9", 400))

	// An abandoned partial with an intact descriptor must never be adoptable.
	tempDir := filepath.Join(root, "pub-c", "asset-3")
	os.MkdirAll(tempDir, 0o755)
	os.WriteFile(filepath.Join(tempDir, ".unity-sync-dl-999"), pkg(t, "333", "9", 400), 0o644)

	got, ok := cache.Locate(root, "222", "")
	if !ok || !strings.Contains(got.RelPath, "asset-2") {
		t.Errorf("Locate(222) = %+v, %v", got, ok)
	}
	if _, ok := cache.Locate(root, "333", ""); ok {
		t.Error("Locate adopted an abandoned download temp")
	}
}

func TestLocatePrefersTheFileAlreadyAtTheDerivedPath(t *testing.T) {
	root := t.TempDir()
	derived := cache.RelPath("pub", "asset-1")
	storeCommitted(t, root, "pub", "asset-1", pkg(t, "111", "9", 400))
	storeCommitted(t, root, "old-pub", "old-asset", pkg(t, "111", "9", 400))

	got, ok := cache.Locate(root, "111", derived)
	if !ok || got.RelPath != derived {
		t.Errorf("Locate = %q, want the copy already at %q", got.RelPath, derived)
	}
}

func TestRelocateMovesTheDirectoryAndPrunesTheOldOne(t *testing.T) {
	root := t.TempDir()
	from := cache.RelPath("pub", "old-slug-111")
	storeCommitted(t, root, "pub", "old-slug-111", pkg(t, "111", "9", 400))
	to := cache.RelPath("pub", "new-slug-111")

	if err := cache.Relocate(root, from, to); err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(to))); err != nil {
		t.Fatalf("file is not at the new path: %v", err)
	}
	// quarry reads the pack facet from the directory, so the old directory must go.
	if _, err := os.Stat(filepath.Join(root, "pub", "old-slug-111")); !os.IsNotExist(err) {
		t.Error("the emptied old directory survived the move")
	}
}

func TestRelocateIsANoOpWhenAlreadyInPlace(t *testing.T) {
	root := t.TempDir()
	rel := cache.RelPath("pub", "asset-1")
	storeCommitted(t, root, "pub", "asset-1", pkg(t, "111", "9", 400))
	if err := cache.Relocate(root, rel, rel); err != nil {
		t.Errorf("Relocate onto itself = %v, want nil: this is the common adopt case", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Errorf("the no-op relocation lost the file: %v", err)
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
