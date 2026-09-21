package lockfile_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/lockfile"
)

func sample() lockfile.Lockfile {
	lf := lockfile.New()
	lf.Assets["quick-outline-115488"] = lockfile.Entry{
		AssetID:        "115488",
		Name:           "Quick Outline",
		State:          "published",
		Publisher:      lockfile.Publisher{ID: "37073", Name: "Chris Nolet"},
		Version:        lockfile.Version{ID: "683375", Name: "1.1", PublishedDate: "2022-03-07T16:46:24Z"},
		AdvertisedSize: 33824,
		Resolution: lockfile.Resolution{
			Tracked:            true,
			ResolvedVersionID:  "683375",
			DeliveredVersionID: "683375",
			SizeBytes:          33822,
			SHA256:             "abc123",
			CachePath:          "chris-nolet/quick-outline-115488/quick-outline-115488.unitypackage",
			DownloadedAt:       "2026-08-22T04:00:00Z",
			StoreFilename:      "Quick Outline.unitypackage",
		},
	}
	lf.Assets["unowned-yet-999"] = lockfile.Entry{
		AssetID:        "999",
		Name:           "Never Downloaded",
		State:          "published",
		Version:        lockfile.Version{ID: "1", Name: "1.0"},
		AdvertisedSize: 4096,
		Resolution:     lockfile.Resolution{Tracked: false},
	}
	return lf
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unity-sync.lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := lockfile.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := sample()
	for key, w := range want.Assets {
		g, ok := got.Assets[key]
		if !ok {
			t.Fatalf("entry %q vanished", key)
		}
		if g != w {
			t.Errorf("entry %q round-tripped as %+v, want %+v", key, g, w)
		}
	}
}

func TestMissingFileLoadsEmpty(t *testing.T) {
	lf, err := lockfile.Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load of a missing file = %v, want nil", err)
	}
	if len(lf.Assets) != 0 {
		t.Errorf("got %d entries, want none", len(lf.Assets))
	}
}

func TestSaveIsByteStableAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := lockfile.Save(a, sample()); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.Save(b, sample()); err != nil {
		t.Fatal(err)
	}
	ra, _ := os.ReadFile(a)
	rb, _ := os.ReadFile(b)
	if string(ra) != string(rb) {
		t.Error("two saves of the same content differ; the diff would be noise")
	}
}

// A run timestamp would dirty the committed file on every no-op run, which is the churn
// that buries the changelog the file exists to be.
func TestNoRunTimestampIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, forbidden := range []string{"generatedAt", "updatedAt", "syncedAt"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("lockfile carries a run timestamp %q", forbidden)
		}
	}
}

// sizeBytes must be what was received, never the advertised value: they differ by 0-16
// bytes, and conflating them makes cheap verify fail forever.
func TestAdvertisedAndReceivedSizesAreSeparateFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	lf, err := lockfile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e := lf.Assets["quick-outline-115488"]
	if e.AdvertisedSize == e.SizeBytes {
		t.Fatal("test fixture no longer distinguishes the two sizes")
	}
	if e.SizeBytes != 33822 || e.AdvertisedSize != 33824 {
		t.Errorf("sizes crossed over: sizeBytes=%d advertisedSize=%d", e.SizeBytes, e.AdvertisedSize)
	}
}

