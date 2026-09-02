package cache_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
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
	// The Windows-shaped values matter on the platform this suite does not run on.
	// filepath.Clean there strips a drive prefix before resolving "..", then restores it,
	// so "Z:../../x" cleans to itself and a leading-".." test never sees the escape;
	// resolve then joins it under the root and the ".." walk right back out. Canonical
	// works in slash space precisely so these fail here too.
	for _, rel := range []string{
		"", "/etc/passwd", "../escape.unitypackage", "a/../../escape",
		"Z:../../../../../Users/me/Documents/thesis.docx",
		"C:../x",
		`pub\a\a.unitypackage`,
		"pub/con/a.unitypackage",
	} {
		if cache.Verify(root, rel, 1, "") {
			t.Errorf("Verify accepted path %q", rel)
		}
		if _, _, err := cache.Hash(root, rel); err == nil {
			t.Errorf("Hash accepted path %q", rel)
		}
		// RemoveStale deletes. A path that escapes the root would delete a file the tool
		// never wrote, and the lockfile it takes these from is hand-editable.
		if err := cache.RemoveStale(root, rel); err == nil {
			t.Errorf("RemoveStale accepted path %q", rel)
		}
		if _, err := cache.Canonical(rel); err == nil {
			t.Errorf("Canonical accepted path %q", rel)
		}
	}
}

// The digest and size Hash returns are what an adoption records as the asset's truth, so a
// wrong answer here is laundered into the lockfile through the one route that skips the
// download guards.
func TestHashReportsTheFilesRealDigestAndSize(t *testing.T) {
	root := t.TempDir()
	body := pkg(t, "111", "v1", 4096)
	p := storeCommitted(t, root, "pub", "asset", body)

	sha, size, err := cache.Hash(root, p.RelPath)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	want := sha256.Sum256(body)
	if sha != hex.EncodeToString(want[:]) {
		t.Errorf("sha = %s, want %s", sha, hex.EncodeToString(want[:]))
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}
}

