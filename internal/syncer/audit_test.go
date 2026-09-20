package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/retry"
	"github.com/curbol/unity-sync/internal/store"
	"github.com/curbol/unity-sync/internal/unitypackage"
)

// Failure models this package must keep pinned: the guards between a bad response and a
// committed lockfile entry, and the blast radius of one bad asset.

func TestSemanticGuardsRejectAndDiscard(t *testing.T) {
	good := pkg(t, "1", "v1", 2000)

	cases := []struct {
		name       string
		body       []byte
		advertised int64        // defaults to 2000
		lookup     *model.Asset // when set, the re-query reports this
		wantOK     bool
		wantWarn   string
		wantStore  bool
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
		{
			// The allowance is capped at 4096 absolute, because the gap it forgives is a
			// fixed alignment artifact rather than a proportion. Every other case here is
			// small enough that advertised/8 is the binding half, so this is the only one
			// that fails if the cap is dropped — and without it a 23 GB package ended
			// cleanly 2 GB early clears the floor and is recorded as that asset's truth.
			name:       "10% short of a large package is below the absolute floor",
			body:       pkg(t, "1", "v1", 90000),
			advertised: 100000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, lockPath := newRun(t)
			advertised := tc.advertised
			if advertised == 0 {
				advertised = 2000
			}
			a := asset("1", "Asset", "v1", advertised)
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
				// A republish is not a failure: nothing was stored and the next run picks
				// up the new build. Counting it would make an ordinary event exit
				// non-zero for every wrapper script watching this.
				if rep.Failed() {
					t.Error("an accepted body made the run exit non-zero")
				}
				return
			}
			if res.Err == nil {
				t.Fatal("Run accepted a body it should have refused")
			}
			// The other half of "a failed download fails its asset, not the run": a
			// corrupt body still has to exit non-zero. Only a pulled asset is permanent
			// and silent, so a guard rejection widened into that bucket would report a
			// clean sync over a download the tool itself refused.
			if !rep.Failed() {
				t.Error("a rejected body left the run exiting 0")
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

// The mirror image of the rule above, and the half with no other observable. An expired
// session makes every remaining download pointless, so it is the one error that stops the
// pool; without that, a session dying at asset 5 of 300 sends the other 295 to the store
// to fail one at a time. Deleting the cancelPool call leaves every other test green.
func TestAnExpiredSessionStopsThePool(t *testing.T) {
	root, lockPath := newRun(t)
	owned, bodies := manyAssets(t, 40)
	// Whichever asset the pool reaches first kills the session; every fetch after that is
	// waste. Keyed to the first fetch served rather than to asset "0", because with one
	// worker the goroutine that wins the semaphore is not the one spawned first, and
	// pinning it to an id made this test fail roughly one run in forty.
	fs := &fakeStore{owned: owned, bodies: bodies, firstErr: store.ErrExpiredSession}

	o := opts(root, allSelected(owned...))
	o.Concurrency = 1
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.fetched) == len(owned) {
		t.Errorf("the pool fetched all %d assets after the session expired; it must stop early",
			len(fs.fetched))
	}
	if !rep.Failed() {
		t.Error("an expired session left the run reporting success")
	}
	// The assets the pool never reached are counted apart from the one that failed. Left
	// in Retryable they are indistinguishable from real failures, and the summary then
	// prints a line per asset — burying the expired session among 39 context cancellations.
	if rep.NotAttempted == 0 {
		t.Error("no asset was recorded as unattempted; the cancelled ones are being reported as failures")
	}
	if rep.Retryable != 1 {
		t.Errorf("Retryable = %d, want only the asset whose fetch actually failed", rep.Retryable)
	}
	named := 0
	for _, r := range rep.Results {
		if r.Err != nil {
			named++
		}
		if r.NotAttempted && r.Err != nil {
			t.Errorf("%s was never attempted but carries an error", r.Asset.Name)
		}
	}
	if named != 1 {
		t.Errorf("%d assets carry an error, want only the one the session died on", named)
	}
}

// republished keeps the size floor on when the re-read itself fails: a Lookup error says
// nothing about whether the body was truncated, and reading it as "republished" would
// discard the one guard that catches a cleanly-ended short stream.
func TestALookupFailureDoesNotExcuseAShortBody(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 100000)
	fs := &fakeStore{
		owned:    []model.Asset{a},
		bodies:   map[string][]byte{"1": pkg(t, "1", "v1", 400)},
		lookupEr: map[string]error{"1": errors.New("the store is unreachable")},
	}

	o := opts(root, allSelected(a))
	o.Retry = retryPolicyWithAttempts(2)
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Err == nil {
		t.Fatal("a short body passed because the republish re-read failed")
	}
	if !strings.Contains(rep.Results[0].Err.Error(), "ended early") {
		t.Errorf("error = %v, want the short-body verdict", rep.Results[0].Err)
	}
	if _, e, _ := rep.Lockfile.FindByAssetID("1"); e.Tracked {
		t.Error("the truncated body was recorded as the asset's truth")
	}
}

// The incremental save runs from inside every download goroutine, so what it writes has to
// survive them running at once. Each save rebuilds the whole document from the shared
// resolutions map, and a snapshot that lost a peer's entry — or a rename ordered against
// the snapshot it came from — shows up here as a mirrored asset missing from the file.
func TestConcurrentDownloadsEachLandInTheLockfile(t *testing.T) {
	root, lockPath := newRun(t)
	owned, bodies := manyAssets(t, 12)
	fs := &fakeStore{owned: owned, bodies: bodies, hold: 2 * time.Millisecond}
	o := opts(root, allSelected(owned...))
	o.Concurrency = 6

	if _, err := Run(context.Background(), fs, lockfile.New(), lockPath, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	saved, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range owned {
		_, e, ok := saved.FindByAssetID(a.ID)
		if !ok || !e.Tracked {
			t.Errorf("asset %s downloaded but is not in the saved lockfile", a.ID)
		}
	}
}

// The other half, and the one the post-run check above cannot see: Run ends with an
// unconditional save that rebuilds the whole document, so a lost incremental write is
// invisible once the run is over. It matters while the run is still going, because the
// incremental save exists precisely so a run killed at asset 90 of 100 keeps the 89.
//
// persist holds the mutex across both the map write and the save. Split into two
// statements, two goroutines reach the rename in the order opposite to how they built
// their snapshots and the older one wins, dropping a record a crash a moment later would
// never get back. What that looks like from outside is two saves in flight at once, and
// Save leaves a temp beside the lockfile for the whole of its write-fsync-rename — so a
// second temp appearing is the serialization being gone.
func TestLockfileSavesNeverOverlap(t *testing.T) {
	root, lockPath := newRun(t)
	owned, bodies := manyAssets(t, 60)

	var overlapped atomic.Bool
	lockDir := filepath.Dir(lockPath)
	sample := func() {
		entries, err := os.ReadDir(lockDir)
		if err != nil {
			return
		}
		var inFlight int
		for _, e := range entries {
			// lockfile.TempPrefix, not a copy of it: a literal here goes stale silently
			// when Save's prefix changes, and this watcher then counts nothing, finds no
			// overlap, and passes forever without observing a single write.
			if strings.HasPrefix(e.Name(), lockfile.TempPrefix) {
				inFlight++
			}
		}
		if inFlight > 1 {
			overlapped.Store(true)
		}
	}

	fs := &fakeStore{owned: owned, bodies: bodies, beforeFetch: func(int) { sample() }}
	o := opts(root, allSelected(owned...))
	o.Concurrency = 12

	done, polled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-done:
				return
			default:
				sample()
				// A save spans a write, an fsync and a rename, so sampling every few
				// tens of microseconds still lands inside one many times over. Spinning
				// without it burns a core and contends on the directory being watched.
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()

	if _, err := Run(context.Background(), fs, lockfile.New(), lockPath, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(done)
	<-polled

	if overlapped.Load() {
		t.Error("two lockfile saves were in flight at once: the write is no longer in the " +
			"same critical section as the map update, so a stale snapshot can land last")
	}
	// The run still has to have left a complete record, or the check above passed only
	// because nothing was written.
	saved, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range owned {
		if _, e, ok := saved.FindByAssetID(a.ID); !ok || !e.Tracked {
			t.Fatalf("asset %s is missing from the saved lockfile", a.ID)
		}
	}
}

func TestConcurrencyCeilingIsHonoured(t *testing.T) {
	root, lockPath := newRun(t)
	assets, bodies := manyAssets(t, 6)
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
	place(t, root, a.PublisherSlug(), a.Slug(), pkg(t, "1", "v1", 500))

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
	place(t, root, a.PublisherSlug(), "old-slug-1", pkg(t, "1", "v1", 500))

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

// A lost lockfile makes every owned asset ask the adopt question at once, and the answer
// comes from one shared scan of the library rather than a walk per asset. Sharing it is
// where a mix-up would show: an index keyed or reused wrongly hands an asset another
// product's file, and adoption is the one route that skips the download guards.
func TestOneScanServesEveryAdoptionInARun(t *testing.T) {
	root, lockPath := newRun(t)
	var owned []model.Asset
	for _, id := range []string{"1", "2", "3", "4"} {
		a := asset(id, "Asset "+id, "v1", 500)
		owned = append(owned, a)
		// Each under a stale slug, so every one of them needs the scan and a relocation.
		place(t, root, a.PublisherSlug(), "old-slug-"+id, pkg(t, id, "v1", 500))
	}

	fs := &fakeStore{owned: owned}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(owned...)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("adoption downloaded %v", fs.fetched)
	}
	for _, a := range owned {
		_, e, ok := rep.Lockfile.FindByAssetID(a.ID)
		if !ok || !e.Tracked {
			t.Fatalf("asset %s was not adopted", a.ID)
		}
		derived := cache.RelPath(a.PublisherSlug(), a.Slug())
		if e.CachePath != derived {
			t.Errorf("asset %s cachePath = %q, want %q", a.ID, e.CachePath, derived)
		}
		// The descriptor at the recorded path must be this asset's own, not a neighbour's.
		m, err := unitypackage.ReadFile(filepath.Join(root, filepath.FromSlash(e.CachePath)))
		if err != nil {
			t.Fatalf("asset %s: reading the adopted file: %v", a.ID, err)
		}
		if m.ID != a.ID {
			t.Errorf("asset %s adopted a package for product %s", a.ID, m.ID)
		}
	}
}

// The one door into the cache that skips every download guard. A truncation or a mid-file
// flip leaves the descriptor intact and can clear the size floor, so an adopt scan that
// considered the file which just failed verification would re-hash the damaged bytes and
// record them as truth.
func TestAFileThatFailedVerificationIsNotAdoptedBackIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fullVerify bool
		damage     func(t *testing.T, path string, size int64)
	}{
		{
			name: "truncated",
			damage: func(t *testing.T, path string, size int64) {
				if err := os.Truncate(path, size-100); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:       "mid-file flip under --verify",
			fullVerify: true,
			damage: func(t *testing.T, path string, _ int64) {
				f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				f.WriteAt([]byte{0xFF}, 3000)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, lockPath := newRun(t)
			a := asset("1", "Asset", "v1", 40000)
			good := pkg(t, "1", "v1", 40000)

			p := place(t, root, a.PublisherSlug(), a.Slug(), good)
			prior := lockfile.New()
			prior.Assets[a.Slug()] = lockfile.Entry{
				AssetID: "1", Name: a.Name,
				Version: lockfile.Version{ID: "v1"},
				Resolution: lockfile.Resolution{
					Tracked:           true,
					ResolvedVersionID: "v1", DeliveredVersionID: "v1",
					SizeBytes: p.Size, SHA256: p.SHA256, CachePath: p.RelPath,
				},
			}
			tc.damage(t, filepath.Join(root, filepath.FromSlash(p.RelPath)), p.Size)

			fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": good}}
			o := opts(root, allSelected(a))
			o.FullVerify = tc.fullVerify

			rep, err := Run(context.Background(), fs, prior, lockPath, o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.Results[0].Class == Adopted {
				t.Fatal("a damaged file was adopted back in and its digest recorded as truth")
			}
			if len(fs.fetched) != 1 {
				t.Errorf("fetched %v, want the damaged asset re-downloaded", fs.fetched)
			}
			_, e, _ := rep.Lockfile.FindByAssetID("1")
			if e.SizeBytes != int64(len(good)) {
				t.Errorf("sizeBytes = %d, want the freshly downloaded %d", e.SizeBytes, len(good))
			}
		})
	}
}

// A rename that also bumps the version downloads to the new derived path, so the prior
// directory would otherwise be left holding a superseded copy of the same asset.
func TestARenameWithAVersionBumpDoesNotStrandTheOldDirectory(t *testing.T) {
	root, lockPath := newRun(t)
	renamed := asset("1", "Brand New Name", "v2", 500)

	old := place(t, root, renamed.PublisherSlug(), "old-name-1", pkg(t, "1", "v1", 500))
	prior := lockfile.New()
	prior.Assets["old-name-1"] = lockfile.Entry{
		AssetID: "1", Name: "Old Name",
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: old.Size, SHA256: old.SHA256, CachePath: old.RelPath,
		},
	}

	fs := &fakeStore{owned: []model.Asset{renamed}, bodies: map[string][]byte{"1": pkg(t, "1", "v2", 500)}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(renamed)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class != Changed {
		t.Fatalf("class = %v, want Changed", rep.Results[0].Class)
	}
	if _, err := os.Stat(filepath.Join(root, renamed.PublisherSlug(), "old-name-1")); !os.IsNotExist(err) {
		t.Error("the superseded copy's directory was left behind after the rename")
	}
	derived := cache.RelPath(renamed.PublisherSlug(), renamed.Slug())
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(derived))); err != nil {
		t.Errorf("the new build is not at its derived path: %v", err)
	}
}

// Both of these tell the user something the design says they should hear, and neither
// changes any other observable, so only an assertion catches their absence.
func TestDownloadWarnings(t *testing.T) {
	t.Run("advertised and delivered versions disagree", func(t *testing.T) {
		root, lockPath := newRun(t)
		a := asset("1", "Asset", "v-advertised", 500)
		fs := &fakeStore{owned: []model.Asset{a},
			bodies: map[string][]byte{"1": pkg(t, "1", "v-delivered", 500)}}

		rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Results[0].Err != nil {
			t.Fatalf("a version mismatch must not fail the asset: %v", rep.Results[0].Err)
		}
		if !strings.Contains(rep.Results[0].Warning, "v-delivered") {
			t.Errorf("warning = %q, want it to name the build actually served", rep.Results[0].Warning)
		}
		_, e, _ := rep.Lockfile.FindByAssetID("1")
		if e.ResolvedVersionID != "v-advertised" || e.DeliveredVersionID != "v-delivered" {
			t.Errorf("both ids should be recorded, got resolved=%q delivered=%q",
				e.ResolvedVersionID, e.DeliveredVersionID)
		}
	})

	t.Run("package carries no descriptor", func(t *testing.T) {
		root, lockPath := newRun(t)
		a := asset("1", "Asset", "v1", 500)
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write(bytes.Repeat([]byte("x"), 400))
		zw.Close()
		body := buf.Bytes()
		for len(body) < 500 {
			body = append(body, 0)
		}
		fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": body}}

		rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Results[0].Err != nil {
			t.Fatalf("a descriptor-less package must still be stored: %v", rep.Results[0].Err)
		}
		if !strings.Contains(rep.Results[0].Warning, "no store metadata") {
			t.Errorf("warning = %q, want it to say later checks fall back to size", rep.Results[0].Warning)
		}
		_, e, _ := rep.Lockfile.FindByAssetID("1")
		if e.DeliveredVersionID != "" {
			t.Errorf("deliveredVersionId = %q, want empty", e.DeliveredVersionID)
		}
	})

	t.Run("both a version mismatch and a size outside the window", func(t *testing.T) {
		root, lockPath := newRun(t)
		// 200 short of 4000: past the +-64 window, inside the floor's 500-byte allowance.
		a := asset("1", "Asset", "v-advertised", 4000)
		fs := &fakeStore{owned: []model.Asset{a},
			bodies: map[string][]byte{"1": pkg(t, "1", "v-delivered", 3800)}}

		rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := rep.Results[0].Warning
		// The size notice must not silence the version one: the lockfile now holds two
		// different ids and this line is the only thing that explains why.
		if !strings.Contains(got, "v-delivered") {
			t.Errorf("warning = %q, want the version mismatch kept alongside the size notice", got)
		}
		if !strings.Contains(got, "3800") {
			t.Errorf("warning = %q, want the size notice too", got)
		}
	})

	t.Run("outside the advisory window but above the floor", func(t *testing.T) {
		root, lockPath := newRun(t)
		// 200 bytes short of 4000: past the +-64 window, inside the floor's 500-byte
		// allowance (4000/8).
		a := asset("1", "Asset", "v1", 4000)
		fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 3800)}}

		rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Results[0].Err != nil {
			t.Fatalf("a body inside the floor must not fail: %v", rep.Results[0].Err)
		}
		if !strings.Contains(rep.Results[0].Warning, "3800") {
			t.Errorf("warning = %q, want it to report the received count", rep.Results[0].Warning)
		}
	})
}