// The committed file must not grow empty resolution keys for the assets a run never
// resolved, which on a no-op run is most of it.
//
// This asserted nothing until it was rewritten. It sliced from the entry's key to the
// first "}", and Entry marshals in declaration order with Publisher carrying no
// omitempty — so the block it inspected ended at the close of the empty "publisher"
// object, before "version", and could not contain the keys it forbade whatever Save
// wrote. Dropping omitempty from all eight Resolution fields left the whole suite green.
// Decoding the entry and comparing key sets is what makes the assertion real. It says
// nothing about field *order*, because a map discards it; that half is
// TestEntryKeysKeepTheirPlaces.
func TestUntrackedEntriesOmitResolutionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Assets map[string]map[string]json.RawMessage `json:"assets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	untracked, ok := doc.Assets["unowned-yet-999"]
	if !ok {
		t.Fatal("untracked entry missing")
	}
	// Every resolution key, not the four that used to be listed: the point is that the
	// half is absent, so a ninth field added later is covered without editing this.
	for _, field := range []string{
		"resolvedVersionId", "deliveredVersionId", "sizeBytes",
		"sha256", "cachePath", "downloadedAt", "storeFilename",
	} {
		if _, present := untracked[field]; present {
			t.Errorf("untracked entry carries %q", field)
		}
	}
	// tracked has no omitempty and is meant to be written, so its absence would mean the
	// entry was not decoded at all and the loop above passed vacuously.
	if _, present := untracked["tracked"]; !present {
		t.Error(`untracked entry has no "tracked" key, so this test is reading the wrong thing`)
	}

	tracked, ok := doc.Assets["quick-outline-115488"]
	if !ok {
		t.Fatal("tracked entry missing")
	}
	// The other half of the contract: omitempty must not be swallowing a resolved
	// entry's fields either.
	for _, field := range []string{"resolvedVersionId", "sha256", "cachePath"} {
		if _, present := tracked[field]; !present {
			t.Errorf("tracked entry is missing %q", field)
		}
	}
}

// The byte placement of every key, which the Resolution embedding exists to hold still:
// encoding/json emits an embedded struct's fields at the embedded field's own index, so
// moving Resolution above the advertised half shifts every key in every entry.
//
// Nothing else notices. TestRoundTrip compares structs and TestSaveIsByteStableAcrossRuns
// compares two saves from the same binary, so both stay green while the first sync after
// such a change rewrites the whole committed file — a diff touching every line of a
// three-thousand-asset lockfile, which is the end of the "a month's diff reads like a
// changelog" property the file exists for.
func TestEntryKeysKeepTheirPlaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := keyOrder(raw, "quick-outline-115488")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"assetId", "name", "state", "publisher", "version", "advertisedSize",
		"tracked", "resolvedVersionId", "deliveredVersionId", "sizeBytes",
		"sha256", "cachePath", "downloadedAt", "storeFilename",
	}
	if len(got) != len(want) {
		t.Fatalf("entry has keys %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry keys are %v, want %v (the advertised half comes first and the "+
				"resolution half follows it, in the order Resolution declares)", got, want)
		}
	}
}

// keyOrder reads one entry's keys in the order they were written, which is the thing a
// map cannot answer.
func keyOrder(raw []byte, key string) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Into "assets", then into the entry, then collect its field names. Every Token call
	// below is a step down one level of a document this test just wrote.
	depth := 0
	var inEntry bool
	var keys []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", key, err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if inEntry && depth == 2 {
					return keys, nil
				}
			}
		case string:
			switch {
			case inEntry && depth == 3:
				keys = append(keys, t)
				if err := skipValue(dec); err != nil {
					return nil, err
				}
			case depth == 2 && t == key:
				inEntry = true
			}
		}
	}
}

// skipValue consumes the value after a key, so the next token is the following key rather
// than something nested inside it.
func skipValue(dec *json.Decoder) error {
	var v json.RawMessage
	return dec.Decode(&v)
}

// A rename changes the key by construction, so lookups that must survive one go by id.
func TestFindByAssetIDIgnoresTheKey(t *testing.T) {
	lf := sample()
	key, e, ok := lf.FindByAssetID("115488")
	if !ok {
		t.Fatal("FindByAssetID missed a present entry")
	}
	if key != "quick-outline-115488" || e.Name != "Quick Outline" {
		t.Errorf("FindByAssetID = %q, %+v", key, e)
	}
	if _, _, ok := lf.FindByAssetID("nope"); ok {
		t.Error("FindByAssetID invented an entry")
	}
}

func TestCorruptLockfileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := lockfile.Load(path); err == nil {
		t.Error("Load accepted a corrupt lockfile")
	}
}

// The lockfile is meant to be committed and read by other people and tools. Writing
// through a temp file and renaming would otherwise leave it owner-only.
func TestSaveDoesNotMakeTheLockfileOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
	path := filepath.Join(t.TempDir(), "unity-sync.lock.json")
	lf := lockfile.New()
	lf.Assets["a-1"] = lockfile.Entry{AssetID: "1", Name: "A"}

	if err := lockfile.Save(path, lf); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("a fresh lockfile is mode %04o, want 0644", got)
	}

	// An existing file keeps whatever mode the user gave it.
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.Save(path, lf); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o664 {
		t.Errorf("rewriting reset the mode to %04o, want the 0664 it had", got)
	}
}

// Every other failure in Save removes the temp; the rename used not to. On Windows a
// rename over an open destination fails outright — an editor holding the file, an
// on-access scanner — and Save runs once per download, so the orphans pile up in the
// directory the user commits.
func TestSaveLeavesNoTempBehindWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory where the file should go: the rename cannot succeed onto it.
	path := filepath.Join(dir, "unity-sync.lock.json")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.Save(path, sample()); err == nil {
		t.Fatal("Save reported success onto a destination it could not take")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unity-sync-lock-") {
			t.Errorf("a failed rename left the temp file %q beside the lockfile", e.Name())
		}
	}
}

// The lockfile is committed and a rename changes an entry's key, so a merge that keeps
// both sides of one leaves two entries for one product. FindByAssetID walks a map, so
// without a refusal the run picks between them at random — the same checkout classifies
// the asset differently from run to run, and build drops the entry it did not pick.
func TestTwoEntriesForOneAssetAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unity-sync.lock.json")
	body := `{"assets":{
	  "quick-outline-115488":{"assetId":"115488","name":"Quick Outline","tracked":true,
	    "resolvedVersionId":"v1","cachePath":"acme/quick-outline-115488/quick-outline-115488.unitypackage"},
	  "outline-115488":{"assetId":"115488","name":"Outline","tracked":true,
	    "resolvedVersionId":"v2","cachePath":"acme/outline-115488/outline-115488.unitypackage"}
	}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := lockfile.Load(path)
	if err == nil {
		t.Fatal("Load accepted two entries for one asset id")
	}
	for _, want := range []string{"115488", "quick-outline-115488", "outline-115488"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q does not name %q", err, want)
		}
	}
}