// RemoveStale is the only function allowed to delete a mirrored package. It has to take
// the directories the removal empties with it, or a rename leaves the old publisher and
// asset folders behind for quarry to index as empty facets.
func TestRemoveStaleDeletesTheFileAndPrunesItsParents(t *testing.T) {
	root := t.TempDir()
	p := storeCommitted(t, root, "pub", "asset", pkg(t, "111", "v1", 1024))

	if err := cache.RemoveStale(root, p.RelPath); err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p.RelPath))); !os.IsNotExist(err) {
		t.Error("the package survived RemoveStale")
	}
	if _, err := os.Stat(filepath.Join(root, "pub")); !os.IsNotExist(err) {
		t.Error("the emptied publisher directory survived")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("pruning climbed past the library root: %v", err)
	}
	// A run re-records a removal it already made; a missing file is done, not an error.
	if err := cache.RemoveStale(root, p.RelPath); err != nil {
		t.Errorf("removing an already-gone file = %v, want nil", err)
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

	if _, ok := cache.Scan(root).Find("111", "", rel); ok {
		t.Fatal("the canonical exclude did not skip the file")
	}
	for _, spelling := range []string{"./" + rel, "pub/./asset-1/asset-1.unitypackage"} {
		if _, ok := cache.Scan(root).Find("111", "", spelling); ok {
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
	spellings := map[string]func(*testing.T, string) string{
		"absolute": func(_ *testing.T, base string) string { return base },
		"trailing slash": func(_ *testing.T, base string) string {
			return base + string(filepath.Separator)
		},
		// The skip takes the subtest's own t: skipping the parent from inside a subtest
		// exits the parent goroutine and fails the whole test instead.
		"dot relative": func(t *testing.T, base string) string {
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
			root := spell(t, base)
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

	if _, ok := cache.Scan(root).Find("111", ""); !ok {
		t.Fatal("the candidate is not findable at all")
	}
	for _, bad := range []string{"/etc/passwd", "../outside/x.unitypackage"} {
		if _, ok := cache.Scan(root).Find("111", "", bad); ok {
			t.Errorf("exclusion %q was dropped and a candidate offered anyway", bad)
		}
	}
}

// Two spellings of one path have to compare equal. Callers hold a path that came out of
// the committed, hand-editable lockfile against one this package derived, and deciding
// they are two files means deleting the one that was just written or refusing to move onto
// a destination that is really the source.
func TestCanonicalCollapsesTheSpellingsOfOnePath(t *testing.T) {
	same := []string{
		"pub/a/a.unitypackage",
		"./pub/a/a.unitypackage",
		"pub//a/a.unitypackage",
		"pub/b/../a/a.unitypackage",
		"./pub/./a/a.unitypackage",
	}
	want, err := cache.Canonical(same[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range same[1:] {
		got, err := cache.Canonical(rel)
		if err != nil {
			t.Errorf("Canonical(%q): %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("Canonical(%q) = %q, want %q", rel, got, want)
		}
		if !cache.SamePath(rel, same[0]) {
			t.Errorf("SamePath(%q, %q) = false", rel, same[0])
		}
	}
	if cache.SamePath("pub/a/a.unitypackage", "pub/b/b.unitypackage") {
		t.Error("SamePath called two different files the same")
	}
	// A value it cannot resolve matches nothing, including another unresolvable one: the
	// caller is about to delete or move something on the strength of the answer.
	if cache.SamePath("../escape", "../escape") {
		t.Error("SamePath matched a pair of paths that leave the root")
	}
}

// The sweep and the adopt scan both recognise an in-flight download by its filename, and
// every other test writes that name by hand. Nothing put a temp Store actually created in
// front of either, so a change to the CreateTemp pattern that no longer matched tempPrefix
// would leave the suite green while 23 GB partials accumulated in the library forever.
func TestATempStoreCreatedIsATempTheSweepAndScanRecognise(t *testing.T) {
	root := t.TempDir()
	p, err := cache.Store(root, "pub", "asset", bytes.NewReader(pkg(t, "115488", "v1", 400)))
	if err != nil {
		t.Fatal(err)
	}
	// The adopt scan must not offer an uncommitted partial as something to adopt: a
	// truncated body can clear the size floor with its descriptor intact.
	if _, ok := cache.Scan(root).Find("115488", ""); ok {
		t.Error("the adopt scan offered an uncommitted download temp as a candidate")
	}
	old := time.Unix(1600000000, 0)
	if err := os.Chtimes(p.TempPath(), old, old); err != nil {
		t.Fatal(err)
	}
	n, freed := cache.SweepTemps(root, time.Unix(1700000000, 0))
	if n != 1 {
		t.Fatalf("SweepTemps reclaimed %d, want 1: Store's temp name no longer matches what "+
			"the sweep looks for, so abandoned downloads are never reclaimed", n)
	}
	if freed != p.Size {
		t.Errorf("freed = %d bytes, want %d", freed, p.Size)
	}
	if _, err := os.Stat(p.TempPath()); !os.IsNotExist(err) {
		t.Error("the temp survived its own sweep")
	}
}

// Store creates the two directories a package lives in before it opens the temp. Discard
// runs whenever a semantic guard rejects a body, which for an asset that never downloads
// successfully means an empty <publisher>/<asset>/ pair left in a tree quarry walks.
func TestDiscardUnwindsTheDirectoriesStoreCreated(t *testing.T) {
	root := t.TempDir()
	p, err := cache.Store(root, "pub", "asset", bytes.NewReader([]byte("rejected")))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	for _, dir := range []string{filepath.Join(root, "pub", "asset"), filepath.Join(root, "pub")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived a discarded download", dir)
		}
	}
	// Never past the root, whatever the removal empties.
	if _, err := os.Stat(root); err != nil {
		t.Errorf("pruning climbed past the library root: %v", err)
	}
}

// The other half, and the one a real run hits: the body itself fails mid-transfer. A
// stalled download is in the failure model, retry reopens a fresh temp for every attempt,
// and a 23 GB package that strands its partial on each one leaves tens of gigabytes
// behind until some later run's sweep clears the grace window.
func TestStoreUnwindsWhenTheBodyFailsMidTransfer(t *testing.T) {
	root := t.TempDir()
	body := io.MultiReader(bytes.NewReader([]byte("first chunk")), errReader{})
	if _, err := cache.Store(root, "pub", "asset", body); err == nil {
		t.Fatal("Store accepted a body that failed mid-transfer")
	}
	for _, dir := range []string{filepath.Join(root, "pub", "asset"), filepath.Join(root, "pub")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived a failed download", dir)
		}
	}
	var temps int
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			temps++
		}
		return nil
	})
	if temps != 0 {
		t.Errorf("%d file(s) left under the root; the partial was not removed", temps)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("the transfer stalled") }

// Relocate is the only export that moves a real file to a caller-supplied destination, so
// both ends have to be confined and not just the source. Asserting an error is not enough:
// an unconfined destination still errors when the source happens not to exist, so the file
// has to be real and the check has to be that nothing landed outside the root.
func TestRelocateConfinesBothEnds(t *testing.T) {
	for _, bad := range []string{"", "/etc/passwd", "../escape.unitypackage", "a/../../escape"} {
		outside := t.TempDir()
		root := filepath.Join(outside, "library")
		src := filepath.Join(root, "pub", "a")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		real := filepath.Join(src, "a.unitypackage")
		if err := os.WriteFile(real, []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := cache.Relocate(root, "pub/a/a.unitypackage", bad); err == nil {
			t.Errorf("Relocate accepted destination %q", bad)
		}
		if _, err := os.Stat(real); err != nil {
			t.Errorf("Relocate to %q moved the source anyway: %v", bad, err)
		}
		// Nothing may have been written above the root, whatever the destination spelled.
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "library" {
			t.Errorf("Relocate to %q wrote outside the library root: %v", bad, entries)
		}

		if err := cache.Relocate(root, bad, "pub/b/b.unitypackage"); err == nil {
			t.Errorf("Relocate accepted source %q", bad)
		}
		if _, err := os.Stat(filepath.Join(root, "pub", "b", "b.unitypackage")); err == nil {
			t.Errorf("Relocate from %q produced a destination file", bad)
		}
	}
}

// Find prefers the copy already at the derived path, so an adopt that is really a no-op
// does not turn into a relocation onto an occupied destination. preferRel comes from the
// same layout the exclusions do, so it has to be compared the same way: raw, a caller
// holding a differently-spelled path silently loses the preference and Relocate refuses.
func TestFindPrefersTheCopyAlreadyInPlaceWhateverItIsCalled(t *testing.T) {
	root := t.TempDir()
	inPlace := "pub-one/asset-1/asset-1.unitypackage"
	for _, rel := range []string{inPlace, "elsewhere/stray-1/stray-1.unitypackage"} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, pkg(t, "1", "v1", 400), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix := cache.Scan(root)
	for _, spelling := range []string{inPlace, "./" + inPlace, "pub-one//asset-1/asset-1.unitypackage"} {
		got, ok := ix.Find("1", spelling)
		if !ok {
			t.Fatalf("Find(%q) found nothing", spelling)
		}
		if got.RelPath != inPlace {
			t.Errorf("Find(%q) chose %q, want the copy already in place at %q",
				spelling, got.RelPath, inPlace)
		}
	}
}

// safeSegment is the gate both slugs pass through, and model keeps the derived ones clear
// of device names. This is the backstop for a segment that arrives another way: on Windows
// MkdirAll would fail with an errno rather than anything naming the cause, and on Linux it
// would happily create a directory the same lockfile cannot be used from on Windows.
func TestStoreRefusesAWindowsReservedSegment(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ publisher, asset string }{
		{"con", "quick-outline-115488"},
		{"CON", "quick-outline-115488"},
		{"chris-nolet", "aux"},
		{"lpt9", "quick-outline-115488"},
	} {
		p, err := cache.Store(root, tc.publisher, tc.asset, bytes.NewReader(pkg(t, "115488", "683375", 500)))
		if err == nil {
			p.Discard()
			t.Errorf("Store(%q, %q) was accepted; Windows reserves that name for a device",
				tc.publisher, tc.asset)
		}
	}
}

// A cachePath is committed and hand-editable, so one missing its filename segment names
// the asset's own directory — which always exists once a run has written there. Without
// the IsDir check a directory whose reported size matched would verify with nothing read.
func TestVerifyRefusesADirectory(t *testing.T) {
	root := t.TempDir()
	rel := cache.RelPath("chris-nolet", "quick-outline-115488")
	storeCommitted(t, root, "chris-nolet", "quick-outline-115488", pkg(t, "115488", "683375", 500))

	dir := path.Dir(rel)
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if cache.Verify(root, dir, fi.Size(), "") {
		t.Errorf("Verify(%q) accepted a directory whose size happened to match", dir)
	}
}