// Retrying either of these wastes a backoff on something a second attempt cannot fix, and
// the waste is invisible: the run still ends the same way, just later.
func TestPermanentDownloadFailuresAreNotRetried(t *testing.T) {
	for name, sentinel := range map[string]error{
		"pulled asset":    store.ErrNotDownloadable,
		"expired session": store.ErrExpiredSession,
	} {
		t.Run(name, func(t *testing.T) {
			root, lockPath := newRun(t)
			a := asset("1", "Asset", "v1", 500)
			fs := &fakeStore{owned: []model.Asset{a}, fetchEr: map[string]error{"1": sentinel}}

			o := opts(root, allSelected(a))
			o.Retry = retryPolicyWithAttempts(3)

			rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(fs.fetched) != 1 {
				t.Errorf("fetched %d times, want exactly 1: %v cannot be fixed by trying again",
					len(fs.fetched), sentinel)
			}
			if rep.Results[0].Err == nil {
				t.Error("the failure was swallowed")
			}
		})
	}
}

// The counter-case to the two above, and the half that keeps them honest. A body that
// stopped delivering is the one download failure a second attempt is expected to fix — a
// fresh connection is the whole remedy — so marking ErrStalled permanent anywhere between
// stallGuard.Read and the pool's classification turns one transient silence into a failed
// asset, and on a 23 GB package that is the whole transfer thrown away.
func TestAStalledTransferIsRetriedRatherThanFailed(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)
	fs := &fakeStore{
		owned:    []model.Asset{a},
		bodies:   map[string][]byte{"1": pkg(t, "1", "v1", 500)},
		firstErr: fmt.Errorf("%w: no bytes for 2m0s", store.ErrStalled),
	}

	o := opts(root, allSelected(a))
	o.Retry = retryPolicyWithAttempts(2)

	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.fetched) != 2 {
		t.Errorf("fetched %d times, want 2: a stall is exactly what a fresh connection fixes", len(fs.fetched))
	}
	if rep.Results[0].Err != nil {
		t.Errorf("the retried asset still failed: %v", rep.Results[0].Err)
	}
	if rep.Failed() {
		t.Error("a stall that the retry cleared still failed the run")
	}
	if e := rep.Lockfile.Assets[a.Slug()]; !e.Tracked {
		t.Error("the asset was not recorded after the retry succeeded")
	}
}

