package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/model"
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
	if err != nil {
		t.Errorf("the destination file was disturbed: %v", err)
	} else if fi.Size() != 900 {
		t.Errorf("the destination file was disturbed: size %d, want 900", fi.Size())
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
	n, bytesFreed := cache.SweepTemps(t.Context(), root, cutoff)
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
		if _, _, err := cache.Hash(t.Context(), root, rel); err == nil {
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

	sha, size, err := cache.Hash(t.Context(), root, p.RelPath)
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
	n, bytes := cache.SweepTemps(t.Context(), filepath.Join(t.TempDir(), "never-created"), time.Now())
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

	if _, ok := cache.Scan(t.Context(), root).Find("111", "", nil, rel); ok {
		t.Fatal("the canonical exclude did not skip the file")
	}
	for _, spelling := range []string{"./" + rel, "pub/./asset-1/asset-1.unitypackage"} {
		if _, ok := cache.Scan(t.Context(), root).Find("111", "", nil, spelling); ok {
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
		// The working directory itself, which is the one spelling where a cleaned root is
		// not a prefix of the paths joined under it: "." cleans to "." while
		// Join(".", "pub/a") cleans to "pub/a", carrying no "./" for a prefix test to
		// match. Every prune site is dead for `--library .` without this.
		"dot": func(t *testing.T, base string) string {
			t.Chdir(base)
			return "."
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
			if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(
				cache.RelPath("pub", "new-slug-1")))); err != nil {
				t.Errorf("root spelled %q: the file is not at the new path: %v", root, err)
			}
			// quarry reads the pack facet from the directory, so the old one must go.
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

	if _, ok := cache.Scan(t.Context(), root).Find("111", "", nil); !ok {
		t.Fatal("the candidate is not findable at all")
	}
	for _, bad := range []string{"/etc/passwd", "../outside/x.unitypackage"} {
		if _, ok := cache.Scan(t.Context(), root).Find("111", "", nil, bad); ok {
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
	if _, ok := cache.Scan(t.Context(), root).Find("115488", "", nil); ok {
		t.Error("the adopt scan offered an uncommitted download temp as a candidate")
	}
	old := time.Unix(1600000000, 0)
	if err := os.Chtimes(p.TempPath(), old, old); err != nil {
		t.Fatal(err)
	}
	n, freed := cache.SweepTemps(t.Context(), root, time.Unix(1700000000, 0))
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
	ix := cache.Scan(t.Context(), root)
	for _, spelling := range []string{inPlace, "./" + inPlace, "pub-one//asset-1/asset-1.unitypackage"} {
		got, ok := ix.Find("1", spelling, nil)
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

// Store and Canonical are the write gate and the read gate on the same path, and nothing
// else holds them to the same alphabet. A segment Store accepts but Canonical refuses
// creates a file no later run can resolve: Verify answers false, the entry becomes an
// exclusion that refuses every adopt candidate, and the asset re-downloads in full every
// run while the only output is a warning about a superseded copy.
func TestEveryPathStoreCanWriteIsOneCanonicalAccepts(t *testing.T) {
	for _, tc := range []struct{ name, publisher, asset string }{
		{"ordinary names", "acme-tools", "quick-outline-115488"},
		{"publisher id fallback", "publisher-1234", "quick-outline-115488"},
		{"asset id fallback", "acme-tools", "115488"},
		{"both fallbacks", "publisher-unknown", "115488"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cache.Canonical(cache.RelPath(tc.publisher, tc.asset)); err != nil {
				t.Errorf("Store would write %q, which Canonical refuses: %v",
					cache.RelPath(tc.publisher, tc.asset), err)
			}
		})
	}
}

func TestStoreRefusesASegmentCanonicalWouldNotResolve(t *testing.T) {
	root := t.TempDir()
	// A colon is legal in a Linux path and is the one character the two gates disagreed
	// about, so it is what a product id carrying one would produce.
	if _, err := cache.Store(root, "acme", "quick-outline-115:488", strings.NewReader("x")); err == nil {
		t.Error("Store wrote a path Canonical refuses")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the refused Store left %d entry/entries under the root", len(entries))
	}
}

// The sweep walks the whole library, so its filename prefix is the only thing between it
// and the user's own files — and a library pointed at a project directory holds the
// lockfile's temps too. The positive control matters as much as the survivors: without
// it this passes on a sweep that removes nothing at all.
func TestTheSweepRemovesOnlyWhatItWrote(t *testing.T) {
	root := t.TempDir()
	// An abandoned download: Store leaves the bytes in a temp and Commit is never called.
	if _, err := cache.Store(root, "pub", "asset-1", strings.NewReader("a partial body")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	keep := map[string]string{
		"a committed package": filepath.Join(root, "pub", "asset-1", "asset-1.unitypackage"),
		"an unrelated file":   filepath.Join(root, "pub", "asset-1", "notes.txt"),
		"a dotfile":           filepath.Join(root, "pub", ".DS_Store"),
	}
	for what, p := range keep {
		if err := os.WriteFile(p, []byte("keep me"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", what, err)
		}
	}

	n, freed := cache.SweepTemps(t.Context(), root, time.Now().Add(time.Hour))
	if n != 1 {
		t.Errorf("swept %d files, want exactly the one abandoned temp", n)
	}
	if freed == 0 {
		t.Error("freed 0 bytes for a temp that held some")
	}
	for what, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the sweep removed %s (%s)", what, p)
		}
	}
}

// Commit is the one write here that deliberately lands on an occupied path: the occupant
// is this asset's own superseded version. A Commit that refused it by symmetry with
// Relocate would break every re-download while leaving this package's suite green.
//
// The mode is the destination's when there is one, so a library deliberately locked down
// is not widened by a re-download, and 0644 otherwise — CreateTemp makes the temp 0600
// and the rename would carry that over, leaving a downloaded package owner-only while an
// adopted one keeps the 0644 it arrived with.
func TestCommitReplacesThisAssetsSupersededCopyAndSettlesItsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to settle")
	}
	root := t.TempDir()
	final := filepath.Join(root, "pub", "asset-1", "asset-1.unitypackage")

	storeCommitted(t, root, "pub", "asset-1", []byte("old bytes"))
	fi, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("a freshly committed package is mode %v, want 0644", got)
	}

	if err := os.Chmod(final, 0o600); err != nil {
		t.Fatal(err)
	}
	storeCommitted(t, root, "pub", "asset-1", []byte("new bytes"))

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new bytes" {
		t.Errorf("committed file = %q, want the second commit to have replaced the first", got)
	}
	// Windows reports a different permission set, so only the Unix modes are asserted.
	if fi, err := os.Stat(final); err == nil && runtime.GOOS != "windows" {
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("mode = %v, want the destination's own 0600 preserved", mode)
		}
	}
}

