package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
)

func asset(id, name string) model.Asset { return model.Asset{ID: id, Name: name} }

func TestDiscoverWalksUpFromANestedDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "src", "game", "assets")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, manifest.FileName)
	if err := os.WriteFile(want, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := manifest.Discover(nested)
	if !ok || got != want {
		t.Errorf("Discover = %q, %v; want %q", got, ok, want)
	}
}

func TestDiscoverReportsAbsence(t *testing.T) {
	if _, ok := manifest.Discover(t.TempDir()); ok {
		t.Error("Discover invented a manifest")
	}
}

func TestLockPathSitsBesideTheManifest(t *testing.T) {
	if got := manifest.LockPath("/p/unity-sync.toml"); got != "/p/unity-sync.lock.json" {
		t.Errorf("LockPath = %q", got)
	}
}

// Save re-encodes from the struct, so a key that silently decodes to nothing would be
// deleted from the user's committed file on the next write.
func TestUnknownKeysAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifest.FileName)
	os.WriteFile(path, []byte("variant_includes = [\"Unity_*\"]\n"), 0o644)
	if _, err := manifest.Load(path); err == nil {
		t.Error("Load accepted an unknown key it would later delete")
	}
}

func TestEntriesWithoutAnIdAreRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifest.FileName)
	os.WriteFile(path, []byte("[[asset]]\nname = \"No Id\"\nenabled = true\n"), 0o644)
	if _, err := manifest.Load(path); err == nil {
		t.Error("Load accepted an entry with no id, which nothing could match")
	}
}

func TestNewlyOwnedAssetsArriveDisabled(t *testing.T) {
	var m manifest.Manifest
	if _, err := m.Reconcile([]model.Asset{asset("1", "A"), asset("2", "B")}); err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Assets {
		if e.Enabled {
			t.Errorf("asset %s arrived enabled; buying one must never download it", e.ID)
		}
	}
}

// The manifest keys on id precisely so a rename cannot silently reset a selection.
func TestReconcilePreservesSelectionAcrossARename(t *testing.T) {
	m := manifest.Manifest{Assets: []manifest.Entry{
		{ID: "309177", Name: "RPG_Animations - One Hand Base", Enabled: true},
	}}
	if _, err := m.Reconcile([]model.Asset{asset("309177", "Ultimate One Hand Locomotion")}); err != nil {
		t.Fatal(err)
	}
	if len(m.Assets) != 1 {
		t.Fatalf("got %d entries, want 1", len(m.Assets))
	}
	if !m.Assets[0].Enabled {
		t.Error("a rename reset the selection")
	}
	if m.Assets[0].Name != "Ultimate One Hand Locomotion" {
		t.Errorf("Name = %q, want the refreshed one", m.Assets[0].Name)
	}
}

func TestReconcileReportsDropsRatherThanHidingThem(t *testing.T) {
	m := manifest.Manifest{Assets: []manifest.Entry{
		{ID: "1", Name: "Kept", Enabled: true},
		{ID: "2", Name: "Refunded", Enabled: true},
	}}
	dropped, err := m.Reconcile([]model.Asset{asset("1", "Kept")})
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].ID != "2" {
		t.Errorf("dropped = %+v, want the refunded entry", dropped)
	}
}

// A session with a different active org returns an empty or foreign owned set, and this
// is the one file a user curates by hand.
func TestReconcileRefusesToEmptyANonEmptyManifest(t *testing.T) {
	m := manifest.Manifest{Assets: []manifest.Entry{{ID: "1", Name: "A", Enabled: true}}}
	_, err := m.Reconcile(nil)
	var wouldEmpty *manifest.ErrWouldEmpty
	if !errors.As(err, &wouldEmpty) {
		t.Fatalf("Reconcile = %v, want ErrWouldEmpty", err)
	}
	if len(m.Assets) != 1 {
		t.Error("the refused reconcile mutated the manifest anyway")
	}
}

func TestEmptyOwnedSetIsFineForAnEmptyManifest(t *testing.T) {
	var m manifest.Manifest
	if _, err := m.Reconcile(nil); err != nil {
		t.Errorf("Reconcile on a first run with nothing owned = %v, want nil", err)
	}
}

func TestUnknownIDsAreSurfaced(t *testing.T) {
	m := manifest.Manifest{Assets: []manifest.Entry{
		{ID: "1", Name: "Owned", Enabled: true},
		{ID: "404", Name: "Typo", Enabled: true},
	}}
	unknown := m.UnknownIDs([]model.Asset{asset("1", "Owned")})
	if len(unknown) != 1 || unknown[0].ID != "404" {
		t.Errorf("UnknownIDs = %+v, want the entry the account does not own", unknown)
	}
}

func TestSaveLoadRoundTripSortsById(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifest.FileName)
	m := manifest.Manifest{Assets: []manifest.Entry{
		{ID: "222", Name: "B", Enabled: false},
		{ID: "111", Name: "A", Enabled: true},
	}}
	if err := manifest.Save(path, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Assets) != 2 || got.Assets[0].ID != "111" {
		t.Fatalf("round trip = %+v, want sorted by id", got.Assets)
	}
	if !got.Assets[0].Enabled || got.Assets[1].Enabled {
		t.Errorf("selection flags did not survive: %+v", got.Assets)
	}
	if ids := got.EnabledIDs(); !ids["111"] || ids["222"] {
		t.Errorf("EnabledIDs = %v", ids)
	}
}

func TestSaveDoesNotReorderTheCallersSlice(t *testing.T) {
	m := manifest.Manifest{Assets: []manifest.Entry{{ID: "222"}, {ID: "111"}}}
	if err := manifest.Save(filepath.Join(t.TempDir(), manifest.FileName), m); err != nil {
		t.Fatal(err)
	}
	if m.Assets[0].ID != "222" {
		t.Error("Save sorted the caller's slice in place")
	}
}

// The manifest is committed and hand-edited, so a save must not quietly strip group and
// other from it.
func TestSaveDoesNotMakeTheManifestOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), manifest.FileName)
	m := manifest.Manifest{Assets: []manifest.Entry{{ID: "1", Name: "A", Enabled: true}}}

	if err := manifest.Save(path, m); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("a fresh manifest is mode %04o, want 0644", got)
	}

	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(path, m); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o664 {
		t.Errorf("rewriting reset the mode to %04o, want the 0664 it had", got)
	}
}