// retryPolicyWithAttempts gives a test a real attempt budget without real sleeping.
func retryPolicyWithAttempts(n int) retry.Policy {
	return retry.Policy{Attempts: n, Base: time.Millisecond, Sleep: func(time.Duration) {}}
}

// The one input Run refuses before it touches the network. A malformed glob otherwise
// reaches path.Match once per owned asset, where the error is discarded and every asset
// silently fails to match — so a typo in --only reports "0 selected" against a full
// library rather than saying the pattern is wrong.
func TestABadOnlyPatternIsRefusedBeforeAnythingIsEnumerated(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)
	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}

	o := opts(root, allSelected(a))
	o.OnlyGlob = "[" // an unterminated character class

	_, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err == nil {
		t.Fatal("Run accepted a pattern path.Match cannot compile")
	}
	if !strings.Contains(err.Error(), "--only") {
		t.Errorf("error %q does not name the flag that was wrong", err)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("a bad pattern still reached the store: %v", fs.fetched)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Error("a bad pattern still wrote a lockfile")
	}
}

// The classification pass relocates and deletes whole packages before the pool starts, and
// those moves are recorded by the same per-asset write the downloads use. Left to the
// closing save, a run that adopts and fetches nothing rides entirely on one write: lose it
// to a held-open file, a full disk or a kill, and the library has moved while the lockfile
// still names the old path with a digest that no longer describes the bytes there — which
// the next run reads as Unchanged and carries forward, and nothing but --verify looks again.
//
// The first fetch is the observation point: the whole classification pass has run by then,
// so whatever it resolved must already be on disk.
func TestAdoptionsAndMovesAreRecordedBeforeTheFirstDownload(t *testing.T) {
	root, lockPath := newRun(t)
	adopted := asset("1", "Adopted", "v1", 500)
	moved := asset("2", "Renamed Since", "v1", 500)
	fetched := asset("3", "Fetched", "v1", 500)

	// Off the derived path, so adopting it is a relocation the lockfile has to learn.
	place(t, root, adopted.PublisherSlug(), "stale-slug-1", pkg(t, "1", "v1", 500))
	// Current at a path the old slug named, so it classifies Unchanged and then moves.
	oldRel := cache.RelPath(moved.PublisherSlug(), "old-name-2")
	p := place(t, root, moved.PublisherSlug(), "old-name-2", pkg(t, "2", "v1", 500))

	prior := lockfile.New()
	prior.Assets["old-name-2"] = lockfile.Entry{
		AssetID: "2", Name: "Old Name",
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: p.Size, SHA256: p.SHA256, CachePath: oldRel,
		},
	}

	var atFirstFetch lockfile.Lockfile
	var loadErr error
	fs := &fakeStore{
		owned:  []model.Asset{adopted, moved, fetched},
		bodies: map[string][]byte{"3": pkg(t, "3", "v1", 500)},
		beforeFetch: func(int) {
			atFirstFetch, loadErr = lockfile.Load(lockPath)
		},
	}

	rep, err := Run(context.Background(), fs, prior,
		lockPath, opts(root, allSelected(adopted, moved, fetched)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loadErr != nil {
		t.Fatalf("reading the lockfile mid-run: %v", loadErr)
	}
	for _, tc := range []struct {
		id, want string
		class    Class
	}{
		{"1", cache.RelPath(adopted.PublisherSlug(), adopted.Slug()), Adopted},
		{"2", cache.RelPath(moved.PublisherSlug(), moved.Slug()), Unchanged},
	} {
		_, e, ok := atFirstFetch.FindByAssetID(tc.id)
		if !ok || !e.Tracked {
			t.Errorf("asset %s (%v) was not in the lockfile when the first download began; "+
				"its bytes had already moved on disk", tc.id, tc.class)
			continue
		}
		if e.CachePath != tc.want {
			t.Errorf("asset %s recorded at %q mid-run, want %q", tc.id, e.CachePath, tc.want)
		}
	}
	// The classes above are what the assertions assume; a fixture that stopped producing
	// them would leave this test passing for the wrong reason.
	for _, r := range rep.Results {
		switch r.Asset.ID {
		case "1":
			if r.Class != Adopted {
				t.Fatalf("asset 1 classified %v, want Adopted", r.Class)
			}
		case "2":
			if r.Class != Unchanged {
				t.Fatalf("asset 2 classified %v, want Unchanged", r.Class)
			}
		}
	}
}

// countScans and countVerifies replace the two probes with counting wrappers for the
// duration of a test, restoring them afterwards.
func countScans(t *testing.T) *int {
	t.Helper()
	var n int
	prev := scanLibrary
	scanLibrary = func(ctx context.Context, root string) *cache.Index {
		n++
		return prev(ctx, root)
	}
	t.Cleanup(func() { scanLibrary = prev })
	return &n
}

func countVerifies(t *testing.T) *int {
	t.Helper()
	var n int
	prev := verifyDeep
	verifyDeep = func(ctx context.Context, root, rel, sha string) bool {
		n++
		return prev(ctx, root, rel, sha)
	}
	t.Cleanup(func() { verifyDeep = prev })
	return &n
}

// One walk of the library serves every adopt probe in a run. Probing per asset is
// quadratic exactly when adoption matters most — a lost lockfile makes every owned asset
// ask, over a library that already holds them all — and the outcome is identical either
// way, so only a count sees the difference.
func TestTheLibraryIsWalkedOncePerRunAtMost(t *testing.T) {
	root, lockPath := newRun(t)
	var owned []model.Asset
	for _, id := range []string{"1", "2", "3", "4"} {
		a := asset(id, "Asset "+id, "v1", 500)
		owned = append(owned, a)
		place(t, root, a.PublisherSlug(), "old-slug-"+id, pkg(t, id, "v1", 500))
	}
	scans := countScans(t)

	fs := &fakeStore{owned: owned}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(owned...)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range owned {
		if _, e, ok := rep.Lockfile.FindByAssetID(a.ID); !ok || !e.Tracked {
			t.Fatalf("asset %s was not adopted, so the count below proves nothing", a.ID)
		}
	}
	if *scans != 1 {
		t.Errorf("walked the library %d times for %d adoptions, want 1", *scans, len(owned))
	}
}

// The ordinary run is the one that must not pay for the walk: everything is current,
// nothing asks to adopt, and the scan is built on first use precisely so it never happens.
func TestARunThatAdoptsNothingNeverWalksTheLibrary(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)
	p := place(t, root, a.PublisherSlug(), a.Slug(), pkg(t, "1", "v1", 500))

	prior := lockfile.New()
	prior.Assets[a.Slug()] = lockfile.Entry{
		AssetID: "1", Name: a.Name,
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: p.Size, SHA256: p.SHA256,
			CachePath: cache.RelPath(a.PublisherSlug(), a.Slug()),
		},
	}
	scans := countScans(t)

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class != Unchanged {
		t.Fatalf("class = %v, want Unchanged; the count below proves nothing otherwise", rep.Results[0].Class)
	}
	if *scans != 0 {
		t.Errorf("a run where nothing needed adopting still walked the library %d time(s)", *scans)
	}
}