// Every failure that removes the temp also unwinds the directories Store created for it.
// Without that, an asset whose commit keeps failing leaves an empty <publisher>/<asset>/
// behind on every attempt, in a tree quarry walks. The trigger here is the temp going
// missing under Commit, which is what a second run sweeping with a stale clock does.
// Relocate was the one directory-creating path here that did not unwind. MkdirAll makes
// the destination's parents before the rename, and a rename fails with the source held
// open — an editor, an on-access scanner, which on Windows is a refusal rather than a
// retry — so every attempt left an empty <publisher>/<asset>/ in the tree quarry walks,
// on every run, reported only as a per-asset warning.
func TestAFailedRelocateUnwindsTheDirectoriesItCreated(t *testing.T) {
	root := t.TempDir()
	// A source that is not there is the same shape a sharing violation produces: the
	// destination's parents are made, then the rename fails.
	err := cache.Relocate(root, "pub/old-1/old-1.unitypackage", "pub/new-1/new-1.unitypackage")
	if err == nil {
		t.Fatal("Relocate accepted a source that does not exist")
	}
	for _, dir := range []string{
		filepath.Join(root, "pub", "new-1"),
		filepath.Join(root, "pub"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived a failed relocate", dir)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the prune walked past the library root: %v", err)
	}
}

