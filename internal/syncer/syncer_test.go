package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/store"
)

// ---- fakes -----------------------------------------------------------------

type fakeStore struct {
	owned   []model.Asset
	bodies  map[string][]byte
	fetchEr map[string]error
	lookups map[string]model.Asset

	inFlight atomic.Int32
	maxSeen  atomic.Int32
	hold     time.Duration
	mu       sync.Mutex
	fetched  []string
}

func (f *fakeStore) Enumerate(context.Context) ([]model.Asset, error) { return f.owned, nil }

func (f *fakeStore) Lookup(_ context.Context, id string) (model.Asset, bool, error) {
	a, ok := f.lookups[id]
	return a, ok, nil
}

func (f *fakeStore) Fetch(_ context.Context, id string) (*store.Download, error) {
	n := f.inFlight.Add(1)
	for {
		max := f.maxSeen.Load()
		if n <= max || f.maxSeen.CompareAndSwap(max, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	if f.hold > 0 {
		time.Sleep(f.hold)
	}
	f.mu.Lock()
	f.fetched = append(f.fetched, id)
	f.mu.Unlock()

	if err := f.fetchEr[id]; err != nil {
		return nil, err
	}
	body, ok := f.bodies[id]
	if !ok {
		return nil, store.ErrNotDownloadable
	}
	return &store.Download{Body: io.NopCloser(bytes.NewReader(body)), Filename: id + ".unitypackage"}, nil
}

// pkg builds a package carrying a descriptor, padded to size.
func pkg(t *testing.T, productID, versionID string, size int) []byte {
	t.Helper()
	d := []byte(`{"id":"` + productID + `","version_id":"` + versionID + `"}`)
	extra := []byte{'A', '$', 0, 0}
	binary.LittleEndian.PutUint16(extra[2:4], uint16(len(d)))
	extra = append(extra, d...)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Header.Extra = extra
	zw.Write(bytes.Repeat([]byte("x"), 32))
	zw.Close()
	out := buf.Bytes()
	for len(out) < size {
		out = append(out, 0)
	}
	return out
}

func asset(id, name string, versionID string, size int64) model.Asset {
	return model.Asset{
		ID: id, Name: name, State: model.StatePublished,
		Publisher:      model.Publisher{ID: "p1", Name: "Pub One"},
		Version:        model.Version{ID: versionID, Name: "1.0"},
		AdvertisedSize: size,
	}
}

func newRun(t *testing.T) (root, lockPath string) {
	t.Helper()
	root = t.TempDir()
	return root, filepath.Join(t.TempDir(), "unity-sync.lock.json")
}

func allSelected(assets ...model.Asset) map[string]bool {
	m := map[string]bool{}
	for _, a := range assets {
		m[a.ID] = true
	}
	return m
}

func opts(root string, sel map[string]bool) Options {
	return Options{
		LibraryRoot: root, Selected: sel, Concurrency: 2,
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// ---- classify ---------------------------------------------------------------

func TestClassifyCoversEveryClass(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	live := asset("1", "A", "v2", 1000)
	tracked := lockfile.Entry{Tracked: true, ResolvedVersionID: "v2", CachePath: "p"}

	cases := []struct {
		name      string
		a         model.Asset
		prior     lockfile.Entry
		hasPrior  bool
		cacheOK   func() bool
		adoptable func() bool
		want      Class
	}{
		{"unchanged", live, tracked, true, yes, no, Unchanged},
		{"new", live, lockfile.Entry{}, false, no, no, New},
		{"changed", live, lockfile.Entry{Tracked: true, ResolvedVersionID: "v1", CachePath: "p"}, true, yes, no, Changed},
		{"download-now", live, lockfile.Entry{Tracked: false}, true, no, no, DownloadNow},
		{"cache-missing", live, tracked, true, no, no, CacheMissing},
		{"adopted with no record", live, lockfile.Entry{}, false, no, yes, Adopted},
		{"adopted when a record exists but nothing was mirrored", live,
			lockfile.Entry{Tracked: false}, true, no, yes, Adopted},
		{"undownloadable", model.Asset{ID: "1", State: model.StateDisabled}, lockfile.Entry{}, false, no, no, Undownloadable},
		{"disabled but already mirrored stays usable",
			model.Asset{ID: "1", State: model.StateDisabled, Version: model.Version{ID: "v2"}},
			tracked, true, yes, no, Unchanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.a, tc.prior, tc.hasPrior, tc.cacheOK, tc.adoptable); got != tc.want {
				t.Errorf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- lockfile coverage and carry-forward -------------------------------------

// The lockfile is the record of what is owned, not of what happens to be mirrored, and
// the adopt design depends on every owned asset having an entry.
func TestEveryOwnedAssetGetsAnEntryEvenWhenOnlyOneIsSelected(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Enabled", "v1", 500)
	b := asset("2", "Not enabled", "v1", 500)
	fs := &fakeStore{owned: []model.Asset{a, b}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}

	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Lockfile.Assets) != 2 {
		t.Fatalf("lockfile has %d entries, want 2", len(rep.Lockfile.Assets))
	}
	_, other, _ := rep.Lockfile.FindByAssetID("2")
	if other.Tracked {
		t.Error("the unselected asset was recorded as mirrored")
	}
	if other.Name != "Not enabled" {
		t.Errorf("the unselected asset lost its advertised metadata: %+v", other)
	}
}

// The regression this guards is subtle: refreshing the advertised version beside a stale
// resolution would mark an unresolved asset as already current, forever.
func TestOnlyGlobPreservesOutOfScopeRecordsAndKeepsTheDiffKey(t *testing.T) {
	root, lockPath := newRun(t)
	inScope := asset("1", "In scope", "v2", 500)
	outScope := asset("2", "Out of scope", "v2", 500)

	prior := lockfile.New()
	prior.Assets["out-of-scope-2"] = lockfile.Entry{
		AssetID: "2", Name: "Out of scope", Tracked: true,
		ResolvedVersionID: "v1", DeliveredVersionID: "v1",
		SHA256: "old-sha", CachePath: "pub-one/out-of-scope-2/out-of-scope-2.unitypackage",
		SizeBytes: 400, Version: lockfile.Version{ID: "v1"},
	}

	fs := &fakeStore{owned: []model.Asset{inScope, outScope}, bodies: map[string][]byte{"1": pkg(t, "1", "v2", 500)}}
	o := opts(root, allSelected(inScope, outScope))
	o.OnlyGlob = "in-scope-1"

	rep, err := Run(context.Background(), fs, prior, lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, out, ok := rep.Lockfile.FindByAssetID("2")
	if !ok {
		t.Fatal("the out-of-scope entry vanished")
	}
	if out.SHA256 != "old-sha" || out.CachePath == "" {
		t.Errorf("out-of-scope resolution was not carried forward: %+v", out)
	}
	if out.Version.ID != "v2" {
		t.Errorf("advertised version = %q, want the refreshed v2", out.Version.ID)
	}
	if out.ResolvedVersionID != "v1" {
		t.Fatalf("resolvedVersionId = %q, want v1 carried verbatim; refreshing it would mark a "+
			"stale file as current forever", out.ResolvedVersionID)
	}
	// And it must still read as out of date next run.
	if classify(outScope, out, true, func() bool { return true }, func() bool { return false }) != Changed {
		t.Error("the carried-forward entry no longer classifies Changed")
	}
}

func TestRenamedAssetIsRecognisedByIdAndRekeyedOnce(t *testing.T) {
	root, lockPath := newRun(t)
	renamed := asset("1", "Brand New Name", "v1", 500)

	// Put a real file where the old slug says it is.
	body := pkg(t, "1", "v1", 500)
	p, err := cache.Store(root, "pub-one", "old-name-1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}

	prior := lockfile.New()
	prior.Assets["old-name-1"] = lockfile.Entry{
		AssetID: "1", Name: "Old Name", Tracked: true,
		ResolvedVersionID: "v1", DeliveredVersionID: "v1",
		SizeBytes: p.Size, SHA256: p.SHA256, CachePath: p.RelPath,
		Version: lockfile.Version{ID: "v1"},
	}

	fs := &fakeStore{owned: []model.Asset{renamed}}
	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(renamed)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fs.fetched) != 0 {
		t.Errorf("a rename triggered a download of %v", fs.fetched)
	}
	if rep.Results[0].Class != Unchanged {
		t.Errorf("class = %v, want Unchanged", rep.Results[0].Class)
	}
	if len(rep.Lockfile.Assets) != 1 {
		t.Fatalf("lockfile has %d entries, want 1 — a rename must re-key, not duplicate", len(rep.Lockfile.Assets))
	}
	if _, ok := rep.Lockfile.Assets["brand-new-name-1"]; !ok {
		t.Errorf("entry was not re-keyed: %v", keys(rep.Lockfile))
	}
	moved := filepath.Join(root, "pub-one", "brand-new-name-1", "brand-new-name-1.unitypackage")
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("the cached directory did not move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pub-one", "old-name-1")); !os.IsNotExist(err) {
		t.Error("the old directory survived, so quarry's pack facet would stay stale")
	}
}

func TestOwnershipDropIsReportedNotJustRemoved(t *testing.T) {
	root, lockPath := newRun(t)
	kept := asset("1", "Kept", "v1", 500)
	prior := lockfile.New()
	prior.Assets["kept-1"] = lockfile.Entry{AssetID: "1", Name: "Kept", Version: lockfile.Version{ID: "v1"}}
	prior.Assets["gone-2"] = lockfile.Entry{
		AssetID: "2", Name: "Refunded", Tracked: true,
		CachePath: "pub-one/gone-2/gone-2.unitypackage", SizeBytes: 900,
	}
	fs := &fakeStore{owned: []model.Asset{kept}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}

	rep, err := Run(context.Background(), fs, prior, lockPath, opts(root, allSelected(kept)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0].AssetID != "2" {
		t.Fatalf("Removed = %+v, want the no-longer-owned asset named", rep.Removed)
	}
	if _, _, ok := rep.Lockfile.FindByAssetID("2"); ok {
		t.Error("the dropped asset is still in the lockfile")
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

type failingEnumerate struct{ fakeStore }

func (f *failingEnumerate) Enumerate(context.Context) ([]model.Asset, error) {
	return nil, errors.New("session expired")
}

// ---- download guards ----------------------------------------------------------

func TestSemanticGuardsRejectAndDiscard(t *testing.T) {
	good := pkg(t, "1", "v1", 2000)

	cases := []struct {
		name   string
		body   []byte
		lookup *model.Asset // when set, the re-query reports this
		wantOK bool
	}{
		{name: "not gzip at all", body: []byte("<html>sign in</html>")},
		{name: "descriptor names another product", body: pkg(t, "999", "v1", 2000)},
		{name: "short body, re-query unchanged", body: pkg(t, "1", "v1", 100)},
		{
			name:   "short body, re-query shows a republish",
			body:   pkg(t, "1", "v1", 100),
			lookup: &model.Asset{ID: "1", Version: model.Version{ID: "v2"}, AdvertisedSize: 4000},
		},
		{name: "20 bytes short is a warning only", body: pkg(t, "1", "v1", 1980), wantOK: true},
		{name: "exact", body: good, wantOK: true},
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
			if tc.wantOK {
				if res.Err != nil {
					t.Fatalf("Run rejected an acceptable body: %v", res.Err)
				}
				return
			}
			if res.Err == nil {
				t.Fatal("Run accepted a body it should have refused")
			}
			// Nothing may survive at a real cache path.
			final := filepath.Join(root, "pub-one", "asset-1", "asset-1.unitypackage")
			if _, err := os.Stat(final); !os.IsNotExist(err) {
				t.Error("a rejected body was committed to the cache")
			}
			if leftovers := tempsUnder(t, root); leftovers != 0 {
				t.Errorf("%d temp files survived a rejected download", leftovers)
			}
		})
	}
}

func tempsUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && len(d.Name()) > 15 && d.Name()[:15] == ".unity-sync-dl-" {
			n++
		}
		return nil
	})
	return n
}

// ---- failure isolation and concurrency ----------------------------------------

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

func TestProgressSurvivesAMidRunFailure(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "First", "v1", 500)
	b := asset("2", "Breaks", "v1", 500)
	fs := &fakeStore{
		owned:   []model.Asset{a, b},
		bodies:  map[string][]byte{"1": pkg(t, "1", "v1", 500)},
		fetchEr: map[string]error{"2": errors.New("connection reset")},
	}
	o := opts(root, allSelected(a, b))
	o.Concurrency = 1

	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Retryable != 1 {
		t.Errorf("Retryable = %d, want 1", rep.Retryable)
	}
	saved, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, first, _ := saved.FindByAssetID("1")
	if !first.Tracked {
		t.Error("the asset fetched before the failure was not persisted")
	}
}

// ---- dry run -------------------------------------------------------------------

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

func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var out []byte
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel...)
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				out = append(out, []byte(fmt.Sprint(fi.Size()))...)
			}
		}
		out = append(out, '\n')
		return nil
	})
	return string(out)
}