// Under --verify the cheap probe becomes a full re-hash, so a probe asked twice for one
// file is not a repeated lookup but a doubling of the cost of verifying the whole library.
// classify asks, and the excludeRel decision asks again.
func TestVerifyHashesEachFileOncePerRun(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)
	p := place(t, root, a.PublisherSlug(), a.Slug(), pkg(t, "1", "v1", 500))

	prior := lockfile.New()
	prior.Assets[a.Slug()] = lockfile.Entry{
		AssetID: "1", Name: a.Name,
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: p.Size,
			// Deliberately wrong, so the deep verify fails and the run goes on to ask the
			// adopt question — which is the second place the same probe is reached from.
			SHA256:    "0000000000000000000000000000000000000000000000000000000000000000",
			CachePath: cache.RelPath(a.PublisherSlug(), a.Slug()),
		},
	}
	hashes := countVerifies(t)

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}
	o := opts(root, allSelected(a))
	o.FullVerify = true
	if _, err := Run(context.Background(), fs, prior, lockPath, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if *hashes != 1 {
		t.Errorf("re-hashed one file %d times in a single run, want 1", *hashes)
	}
}

// The probes this wraps re-hash whole packages under --verify, so asking twice does not
// merely repeat work, it doubles the cost of verifying the whole library.
func TestMemoizeRunsTheProbeOnce(t *testing.T) {
	calls := 0
	probe := memoize(func() bool {
		calls++
		return true
	})
	for range 5 {
		if !probe() {
			t.Fatal("memoized probe changed its answer")
		}
	}
	if calls != 1 {
		t.Errorf("probe ran %d times, want 1", calls)
	}

	calls = 0
	falsey := memoize(func() bool {
		calls++
		return false
	})
	for range 3 {
		if falsey() {
			t.Fatal("memoized probe changed its answer")
		}
	}
	if calls != 1 {
		t.Errorf("a false result was recomputed %d times, want 1", calls)
	}
}