// The other half: a destination directory that already held something must survive, or a
// failed move takes a sibling asset's directory with it.
func TestAFailedRelocateLeavesAnOccupiedDestinationDirectoryAlone(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "pub", "sibling-2", []byte("a body"))
	// Same publisher directory, so the prune walks up into one that is not empty.
	err := cache.Relocate(root, "pub/old-1/old-1.unitypackage", "pub/new-1/new-1.unitypackage")
	if err == nil {
		t.Fatal("Relocate accepted a source that does not exist")
	}
	if _, err := os.Stat(filepath.Join(root, "pub", "sibling-2", "sibling-2.unitypackage")); err != nil {
		t.Errorf("a failed relocate removed a sibling asset's directory: %v", err)
	}
}

func TestAFailedCommitUnwindsTheDirectoriesStoreCreated(t *testing.T) {
	root := t.TempDir()
	p, err := cache.Store(root, "pub", "asset-1", strings.NewReader("a body"))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := os.Remove(p.TempPath()); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err == nil {
		t.Fatal("Commit succeeded with no temp to rename")
	}
	for _, dir := range []string{
		filepath.Join(root, "pub", "asset-1"),
		filepath.Join(root, "pub"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s survived a failed commit", dir)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the prune walked past the library root: %v", err)
	}
}

// An entry that records a delivered id is saying the file's own descriptor claimed it.
// A package carrying no descriptor cannot satisfy that, and passing it would verify on
// size alone — which is exactly what an entry with no recorded id already does, so the
// two cases have to stay apart.
func TestVerifyFailsWhenARecordedDeliveredIdHasNoDescriptor(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("a readable gzip stream with no store metadata"))
	zw.Close()
	body := buf.Bytes()

	p := storeCommitted(t, root, "pub", "asset-1", body)
	if !cache.Verify(root, p.RelPath, int64(len(body)), "") {
		t.Fatal("an entry with no recorded delivered id must verify on size alone")
	}
	if cache.Verify(root, p.RelPath, int64(len(body)), "683375") {
		t.Error("a package with no descriptor verified against a recorded delivered id")
	}
	// Falling back to size does not mean falling back to nothing: the size still has to
	// match exactly, or truncation stops being detectable for descriptor-less packages.
	if cache.Verify(root, p.RelPath, int64(len(body))+5, "") {
		t.Error("Verify accepted a size mismatch even with no delivered id recorded")
	}
}