// The flush is what makes a run's per-asset progress survive a crash: Save runs once per
// resolved asset precisely so a kill at asset 90 of 100 keeps the 89, and bytes still in
// the page cache when the rename returns are exactly the record that loses.
//
// Nothing observable distinguishes a Save that skipped it on a machine that stays up, so
// deleting the call left every other assertion in this package green. Ordering is checked
// by reading the destination from inside the flush: before the rename it still holds the
// previous save.
func TestSaveFlushesBeforeItRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unity-sync.lock.json")
	first := lockfile.New()
	first.Assets["a-1"] = lockfile.Entry{AssetID: "1", Name: "A"}
	if err := lockfile.Save(path, first); err != nil {
		t.Fatal(err)
	}

	second := lockfile.New()
	second.Assets["b-2"] = lockfile.Entry{AssetID: "2", Name: "B"}

	var calls int
	var atFlush string
	restore := lockfile.StubSync(func(f *os.File) error {
		calls++
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading the destination during the flush: %v", err)
		}
		atFlush = string(raw)
		return f.Sync()
	})
	defer restore()

	if err := lockfile.Save(path, second); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("Save flushed %d times, want 1: without it the rename can outrun the bytes", calls)
	}
	if !strings.Contains(atFlush, `"assetId": "1"`) || strings.Contains(atFlush, `"assetId": "2"`) {
		t.Error("the destination already held the new content when the flush ran, so the " +
			"rename happened first and the flush no longer protects anything")
	}
}

// A flush that fails is a write that did not reach the disk, so it has to fail the save
// rather than be renamed into place, and it must not leave its temp in a directory the
// user commits.
func TestAFailedFlushLeavesNeitherANewLockfileNorATemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unity-sync.lock.json")
	first := lockfile.New()
	first.Assets["a-1"] = lockfile.Entry{AssetID: "1", Name: "A"}
	if err := lockfile.Save(path, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	restore := lockfile.StubSync(func(*os.File) error { return errors.New("disk went away") })
	defer restore()
	second := lockfile.New()
	second.Assets["b-2"] = lockfile.Entry{AssetID: "2", Name: "B"}
	if err := lockfile.Save(path, second); err == nil {
		t.Fatal("Save reported success after the flush failed")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a failed flush still replaced the committed lockfile")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "unity-sync.lock.json" {
			t.Errorf("a failed flush left %q beside the lockfile", e.Name())
		}
	}
}

// Every error path in Save unlinks its own temp, but a SIGKILL or a power loss between
// the create and the rename cannot — and Save runs once per resolved asset, so a large
// sync spends a lot of windows there. Nothing else would ever remove them, and they land
// in the directory the user commits.
func TestSweepTempsReclaimsWhatAKilledRunLeftBehind(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, ".unity-sync-lock-123456")
	live := filepath.Join(dir, ".unity-sync-lock-inflight")
	keep := filepath.Join(dir, "unity-sync.lock.json")
	for _, p := range []string{orphan, live, keep} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	if n := lockfile.SweepTemps(dir, time.Now().Add(-time.Hour)); n != 1 {
		t.Errorf("swept %d temps, want 1", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphaned temp survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("the sweep removed a temp newer than the cutoff, i.e. a concurrent run's write")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the sweep removed the lockfile itself")
	}
	// A directory that does not exist yet is the first-run case, not an error.
	if n := lockfile.SweepTemps(filepath.Join(dir, "nope"), time.Now()); n != 0 {
		t.Errorf("swept %d temps from a missing directory", n)
	}
}
