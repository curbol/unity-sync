package cache_test

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/fixtures"
)

// pkg builds a gzip stream carrying the store's descriptor, padded to size.
func pkg(t *testing.T, productID, versionID string, size int) []byte {
	t.Helper()
	return fixtures.Package(productID, versionID, size)
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
	if cache.VerifyDeep(t.Context(), root, p.RelPath, p.SHA256) {
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

	got, ok := cache.Scan(t.Context(), root).Find("222", "")
	if !ok || !strings.Contains(got.RelPath, "asset-2") {
		t.Errorf("Locate(222) = %+v, %v", got, ok)
	}
	if _, ok := cache.Scan(t.Context(), root).Find("333", ""); ok {
		t.Error("Locate adopted an abandoned download temp")
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