// SameFile answers the question SamePath cannot on a case-insensitive filesystem, where
// two spellings differing only in case name one file. That difference is observable only
// on Windows and macOS, which CI runs; what every platform can check is that it answers
// from the filesystem and refuses rather than guessing when either side is unsafe or
// absent, because both callers are deciding whether to delete.
func TestSameFileAnswersFromTheFilesystem(t *testing.T) {
	root := t.TempDir()
	one := storeCommitted(t, root, "pub", "asset-1", pkg(t, "1", "v1", 400))
	two := storeCommitted(t, root, "pub", "asset-2", pkg(t, "2", "v1", 400))

	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"one file spelled two ways", one.RelPath, "./" + one.RelPath, true},
		{"two different files", one.RelPath, two.RelPath, false},
		{"a path with nothing on it", one.RelPath, cache.RelPath("pub", "absent"), false},
		{"an unsafe path", one.RelPath, "../escape", false},
		{"two unsafe paths are still not one file", "../escape", "../escape", false},
		{"an empty path", one.RelPath, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cache.SameFile(root, tc.a, tc.b); got != tc.want {
				t.Errorf("SameFile(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Two spellings of one file, which is what Windows and macOS hand this package for free.
// A symlinked alias reproduces the condition on a case-sensitive filesystem too: two
// distinct names that the filesystem resolves to one file. Refusing that as an occupied
// destination fails the adopt of a package already exactly where it belongs, forever and
// on those platforms alone — Index.Find matches it with SameFile and then Relocate, the
// one export that actually moves the file, refuses what Find just handed it.
func TestRelocateOntoAnAliasOfItselfIsANoOp(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "Pub", "asset-111", pkg(t, "111", "9", 400))
	// Relative, and pointing at a sibling inside the library: an alias for a directory
	// that is already there, which is all a case-insensitive filesystem is.
	if err := os.Symlink("Pub", filepath.Join(root, "pub")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	from, to := "Pub/asset-111/asset-111.unitypackage", "pub/asset-111/asset-111.unitypackage"
	if cache.SamePath(from, to) {
		t.Fatal("precondition: SamePath already collapses these, so there is nothing to show")
	}
	if !cache.SameFile(root, from, to) {
		t.Fatal("precondition: the two spellings do not name one file")
	}
	if err := cache.Relocate(root, from, to); err != nil {
		t.Errorf("Relocate refused a destination that is the source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(from))); err != nil {
		t.Errorf("the package was lost: %v", err)
	}
}

// Canonical checks a spelling, and a spelling cannot answer this: every segment of
// "link/victim" is an ordinary name, so the lexical confinement passes while the path
// resolves straight out of the library. The value comes from the lockfile, which is
// committed and hand-editable, and RemoveStale deletes what it is given.
func TestARecordedPathCannotReachOutsideTheLibraryThroughASymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	outside := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	storeCommitted(t, root, "pub", "asset-111", pkg(t, "111", "9", 400))
	victim := filepath.Join(outside, "victim.unitypackage")
	if err := os.WriteFile(victim, pkg(t, "111", "9", 400), 0o644); err != nil {
		t.Fatal(err)
	}
	// Relative, so what these calls refuse is the escape itself rather than the simpler
	// fact of an absolute link.
	if err := os.Symlink(filepath.Join("..", "elsewhere"), filepath.Join(root, "link")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	escaping := "link/victim.unitypackage"

	if err := cache.RemoveStale(root, escaping); err == nil {
		t.Error("RemoveStale followed a link out of the library")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("RemoveStale deleted a file outside the library: %v", err)
	}
	if err := cache.Relocate(root, escaping, cache.RelPath("pub", "asset-222")); err == nil {
		t.Error("Relocate moved a file in from outside the library")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Relocate moved a file outside the library: %v", err)
	}
	if cache.Verify(root, escaping, 400, "9") {
		t.Error("Verify accepted a file outside the library as this asset's cached copy")
	}
	if _, _, err := cache.Hash(t.Context(), root, escaping); err == nil {
		t.Error("Hash read a file outside the library")
	}
}

// Both walks are where a large run spends its time, and main's handler has already taken
// SIGINT's default action away, so a walk that ignores the context cannot be escalated out
// of: the second and third Ctrl-C do nothing either.
func TestBothWalksStopWhenTheContextEnds(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "pub", "asset-111", pkg(t, "111", "9", 400))
	partial, err := cache.Store(root, "pub", "asset-222", strings.NewReader("an abandoned partial"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, ok := cache.Scan(ctx, root).Find("111", "", nil); ok {
		t.Error("Scan went on parsing headers after the run was told to stop")
	}
	if n, _ := cache.SweepTemps(ctx, root, time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("SweepTemps removed %d file(s) after the run was told to stop", n)
	}
	if _, err := os.Stat(partial.TempPath()); err != nil {
		t.Errorf("the cancelled sweep deleted a temp anyway: %v", err)
	}
	// The positive control: with a live context the same calls do their work, so the
	// assertions above cannot pass by the fixtures simply being wrong.
	if _, ok := cache.Scan(t.Context(), root).Find("111", "", nil); !ok {
		t.Error("Scan found nothing even with a live context")
	}
	if n, _ := cache.SweepTemps(t.Context(), root, time.Now().Add(time.Hour)); n != 1 {
		t.Errorf("SweepTemps reclaimed %d temp(s) with a live context, want 1", n)
	}
}

// The write gate has to refuse exactly what the read gate refuses. Store used plain
// os.MkdirAll and os.CreateTemp against a lexically joined path while Verify, Hash,
// RemoveStale and Relocate all went through os.Root, so a symlinked publisher directory —
// what a user does when one publisher outgrows the disk holding a 75 GB library — was
// writable and then unreadable. The download succeeded and was recorded, and every later
// run found Verify false and the adopt scan empty (WalkDir does not descend a symlink),
// classified the asset CacheMissing and fetched the whole package again. Forever, with
// nothing printed: a 23 GB transfer on every sync, reported as "cache-missing 1".
func TestStoreRefusesToWriteThroughASymlinkTheReadsWouldRefuse(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"a link off the library", ""},
		{"a link that stays inside it", "storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "library")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if target == "" {
				target = filepath.Join(base, "disk2")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(filepath.Join(root, target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "bigpub")); err != nil {
				t.Skipf("this filesystem does not support symlinks: %v", err)
			}

			body := pkg(t, "111", "9", 400)
			p, err := cache.Store(root, "bigpub", "asset-111", bytes.NewReader(body))
			if err == nil {
				// The half that made it silent: Store succeeding is only harmless if
				// every later read agrees, and none of them does.
				if err := p.Commit(); err != nil {
					t.Fatalf("Store accepted the path but Commit refused it: %v", err)
				}
				if !cache.Verify(root, p.RelPath, p.Size, "9") {
					t.Fatal("Store wrote a package Verify refuses, which re-downloads it on every run")
				}
				ix := cache.Scan(t.Context(), root)
				if _, ok := ix.Find("111", p.RelPath, nil); !ok {
					t.Fatal("Store wrote a package the adopt scan cannot see, which re-downloads it on every run")
				}
				return
			}
			// Refused is the other acceptable answer, and the one that names the cause.
			if !strings.Contains(err.Error(), "symlink") {
				t.Errorf("Store refused the path without naming the symlink: %v", err)
			}
			if !strings.Contains(err.Error(), "bigpub") {
				t.Errorf("Store refused the path without naming the segment: %v", err)
			}
		})
	}
}

// pruneEmptyParents walked up with plain os.Remove over a lexically joined path, so the
// directory it emptied under a symlinked publisher was removed by deleting the *link* and
// leaving the directory it pointed at. A user who moved a publisher to another disk would
// find the link gone and their library quietly rearranged.
func TestPruningNeverRemovesALinkTheUserPut(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	away := filepath.Join(base, "disk2")
	if err := os.MkdirAll(away, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bigpub")
	if err := os.Symlink(away, link); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	// Whatever Store does with the link, the cleanup that follows a discarded download
	// must not take the link with it.
	if p, err := cache.Store(root, "bigpub", "asset-111", bytes.NewReader(pkg(t, "111", "9", 400))); err == nil {
		if err := p.Discard(); err != nil {
			t.Fatalf("Discard: %v", err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the user's symlink was removed: %v", err)
	}
}

// Invariant 15 spans two packages: model promises a slug that is always one usable
// segment, and cache.safeSegment plus Canonical enforce it. Both halves are well tested
// in isolation, and the cache side uses hand-written strings — so nothing runs a real
// model.Asset through PublisherSlug()/Slug() into Store, and the seam between the promise
// and the enforcement is unwatched.
//
// What that misses: slugify currently emits only [a-z0-9-] with the ends trimmed, which
// is exactly why safeSegment's leading-dot and control-character rules and Canonical's
// colon rule are unreachable from real input. Widen the character class at all — keep dots
// or underscores, transliterate rather than fold — and a name derives a segment Store
// refuses, which fails that one asset on every run with "unsafe asset slug" and nothing
// linking it back to the slug change. Both packages' suites stay green.
func TestEverySlugModelDerivesIsOneCacheWillAccept(t *testing.T) {
	for _, a := range []model.Asset{
		{ID: "1", Name: "Quick Outline", Publisher: model.Publisher{ID: "37073", Name: "Chris Nolet"}},
		{ID: "2", Name: "日本語のアセット", Publisher: model.Publisher{ID: "1", Name: "パブリッシャー"}},
		{ID: "3", Name: "...", Publisher: model.Publisher{ID: "2", Name: "..."}},
		{ID: "4", Name: "../../etc/passwd", Publisher: model.Publisher{ID: "3", Name: "../.."}},
		{ID: "5", Name: `a\b:c/d`, Publisher: model.Publisher{ID: "4", Name: `x:\y`}},
		{ID: "6", Name: "con", Publisher: model.Publisher{ID: "5", Name: "con"}},
		{ID: "7", Name: "aux.txt", Publisher: model.Publisher{ID: "6", Name: "COM1"}},
		{ID: "8", Name: "  ", Publisher: model.Publisher{ID: "7", Name: ""}},
		{ID: "9", Name: "-", Publisher: model.Publisher{ID: "8", Name: "----"}},
		{ID: "10", Name: "a\x00b", Publisher: model.Publisher{ID: "9", Name: "tab\there"}},
		{ID: "11", Name: "emoji 🎮 pack", Publisher: model.Publisher{ID: "10", Name: "🎨"}},
		{ID: "12", Name: "trailing.", Publisher: model.Publisher{ID: "11", Name: "trailing."}},
	} {
		t.Run(a.ID, func(t *testing.T) {
			root := t.TempDir()
			pub, slug := a.PublisherSlug(), a.Slug()
			p, err := cache.Store(root, pub, slug, bytes.NewReader(pkg(t, a.ID, "v1", 400)))
			if err != nil {
				t.Fatalf("cache refuses the slugs model derived for %q / %q: pub=%q slug=%q: %v",
					a.Publisher.Name, a.Name, pub, slug, err)
			}
			if err := p.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			// Three segments deep, because quarry fills its pack facet only when a path
			// has at least three parts.
			parts := strings.Split(p.RelPath, "/")
			if len(parts) != 3 {
				t.Errorf("RelPath %q is %d segments, want 3", p.RelPath, len(parts))
			}
			for _, seg := range parts {
				if seg == "" {
					t.Errorf("RelPath %q has an empty segment", p.RelPath)
				}
			}
			// And what was written has to be readable back through the same gate.
			if !cache.Verify(root, p.RelPath, p.Size, "v1") {
				t.Errorf("Verify refuses the path Store just wrote: %q", p.RelPath)
			}
		})
	}
}

// library_path can be a symlink: pointing a 75 GB mirror at another disk is the same move
// docs/design.md already treats as reasonable for one publisher directory, and a Windows
// junction reports as one too.
//
// Walking over the path rather than through the root made both whole-tree passes stop at
// the link and do nothing. Nothing else broke, which is what made it invisible — os.Root
// resolves a symlinked root like any other directory, so Store, Commit, Verify and Hash
// all kept working. Scan returned an empty index, so every owned asset classified
// cache-missing and re-downloaded in full on every run, and a delisted asset sitting in
// the library was reported unavailable; SweepTemps reclaimed nothing, and reports a count
// the caller only announces when it is non-zero.
func TestASymlinkedLibraryRootIsStillWalked(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	root := filepath.Join(base, "library")
	storeCommitted(t, real, "pub", "asset-111", pkg(t, "111", "9", 400))
	stale := filepath.Join(real, "pub", "asset-111", ".unity-sync-dl-abandoned")
	if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	// The sweep runs before the scan in a real run, and has to find the temp through the
	// same link.
	n, freed := cache.SweepTemps(context.Background(), root, time.Now().Add(time.Hour))
	if n != 1 || freed != int64(len("partial")) {
		t.Errorf("sweep through a symlinked root reclaimed %d file(s), %d bytes; want 1, %d",
			n, freed, len("partial"))
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the abandoned temp survived a sweep through a symlinked root")
	}

	got, ok := cache.Scan(context.Background(), root).Find("111", "", nil)
	if !ok {
		t.Fatal("Scan through a symlinked root found no candidate, so adoption would " +
			"re-download the whole library and a delisted asset would read as unavailable")
	}
	if want := cache.RelPath("pub", "asset-111"); got.RelPath != want {
		t.Errorf("candidate RelPath = %q, want %q relative to the root", got.RelPath, want)
	}
}

// A symlinked directory inside the library is still not descended, which is what the
// confinement rule rests on: Store refuses to write through one, so a package found under
// one would be a file no later read could resolve.
func TestTheWalkStillDoesNotDescendALinkInsideTheLibrary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "library")
	outside := filepath.Join(base, "elsewhere", "pub", "asset-222")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "asset-222.unitypackage"),
		pkg(t, "222", "9", 400), 0o644); err != nil {
		t.Fatal(err)
	}
	storeCommitted(t, root, "pub", "asset-111", pkg(t, "111", "9", 400))
	if err := os.Symlink(filepath.Join("..", "elsewhere"), filepath.Join(root, "link")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	ix := cache.Scan(context.Background(), root)
	if _, ok := ix.Find("222", "", nil); ok {
		t.Error("the scan descended a symlink and offered a package from outside the library")
	}
	if _, ok := ix.Find("111", "", nil); !ok {
		t.Error("the scan stopped at the link instead of skipping it")
	}
}

// The exclusion has to be matched by identity as well as by spelling. On Windows and
// macOS "Pub/a.unitypackage" and "pub/a.unitypackage" are one file that no canonical form
// collapses, and a symlinked alias reproduces that on a case-sensitive filesystem.
//
// Every other exclusion test spells the exclusion so that it canonicalises to the
// candidate's own RelPath, which the string skip already catches — so the identity half
// was dead under test on all three platforms CI runs, and deleting it left the suite
// green. What it stops: the recorded copy failed verification, so the syncer excludes it;
// matched on spelling alone the scan re-offers that same damaged file as its own adopt
// candidate, and adopt hashes it and records the damaged digest as the asset's truth,
// through the one door that skips the download guards.
func TestAnExcludedFileIsSkippedUnderItsOtherSpelling(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "Pub", "asset-111", pkg(t, "111", "9", 400))
	if err := os.Symlink("Pub", filepath.Join(root, "pub")); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}
	scanned := "Pub/asset-111/asset-111.unitypackage"
	recorded := "pub/asset-111/asset-111.unitypackage"
	if cache.SamePath(scanned, recorded) {
		t.Fatal("precondition: SamePath already collapses these, so the string skip would catch it")
	}
	if !cache.SameFile(root, scanned, recorded) {
		t.Fatal("precondition: the two spellings do not name one file")
	}

	ix := cache.Scan(t.Context(), root)
	if _, ok := ix.Find("111", "", nil); !ok {
		t.Fatal("precondition: the scan did not index the package at all")
	}
	if got, ok := ix.Find("111", "", nil, recorded); ok {
		t.Errorf("Find offered %q, which is the excluded file under its other spelling: a "+
			"copy that just failed verification would be re-adopted and its digest recorded",
			got.RelPath)
	}
}

