package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/store"
)

// Failure models this package must keep pinned: the guards between a bad response and a
// committed lockfile entry, and the blast radius of one bad asset.

func TestSemanticGuardsRejectAndDiscard(t *testing.T) {
	good := pkg(t, "1", "v1", 2000)

	cases := []struct {
		name      string
		body      []byte
		lookup    *model.Asset // when set, the re-query reports this
		wantOK    bool
		wantWarn  string
		wantStore bool
	}{
		{name: "not gzip at all", body: []byte("<html>sign in</html>")},
		{name: "descriptor names another product", body: pkg(t, "999", "v1", 2000)},
		{name: "short body, re-query unchanged", body: pkg(t, "1", "v1", 100)},
		{
			// A republish is the one legitimate way to fall below the floor. It warns,
			// stores nothing, and leaves the new build for the next run.
			name:      "short body, re-query shows a republish",
			body:      pkg(t, "1", "v1", 100),
			lookup:    &model.Asset{ID: "1", Version: model.Version{ID: "v2"}, AdvertisedSize: 4000},
			wantOK:    true,
			wantWarn:  "republished",
			wantStore: false,
		},
		{name: "20 bytes short is a warning only", body: pkg(t, "1", "v1", 1980), wantOK: true, wantStore: true},
		{name: "exact", body: good, wantOK: true, wantStore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, lockPath := newRun(t)
			a := asset("1", "Asset", "v1", 2000)
			fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": tc.body}}
			if tc.lookup != nil {
				fs.lookups = map[string]model.Asset{"1": *tc.lookup}
			}
			rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := rep.Results[0]
			final := filepath.Join(root, "pub-one", "asset-1", "asset-1.unitypackage")
			if tc.wantOK {
				if res.Err != nil {
					t.Fatalf("Run rejected an acceptable body: %v", res.Err)
				}
				if tc.wantWarn != "" && !strings.Contains(res.Warning, tc.wantWarn) {
					t.Errorf("warning = %q, want it to mention %q", res.Warning, tc.wantWarn)
				}
				_, statErr := os.Stat(final)
				if tc.wantStore && statErr != nil {
					t.Errorf("expected the package to be committed: %v", statErr)
				}
				if !tc.wantStore && statErr == nil {
					t.Error("a republish stored bytes; it should leave the new build for the next run")
				}
				if leftovers := tempsUnder(t, root); leftovers != 0 {
					t.Errorf("%d temp files survived", leftovers)
				}
				return
			}
			if res.Err == nil {
				t.Fatal("Run accepted a body it should have refused")
			}
			// Nothing may survive at a real cache path.
			if _, err := os.Stat(final); !os.IsNotExist(err) {
				t.Error("a rejected body was committed to the cache")
			}
			if leftovers := tempsUnder(t, root); leftovers != 0 {
				t.Errorf("%d temp files survived a rejected download", leftovers)
			}
		})
	}
}

// The template returns from Run on the first download error. Departing from that is a
// deliberate rule here, so it needs its own test.
func TestOneFailedAssetDoesNotStopTheRest(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Good one", "v1", 500)
	b := asset("2", "Pulled", "v1", 500)
	c := asset("3", "Also good", "v1", 500)
	fs := &fakeStore{
		owned:   []model.Asset{a, b, c},
		bodies:  map[string][]byte{"1": pkg(t, "1", "v1", 500), "3": pkg(t, "3", "v1", 500)},
		fetchEr: map[string]error{"2": store.ErrNotDownloadable},
	}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a, b, c)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	tracked := 0
	for _, e := range rep.Lockfile.Assets {
		if e.Tracked {
			tracked++
		}
	}
	if tracked != 2 {
		t.Errorf("%d assets mirrored, want 2: one failure must not abort the others", tracked)
	}
	// A pulled asset is permanent, so it must not make every future run exit non-zero.
	if rep.Permanent != 1 || rep.Retryable != 0 {
		t.Errorf("Permanent=%d Retryable=%d, want 1 and 0", rep.Permanent, rep.Retryable)
	}
	if rep.Failed() {
		t.Error("a permanently pulled asset made the run report failure")
	}
}