// A recorded file that fails verification is excluded from the adopt scan, so the good
// copy found elsewhere has to displace it — and cache.Relocate refuses an occupied
// destination, which makes clearing the damaged occupant first the whole of the work.
//
// The two spellings are one test because the second is the first with the entry written
// the way a hand-edit or another machine leaves it. The path comparison behind the
// removal is canonical, so "./pub/a/a.unitypackage" has to be recognised as the file
// "pub/a/a.unitypackage" names: compared raw, the damaged copy is left in place, Relocate
// refuses the occupied destination, and the run re-downloads gigabytes it already has.
func TestAGoodCopyDisplacesADamagedOneHoweverTheEntrySpellsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded func(derived string) string
	}{
		{"as derived", func(d string) string { return d }},
		{"non-canonically", func(d string) string { return "./" + d }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, lockPath := newRun(t)
			a := asset("1", "Quick Outline", "v1", 4000)
			derived := cache.RelPath(a.PublisherSlug(), a.Slug())

			// Right product and an intact descriptor, so the scan still sees a candidate;
			// only the size fails against what the entry records.
			damaged := place(t, root, a.PublisherSlug(), a.Slug(), pkg(t, "1", "v1", 3000))
			// A good copy elsewhere in the library, as a rename would leave one.
			stray := place(t, root, "elsewhere", "stray-1", pkg(t, "1", "v1", 4000))

			prior := lockfile.New()
			e := tracked("1", a.Name, "v1", damaged)
			e.SizeBytes = 4000 // does not match the damaged file, so verification fails
			e.CachePath = tc.recorded(derived)
			prior.Assets[a.Slug()] = e

			fs := &fakeStore{owned: []model.Asset{a}}
			rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(a)))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.Results[0].Class != Adopted || rep.Results[0].Err != nil {
				t.Fatalf("class = %v, err = %v; want Adopted with no error",
					rep.Results[0].Class, rep.Results[0].Err)
			}
			if len(fs.fetched) != 0 {
				t.Errorf("adoption fell through to a download of %v", fs.fetched)
			}
			if !cache.VerifyDeep(t.Context(), root, derived, stray.SHA256) {
				t.Error("the derived path does not hold the good copy's bytes")
			}
			got := rep.Lockfile.Assets[a.Slug()]
			if got.CachePath != derived || got.SHA256 != stray.SHA256 || got.SizeBytes != stray.Size {
				t.Errorf("lockfile records %+v, want the adopted copy at the derived path", got)
			}
		})
	}
}
func TestAdoptionRemovesTheEntrysOwnSupersededCopy(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "New Name", "v1", 4000)
	body := pkg(t, "1", "v1", 4000)

	atDerived := place(t, root, a.PublisherSlug(), a.Slug(), body)
	stale := place(t, root, a.PublisherSlug(), "old-name-1", body)
	// Truncated, so the recorded copy fails verification and adoption is what runs.
	if err := os.Truncate(filepath.Join(root, filepath.FromSlash(stale.RelPath)), stale.Size-100); err != nil {
		t.Fatal(err)
	}

	prior := lockfile.New()
	prior.Assets["old-name-1"] = lockfile.Entry{
		AssetID: "1", Name: "Old Name",
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: stale.Size, SHA256: stale.SHA256, CachePath: stale.RelPath,
		},
	}

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class != Adopted || rep.Results[0].Err != nil {
		t.Fatalf("class = %v, err = %v; want a clean Adopted", rep.Results[0].Class, rep.Results[0].Err)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("downloaded %v; the current build was already at the derived path", fs.fetched)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(stale.RelPath))); !os.IsNotExist(err) {
		t.Errorf("the superseded copy at %s survived adoption", stale.RelPath)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(atDerived.RelPath))); err != nil {
		t.Errorf("the adopted copy is gone: %v", err)
	}
}