// The two MkdirAll calls were the paths that created directories and did not unwind them.
// Every other failure in this file does, and the comment inside Store says so — "every
// failure below also unwinds", where "below" was the load-bearing word: MkdirAll makes
// <publisher>/ and then <asset>/, so a failure on the second leaves the first in the tree
// quarry walks, for every asset under that publisher, on every run.
//
// A segment past NAME_MAX is the portable trigger. It is also a real one: the asset slug
// is slugify(name) + "-" + id and the store does not bound a product name, so a long
// enough one derives a directory the filesystem refuses. Both levels of the prune are
// needed, since pruneEmptyParents stops at a directory it cannot open and would not walk
// up from a leaf that was never created.
func TestStoreUnwindsWhenADirectoryCannotBeCreated(t *testing.T) {
	root := t.TempDir()
	tooLong := strings.Repeat("a", 300)
	if _, err := cache.Store(root, "pub", tooLong, strings.NewReader("a body")); err == nil {
		t.Fatalf("Store accepted a %d-character segment", len(tooLong))
	}
	if _, err := os.Stat(filepath.Join(root, "pub")); !os.IsNotExist(err) {
		t.Error("an empty publisher directory survived a failed Store")
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the prune walked past the library root: %v", err)
	}

	// The other half: a publisher directory that already holds an asset must survive.
	storeCommitted(t, root, "pub", "sibling-2", []byte("a body"))
	if _, err := cache.Store(root, "pub", tooLong, strings.NewReader("a body")); err == nil {
		t.Fatal("Store accepted an over-long segment the second time")
	}
	if _, err := os.Stat(filepath.Join(root, "pub", "sibling-2", "sibling-2.unitypackage")); err != nil {
		t.Errorf("a failed Store removed a sibling asset's directory: %v", err)
	}
}

