package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/retry"
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

			p, err := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(good))
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Commit(); err != nil {
				t.Fatal(err)
			}
			prior := lockfile.New()
			prior.Assets[a.Slug()] = lockfile.Entry{
				AssetID: "1", Name: a.Name, Tracked: true,
				ResolvedVersionID: "v1", DeliveredVersionID: "v1",
				SizeBytes: p.Size, SHA256: p.SHA256, CachePath: p.RelPath,
				Version: lockfile.Version{ID: "v1"},
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

	old, err := cache.Store(root, renamed.PublisherSlug(), "old-name-1", bytes.NewReader(pkg(t, "1", "v1", 500)))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Commit(); err != nil {
		t.Fatal(err)
	}
	prior := lockfile.New()
	prior.Assets["old-name-1"] = lockfile.Entry{
		AssetID: "1", Name: "Old Name", Tracked: true,
		ResolvedVersionID: "v1", DeliveredVersionID: "v1",
		SizeBytes: old.Size, SHA256: old.SHA256, CachePath: old.RelPath,
		Version: lockfile.Version{ID: "v1"},
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

// retryPolicyWithAttempts gives a test a real attempt budget without real sleeping.
func retryPolicyWithAttempts(n int) retry.Policy {
	return retry.Policy{Attempts: n, Base: time.Millisecond, Sleep: func(time.Duration) {}}
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

// The damaged file sits at the derived path, so a good copy found elsewhere has nowhere to
// land unless the damaged one goes first. Getting this wrong is not a bad classification,
// it is a permanent one: nothing resolves, the prior entry carries forward, and every later
// run refuses in exactly the same way.
func TestAGoodCopyReplacesTheDamagedFileHoldingItsPath(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 4000)
	good := pkg(t, "1", "v1", 4000)

	derived, err := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if err := derived.Commit(); err != nil {
		t.Fatal(err)
	}
	stray, err := cache.Store(root, "somewhere", "else-1", bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if err := stray.Commit(); err != nil {
		t.Fatal(err)
	}

	// Truncating leaves the descriptor intact, so the scan still sees a candidate; only the
	// recorded size no longer matches, which is what fails verification.
	full := filepath.Join(root, filepath.FromSlash(derived.RelPath))
	if err := os.Truncate(full, derived.Size-100); err != nil {
		t.Fatal(err)
	}

	prior := lockfile.New()
	prior.Assets[a.Slug()] = lockfile.Entry{
		AssetID: "1", Name: a.Name, Tracked: true,
		ResolvedVersionID: "v1", DeliveredVersionID: "v1",
		SizeBytes: derived.Size, SHA256: derived.SHA256, CachePath: derived.RelPath,
		Version: lockfile.Version{ID: "v1"},
	}

	fs := &fakeStore{owned: []model.Asset{a}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Err != nil {
		t.Fatalf("adoption failed: %v", rep.Results[0].Err)
	}
	if rep.Results[0].Class != Adopted {
		t.Fatalf("class = %v, want Adopted", rep.Results[0].Class)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("downloaded %v; a good copy was already on disk", fs.fetched)
	}
	if got := cache.VerifyDeep(root, derived.RelPath, stray.SHA256); !got {
		t.Error("the derived path does not hold the good copy's bytes")
	}
	e, ok := rep.Lockfile.Assets[a.Slug()]
	if !ok || e.CachePath != derived.RelPath || e.SHA256 != stray.SHA256 {
		t.Errorf("lockfile records %+v, want the adopted copy at the derived path", e)
	}
}

// The candidate can already be at the derived path while the lockfile still points at the
// old one — a run killed between the commit and the incremental save leaves exactly that.
// Adoption then relocates nothing, so without an explicit removal the recorded copy is
// orphaned: unreferenced by the new lockfile and named by nothing in the summary.
func TestAdoptionRemovesTheEntrysOwnSupersededCopy(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "New Name", "v1", 4000)
	body := pkg(t, "1", "v1", 4000)

	atDerived, err := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := atDerived.Commit(); err != nil {
		t.Fatal(err)
	}
	stale, err := cache.Store(root, a.PublisherSlug(), "old-name-1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Commit(); err != nil {
		t.Fatal(err)
	}
	// Truncated, so the recorded copy fails verification and adoption is what runs.
	if err := os.Truncate(filepath.Join(root, filepath.FromSlash(stale.RelPath)), stale.Size-100); err != nil {
		t.Fatal(err)
	}

	prior := lockfile.New()
	prior.Assets["old-name-1"] = lockfile.Entry{
		AssetID: "1", Name: "Old Name", Tracked: true,
		ResolvedVersionID: "v1", DeliveredVersionID: "v1",
		SizeBytes: stale.Size, SHA256: stale.SHA256, CachePath: stale.RelPath,
		Version: lockfile.Version{ID: "v1"},
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
		prior.Assets[e.name] = lockfile.Entry{AssetID: e.id, Name: e.name, Tracked: true}
	}
	prior.Assets[kept.Slug()] = lockfile.Entry{AssetID: "1", Name: "Kept", Tracked: false}

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