// Two runs that find the same droppped assets must print them the same way; ranging a map
// does not.
func TestDroppedAssetsAreReportedInAStableOrder(t *testing.T) {
	root, lockPath := newRun(t)
	kept := asset("1", "Kept", "v1", 500)

	prior := lockfile.New()
	for _, e := range []struct{ id, name string }{
		{"7", "Zulu"}, {"8", "Alpha"}, {"9", "Mike"}, {"10", "Bravo"},
	} {
		prior.Assets[e.name] = lockfile.Entry{
			AssetID: e.id, Name: e.name,
			Resolution: lockfile.Resolution{Tracked: true},
		}
	}
	prior.Assets[kept.Slug()] = lockfile.Entry{
		AssetID: "1", Name: "Kept",
		Resolution: lockfile.Resolution{Tracked: false},
	}

	var first []string
	for run := range 6 {
		fs := &fakeStore{owned: []model.Asset{kept}}
		rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(kept)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range rep.Removed {
			names = append(names, e.Name)
		}
		if run == 0 {
			first = names
			if want := []string{"Alpha", "Bravo", "Mike", "Zulu"}; !slices.Equal(names, want) {
				t.Fatalf("order = %v, want %v", names, want)
			}
			continue
		}
		if !slices.Equal(names, first) {
			t.Fatalf("run %d reported %v, run 0 reported %v", run, names, first)
		}
	}
}