// Relocate's own MkdirAll, the same shape one level along: the destination's grandparent
// is created and the parent refused, so the failure is reported as a per-asset warning
// with an empty <publisher>/ left behind.
func TestRelocateUnwindsWhenADirectoryCannotBeCreated(t *testing.T) {
	root := t.TempDir()
	storeCommitted(t, root, "from", "old-1", []byte("a body"))
	tooLong := strings.Repeat("a", 300)
	err := cache.Relocate(root, "from/old-1/old-1.unitypackage",
		"dest/"+tooLong+"/"+tooLong+".unitypackage")
	if err == nil {
		t.Fatal("Relocate accepted an over-long destination segment")
	}
	if _, err := os.Stat(filepath.Join(root, "dest")); !os.IsNotExist(err) {
		t.Error("an empty destination directory survived a failed relocate")
	}
	if _, err := os.Stat(filepath.Join(root, "from", "old-1", "old-1.unitypackage")); err != nil {
		t.Errorf("the source did not survive a failed relocate: %v", err)
	}
}

// The prefer loop is only half of Find's selection. With no candidate at preferRel it
// falls through to walk order, and that arm was never reached: the sibling test always
// passes a spelling that matches one of its two candidates. It is also the arm the gates
// below rest on.
func TestFindFallsBackToWalkOrderWithNoCopyInPlace(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"a-pub/stray-1/stray-1.unitypackage", "z-pub/other-1/other-1.unitypackage"} {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, pkg(t, "1", "v1", 400), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := cache.Scan(t.Context(), root).Find("1", "pub/asset-1/asset-1.unitypackage", nil)
	if !ok {
		t.Fatal("Find found nothing with no copy at the derived path")
	}
	if got.RelPath != "a-pub/stray-1/stray-1.unitypackage" {
		t.Errorf("Find chose %q, want the first in walk order", got.RelPath)
	}
}

