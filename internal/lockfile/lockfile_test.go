package lockfile_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/lockfile"
)

func sample() lockfile.Lockfile {
	lf := lockfile.New()
	lf.Assets["quick-outline-115488"] = lockfile.Entry{
		AssetID:            "115488",
		Name:               "Quick Outline",
		State:              "published",
		Publisher:          lockfile.Publisher{ID: "37073", Name: "Chris Nolet"},
		Version:            lockfile.Version{ID: "683375", Name: "1.1", PublishedDate: "2022-03-07T16:46:24Z"},
		AdvertisedSize:     33824,
		Tracked:            true,
		ResolvedVersionID:  "683375",
		DeliveredVersionID: "683375",
		SizeBytes:          33822,
		SHA256:             "abc123",
		CachePath:          "chris-nolet/quick-outline-115488/quick-outline-115488.unitypackage",
		DownloadedAt:       "2026-08-22T04:00:00Z",
		StoreFilename:      "Quick Outline.unitypackage",
	}
	lf.Assets["unowned-yet-999"] = lockfile.Entry{
		AssetID:        "999",
		Name:           "Never Downloaded",
		State:          "published",
		Version:        lockfile.Version{ID: "1", Name: "1.0"},
		AdvertisedSize: 4096,
		Tracked:        false,
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

func TestUntrackedEntriesOmitResolutionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	if err := lockfile.Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	// The untracked entry must not carry empty resolution keys; omitempty keeps the
	// committed file readable.
	chunk := string(raw)
	start := strings.Index(chunk, `"unowned-yet-999"`)
	if start < 0 {
		t.Fatal("untracked entry missing")
	}
	end := strings.Index(chunk[start:], "}")
	block := chunk[start : start+end]
	for _, field := range []string{"sha256", "cachePath", "downloadedAt", "resolvedVersionId"} {
		if strings.Contains(block, field) {
			t.Errorf("untracked entry carries %q", field)
		}
	}
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