func TestConcurrencyCeilingIsHonoured(t *testing.T) {
	root, lockPath := newRun(t)
	var assets []model.Asset
	bodies := map[string][]byte{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		a := asset(id, "Asset "+id, "v1", 500)
		assets = append(assets, a)
		bodies[id] = pkg(t, id, "v1", 500)
	}
	fs := &fakeStore{owned: assets, bodies: bodies, hold: 20 * time.Millisecond}
	o := opts(root, allSelected(assets...))
	o.Concurrency = 2

	if _, err := Run(context.Background(), fs, lockfile.New(), lockPath, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fs.maxSeen.Load(); got > 2 {
		t.Errorf("peak concurrent fetches = %d, want at most 2; the store is someone else's "+
			"infrastructure and this is the brief's one third-party requirement", got)
	}
	if got := fs.maxSeen.Load(); got < 2 {
		t.Errorf("peak concurrent fetches = %d, want the pool actually used its budget", got)
	}
}

func TestDryRunClassifiesAndTouchesNothing(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)

	// A stale temp and a package sitting off its derived path: a sync would sweep one
	// and relocate the other.
	leaf := filepath.Join(root, "pub-one", "elsewhere")
	os.MkdirAll(leaf, 0o755)
	os.WriteFile(filepath.Join(leaf, ".unity-sync-dl-stale"), []byte("junk"), 0o644)
	os.WriteFile(filepath.Join(leaf, "elsewhere.unitypackage"), pkg(t, "1", "v1", 500), 0o644)
	old := time.Unix(1600000000, 0)
	os.Chtimes(filepath.Join(leaf, ".unity-sync-dl-stale"), old, old)

	before := treeSnapshot(t, root)

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}
	o := opts(root, allSelected(a))
	o.DryRun = true

	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("dry run produced %d results, want 1 — status must classify", len(rep.Results))
	}
	if len(fs.fetched) != 0 {
		t.Errorf("a dry run downloaded %v", fs.fetched)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("a dry run wrote the lockfile")
	}
	if after := treeSnapshot(t, root); after != before {
		t.Errorf("a dry run changed the library tree:\nbefore %v\nafter  %v", before, after)
	}
}

func TestEmptyEnumerationAgainstANonEmptyLockfileIsRefused(t *testing.T) {
	root, lockPath := newRun(t)
	prior := lockfile.New()
	prior.Assets["a-1"] = lockfile.Entry{AssetID: "1"}
	if err := lockfile.Save(lockPath, prior); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(lockPath)

	fs := &fakeStore{}
	_, err := Run(context.Background(), fs, prior, lockPath, opts(root, nil))
	if !errors.Is(err, ErrEmptyLibrary) {
		t.Fatalf("Run = %v, want ErrEmptyLibrary", err)
	}
	after, _ := os.ReadFile(lockPath)
	if string(before) != string(after) {
		t.Error("the refused run rewrote the lockfile anyway")
	}
}

func TestPreDownloadFailureWritesNoLockfileAtAll(t *testing.T) {
	root, lockPath := newRun(t)
	fs := &failingEnumerate{}
	if _, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, nil)); err == nil {
		t.Fatal("Run succeeded despite an enumeration failure")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("a failure before any download still created a lockfile")
	}
}

// The gate the whole adopt design rests on. A file that really is this product, but a
// different build than the store now advertises, must not be adopted: a wrong adopt is
// silent and permanent, where a redundant download is loud and self-correcting.
func TestAdoptRefusesACandidateFromAnotherVersion(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v2", 500)

	// On disk: the right product, stamped with the previous build.
	p, err := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(pkg(t, "1", "v1", 500)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v2", 500)}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class == Adopted {
		t.Fatal("adopted a package stamped with a different version than the store advertises")
	}
	if len(fs.fetched) != 1 {
		t.Errorf("fetched %v, want the asset re-downloaded instead of adopted", fs.fetched)
	}
	_, e, _ := rep.Lockfile.FindByAssetID("1")
	if e.DeliveredVersionID != "v2" {
		t.Errorf("deliveredVersionId = %q, want the freshly downloaded build", e.DeliveredVersionID)
	}
}

// Adoption exists for a file that is not where the current layout would put it, so the
// relocation half needs its own coverage: live QA cannot reach it, because its subject
// already sits at the derived path where relocation is a deliberate no-op.
func TestAdoptRelocatesACandidateFoundOffTheDerivedPath(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Renamed Asset", "v1", 500)

	// The package sits under a stale slug, as it would after an upstream rename.
	stale, err := cache.Store(root, a.PublisherSlug(), "old-slug-1", bytes.NewReader(pkg(t, "1", "v1", 500)))
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Commit(); err != nil {
		t.Fatal(err)
	}

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class != Adopted {
		t.Fatalf("class = %v, want Adopted", rep.Results[0].Class)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("adoption downloaded %v", fs.fetched)
	}

	derived := cache.RelPath(a.PublisherSlug(), a.Slug())
	_, e, _ := rep.Lockfile.FindByAssetID("1")
	if e.CachePath != derived {
		t.Errorf("cachePath = %q, want the derived path %q — a legacy path would keep quarry's "+
			"facets on the old name and strand the file at the next version bump", e.CachePath, derived)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(derived))); err != nil {
		t.Errorf("the adopted file was not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, a.PublisherSlug(), "old-slug-1")); !os.IsNotExist(err) {
		t.Error("the emptied source directory survived the adopt")
	}
}