// accept runs inside the selection, so a candidate the caller rejects cannot mask one it
// would take. Applied to what Find hands back instead, the copy at the derived path wins
// the preference, fails the caller's gate, and the intact copy two files along is never
// examined: the asset re-downloads in full — up to 23 GB — which is the outcome adoption
// exists to avoid. A cloud-sync conflicted copy is the shape that produces it, with the
// current build under the conflicted name and a stale one left at the plain path.
func TestFindDoesNotLetARejectedCandidateMaskAnAcceptableOne(t *testing.T) {
	root := t.TempDir()
	derived := "pub/asset-1/asset-1.unitypackage"
	stale := filepath.Join(root, filepath.FromSlash(derived))
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, pkg(t, "1", "old-version", 400), 0o644); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, "pub", "asset-1", "asset-1 (conflicted copy).unitypackage")
	if err := os.WriteFile(current, pkg(t, "1", "current-version", 400), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := cache.Scan(t.Context(), root).Find("1", derived, func(c cache.Candidate) bool {
		return c.Metadata.VersionID == "current-version"
	})
	if !ok {
		t.Fatal("the preferred copy failed the gate and masked the acceptable one")
	}
	if got.Metadata.VersionID != "current-version" {
		t.Errorf("Find chose %q at version %q, want the copy the caller accepts",
			got.RelPath, got.Metadata.VersionID)
	}
}