// The dry-run test proves the sweep is gated; nothing proved it happens. Deleting the
// SweepTemps call from Run left the whole suite green.
func TestARealRunSweepsAbandonedTemps(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)

	leaf := filepath.Join(root, "pub-one", "asset-1")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(leaf, ".unity-sync-dl-stale")
	if err := os.WriteFile(stale, bytes.Repeat([]byte("x"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	// Older than the run start, which opts() pins to 2023-11-14.
	old := time.Unix(1600000000, 0)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// One the sweep must spare: a concurrent run's transfer, still in flight.
	live := filepath.Join(leaf, ".unity-sync-dl-live")
	if err := os.WriteFile(live, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Swept != 1 {
		t.Errorf("Swept = %d, want 1", rep.Swept)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the abandoned temp survived the run")
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("the run swept a temp newer than its own start: %v", err)
	}
}

// A lockfile is committed, hand-editable and read on other machines, so a recorded
// cachePath can spell the derived one differently and still name the same file. Compared
// as raw strings, "./pub/a/a.unitypackage" reads as a second file: the run deletes it as a
// superseded copy moments after committing the download to it, then records a digest and
// size for a path with nothing on it. The next run calls that CacheMissing and re-fetches
// up to 23 GB.
func TestARecordedPathSpelledDifferentlyIsNotTreatedAsASecondFile(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Quick Outline", "v2", 500)
	derived := cache.RelPath(a.PublisherSlug(), a.Slug())

	old := pkg(t, "1", "v1", 500)
	p := place(t, root, a.PublisherSlug(), a.Slug(), old)

	prior := lockfile.New()
	prior.Assets[a.Slug()] = lockfile.Entry{
		AssetID: "1", Name: a.Name,
		Version: lockfile.Version{ID: "v1"},
		Resolution: lockfile.Resolution{
			Tracked:           true,
			ResolvedVersionID: "v1", DeliveredVersionID: "v1",
			SizeBytes: p.Size, SHA256: p.SHA256,
			CachePath: "./" + derived, // the same file, spelled the way a hand-edit might
		},
	}

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v2", 500)}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, derived)); err != nil {
		t.Fatalf("the run deleted the package it had just downloaded: %v", err)
	}
	e := rep.Lockfile.Assets[a.Slug()]
	if !e.Tracked || e.CachePath != derived {
		t.Fatalf("entry = tracked %v at %q, want tracked at %q", e.Tracked, e.CachePath, derived)
	}
	// The recorded digest has to describe what is actually on disk, which is the only
	// reason deleting the file matters rather than merely being wasteful.
	sha, size, err := cache.Hash(t.Context(), root, e.CachePath)
	if err != nil {
		t.Fatalf("hashing the recorded path: %v", err)
	}
	if sha != e.SHA256 || size != e.SizeBytes {
		t.Errorf("lockfile records %s/%d, disk holds %s/%d", e.SHA256, e.SizeBytes, sha, size)
	}
}

func TestEachDownloadIsPersistedBeforeTheNextOneStarts(t *testing.T) {
	root, lockPath := newRun(t)
	owned, bodies := manyAssets(t, 3)

	// Read from inside the fetch of each later asset: whatever came before it must already
	// be on disk, which is only true if the pool persists as it goes.
	var seenBefore []int
	fs := &fakeStore{owned: owned, bodies: bodies}
	fs.beforeFetch = func(n int) {
		lf, err := lockfile.Load(lockPath)
		if err != nil {
			t.Errorf("loading the lockfile mid-run: %v", err)
			return
		}
		tracked := 0
		for _, e := range lf.Assets {
			if e.Tracked {
				tracked++
			}
		}
		seenBefore = append(seenBefore, tracked)
	}

	o := opts(root, allSelected(owned...))
	o.Concurrency = 1
	if _, err := Run(context.Background(), fs, lockfile.New(), lockPath, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []int{0, 1, 2}
	if !slices.Equal(seenBefore, want) {
		t.Errorf("assets already persisted at each fetch = %v, want %v: progress is not being "+
			"written until the run ends, so a crash discards everything fetched so far",
			seenBefore, want)
	}
}

// classify tests the recorded version before it probes the disk, and the order is load
// bearing rather than incidental: an asset that is about to be replaced by a download must
// not be re-hashed first, and under --verify that probe reads the whole file. Swapping the
// two blocks leaves every other test green, because they all supply a cacheOK that agrees
// with the answer.
func TestAChangedAssetIsDecidedWithoutTouchingTheDisk(t *testing.T) {
	a := asset("1", "A", "v2", 500)
	prior := lockfile.Entry{
		Resolution: lockfile.Resolution{
			Tracked: true, ResolvedVersionID: "v1", CachePath: "p",
		},
	}
	probed := false
	cacheOK := func() bool {
		probed = true
		return false
	}
	if got := classify(a, prior, true, cacheOK, func() bool { return false }); got != Changed {
		t.Errorf("classify = %v, want Changed", got)
	}
	if probed {
		t.Error("classify probed the cache for an asset it had already decided was out of date")
	}
}

// A disabled asset answers 404, so nothing about it can be fixed by fetching. That makes
// it the one class where the run's answer to "do we have it?" is final, and the lockfile
// is not the only thing that knows: a deleted lockfile leaves the bytes on disk with
// nothing pointing at them. Reporting the asset as unavailable while it sits in the
// library tells the user to go and find a package they already have, and records the
// entry untracked so `list` stops counting it as mirrored.
func TestADelistedAssetAlreadyInTheLibraryIsAdoptedNotReported(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("115488", "Quick Outline", "v1", 500)
	a.State = model.StateDisabled

	rel := cache.RelPath(a.PublisherSlug(), a.Slug())
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, pkg(t, a.ID, "v1", 500), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("Results = %d, want 1", len(rep.Results))
	}
	if got := rep.Results[0].Class; got != Adopted {
		t.Errorf("class = %v, want adopted: the package is in the library", got)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("the run fetched %v for a disabled asset", fs.fetched)
	}
	_, e, ok := rep.Lockfile.FindByAssetID(a.ID)
	if !ok || !e.Tracked {
		t.Fatalf("entry tracked = %v, ok = %v; the mirrored copy is not recorded", e.Tracked, ok)
	}
	if e.SHA256 == "" || e.SizeBytes == 0 {
		t.Errorf("entry records no digest or size: %+v", e)
	}
	if rep.Failed() {
		t.Error("adopting a delisted asset made the run report failure")
	}
}

// The other half: a delisted asset with nothing on disk is still reported, and still does
// not fail the run. Without this, making the branch above adopt could quietly turn every
// delisted asset into a silent success.
func TestADelistedAssetWithNoCopyIsStillReported(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("115488", "Quick Outline", "v1", 500)
	a.State = model.StateDisabled

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.Results[0].Class; got != Undownloadable {
		t.Errorf("class = %v, want undownloadable", got)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("the run fetched %v for a disabled asset", fs.fetched)
	}
	// A pulled asset is permanent, not actionable: failing here would make every later
	// run fail forever over an asset the store will never serve again.
	if rep.Failed() {
		t.Error("a delisted asset made the run exit non-zero")
	}
}

// Adoption is the one route into the cache that skips the download guards, so when it
// cannot complete the asset has to fail rather than fall through to a fetch that would
// rename over the file in the way. Relocate refusing an occupied destination is the
// reachable trigger: a file the tool did not write already sits at the derived path.
func TestAnAdoptionThatCannotCompleteFailsItsAsset(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("115488", "Quick Outline", "v1", 500)

	// The copy to adopt, parked somewhere the current layout does not put it.
	stray := filepath.Join(root, "elsewhere", "quick-outline.unitypackage")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, pkg(t, a.ID, "v1", 500), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unrelated file already holding the destination.
	derived := filepath.Join(root, filepath.FromSlash(cache.RelPath(a.PublisherSlug(), a.Slug())))
	if err := os.MkdirAll(filepath.Dir(derived), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(derived, []byte("not this asset"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{a.ID: pkg(t, a.ID, "v1", 500)}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Err == nil {
		t.Error("a refused relocation resolved the asset anyway")
	}
	if !rep.Failed() {
		t.Error("a failed adoption left the run reporting success")
	}
	if len(fs.fetched) != 0 {
		t.Errorf("the run fell through to a download (%v); the occupied destination is "+
			"still holding a file no guard has seen", fs.fetched)
	}
	if got, _ := os.ReadFile(derived); string(got) != "not this asset" {
		t.Error("the file already at the destination was replaced")
	}
}

// Run's classification pass hashes, relocates and deletes whole packages, and under
// --verify re-reads the entire library. Without a ctx check it goes on doing all of that
// after the user has asked it to stop — and main's signal handler has already taken
// SIGINT's default action away, so the second and third Ctrl-C do nothing either.
//
// The observable is the relocation an Unchanged asset takes when its slug moved: that
// branch consults no scan, so it is the one piece of classification work a cancelled
// Scan cannot suppress on its own. A run that keeps going moves the files and reports
// nothing skipped.
func TestACancelledRunStopsClassifyingAndCountsWhatItSkipped(t *testing.T) {
	root, lockPath := newRun(t)

	var owned []model.Asset
	prior := lockfile.New()
	for i := range 20 {
		id := fmt.Sprint(i)
		renamed := asset(id, "New Name "+id, "v1", 500)
		owned = append(owned, renamed)
		// Mirrored under the old slug, so classification would relocate it to the one the
		// current name derives.
		p := place(t, root, renamed.PublisherSlug(), "old-name-"+id, pkg(t, id, "v1", 500))
		prior.Assets["old-name-"+id] = tracked(id, "Old Name "+id, "v1", p)
	}
	before := treeSnapshot(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fs := &fakeStore{owned: owned}
	rep, err := Run(ctx, fs, prior, lockPath, opts(root, allSelected(owned...)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := treeSnapshot(t, root); got != before {
		t.Errorf("a cancelled run relocated packages anyway:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if rep.NotAttempted != len(owned) {
		t.Errorf("NotAttempted = %d, want all %d selected assets counted as skipped",
			rep.NotAttempted, len(owned))
	}
	if !rep.Failed() {
		t.Error("Failed() = false; a run that skipped every asset must not exit 0")
	}
	if len(fs.fetched) != 0 {
		t.Errorf("fetched %v against a cancelled context", fs.fetched)
	}
	// The tail still runs, so every owned asset keeps its entry and its prior resolution
	// rather than being dropped by the run that was interrupted.
	if len(rep.Lockfile.Assets) != len(owned) {
		t.Errorf("lockfile holds %d entries, want all %d owned assets carried forward",
			len(rep.Lockfile.Assets), len(owned))
	}
	if e, ok := rep.Lockfile.Assets["old-name-0"]; !ok || !e.Tracked {
		t.Errorf("entry old-name-0 = %+v, want the prior resolution carried forward under its own key", e)
	}
}

// The floor's allowance is min(4096, advertised/8) and the comparison is strict, so the
// exact boundary is the only place a < that became a <= would show. advertised <= 0 turns
// the floor off, which is store-reachable: product.asset accepts an empty downloadSize as
// zero, and a guard that failed such an asset instead would fail it on every run.
func TestBelowFloorHoldsItsBoundaries(t *testing.T) {
	cases := []struct {
		name               string
		received, adverted int64
		want               bool
	}{
		{"exactly on the allowance", 2000 - 250, 2000, false},
		{"one byte under it", 2000 - 251, 2000, true},
		{"the 4096 cap applies to a large package", 1 << 20, 1<<20 + 4096, false},
		{"a byte under the capped allowance", 1<<20 - 1, 1<<20 + 4096, true},
		{"an exact transfer", 500, 500, false},
		{"no advertised size disables the floor", 1, 0, false},
		{"a negative advertised size disables it too", 1, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := belowFloor(tc.received, tc.adverted); got != tc.want {
				t.Errorf("belowFloor(%d, %d) = %v, want %v", tc.received, tc.adverted, got, tc.want)
			}
		})
	}
}