// ---- manifest reporting ---------------------------------------------------------

func TestUnknownManifestIdsAreReported(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Owned", "v1", 500)
	o := opts(root, allSelected(a))
	o.Manifest = manifest.Manifest{Assets: []manifest.Entry{
		{ID: "1", Name: "Owned", Enabled: true},
		{ID: "404", Name: "Refunded or mistyped", Enabled: true},
	}}
	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 500)}}

	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Unknown) != 1 || rep.Unknown[0].ID != "404" {
		t.Errorf("Unknown = %+v, want the id the account does not own; silence is the failure "+
			"mode this report exists to prevent", rep.Unknown)
	}
}

// ---- adoption ---------------------------------------------------------------------

func TestAdoptionRecordsTheDiffKeyAndLeavesDownloadedAtEmpty(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 500)
	body := pkg(t, "1", "v1", 500)
	p, _ := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(body))
	p.Commit()

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
	_, e, _ := rep.Lockfile.FindByAssetID("1")
	if e.ResolvedVersionID != "v1" {
		t.Errorf("resolvedVersionId = %q; empty would make this asset re-download every run", e.ResolvedVersionID)
	}
	if e.DownloadedAt != "" {
		t.Errorf("downloadedAt = %q, want empty: the file was found, not fetched", e.DownloadedAt)
	}
	if e.SHA256 == "" {
		t.Error("an adopted entry has no digest, so --verify would prove nothing about it")
	}
}

func TestATruncatedFileIsNotAdopted(t *testing.T) {
	root, lockPath := newRun(t)
	a := asset("1", "Asset", "v1", 4000)
	// Descriptor intact, body far short of what the store advertises.
	p, _ := cache.Store(root, a.PublisherSlug(), a.Slug(), bytes.NewReader(pkg(t, "1", "v1", 200)))
	p.Commit()

	fs := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{"1": pkg(t, "1", "v1", 4000)}}
	rep, err := Run(context.Background(), fs, lockfile.New(), lockPath, opts(root, allSelected(a)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].Class == Adopted {
		t.Fatal("a truncated package was adopted; the download floor exists to stop exactly this")
	}
	if len(fs.fetched) != 1 {
		t.Errorf("fetched %v, want the asset re-downloaded", fs.fetched)
	}
}

func keys(lf lockfile.Lockfile) []string {
	out := make([]string, 0, len(lf.Assets))
	for k := range lf.Assets {
		out = append(out, k)
	}
	return out
}
