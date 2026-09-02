package manifest_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports 0666 for every writable file; there are no mode bits to keep")
	}
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

// An empty enumeration is not the only shape a wrong-org session takes. A session for a
// different organisation returns a legitimately different owned set, and one that overlaps
// the curated selection in nothing looks exactly like it — but Reconcile used to accept it,
// clear every selection, and hand the select page an enabled set that was already empty.
// The page's own would-empty refusal then had nothing to compare against and accepted the
// save that replaced the file.
func TestReconcileRefusesAnOwnedSetThatSharesNoSelection(t *testing.T) {
	curated := manifest.Manifest{Assets: []manifest.Entry{
		{ID: "1", Name: "Mine", Enabled: true},
		{ID: "2", Name: "Also mine", Enabled: true},
	}}
	foreign := []model.Asset{{ID: "900", Name: "Someone else's"}, {ID: "901", Name: "And another"}}

	m := curated
	_, err := m.Reconcile(foreign)
	var deselect *manifest.ErrWouldDeselectAll
	if !errors.As(err, &deselect) {
		t.Fatalf("Reconcile = %v, want ErrWouldDeselectAll", err)
	}
	if deselect.Enabled != 2 {
		t.Errorf("Enabled = %d, want 2", deselect.Enabled)
	}

	// Losing some selections is ordinary — a refund, a delisting — and must still go
	// through, with the surviving selection intact.
	m = curated
	dropped, err := m.Reconcile([]model.Asset{{ID: "1", Name: "Mine"}, {ID: "900", Name: "New"}})
	if err != nil {
		t.Fatalf("Reconcile on a partial overlap: %v", err)
	}
	if len(dropped) != 1 || dropped[0].ID != "2" {
		t.Errorf("dropped = %v, want just asset 2", dropped)
	}
	if !m.EnabledIDs()["1"] {
		t.Error("the surviving selection was cleared")
	}

	// And a manifest with nothing selected yet has nothing to protect: a first run whose
	// entries are all disabled must reconcile against anything.
	fresh := manifest.Manifest{Assets: []manifest.Entry{{ID: "1", Name: "Mine"}}}
	if _, err := fresh.Reconcile(foreign); err != nil {
		t.Errorf("Reconcile refused a manifest with no selections to lose: %v", err)
	}
}

// Same rule as the lockfile's: every other failure path removes the temp, and the rename
// is the one that runs on a destination another process can be holding open.
func TestSaveLeavesNoTempBehindWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, manifest.FileName)
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := manifest.Manifest{Assets: []manifest.Entry{{ID: "1", Name: "A", Enabled: true}}}
	if err := manifest.Save(path, m); err == nil {
		t.Fatal("Save reported success onto a destination it could not take")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unity-sync-manifest-") {
			t.Errorf("a failed rename left the temp file %q beside the manifest", e.Name())
		}
	}
}

// The example is what a user copies into unity-sync.toml, and Load refuses a key it
// cannot decode — so feeding the file in checks that its keys still match Entry, where
// reading it does not. The whole [[asset]] block is commented out in the example, so it
// is uncommented first: those are exactly the lines a user uncomments.
func TestTheExampleManifestStillParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "unity-sync.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Split(string(raw), "\n")
	for i, line := range out {
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if strings.HasPrefix(body, "[[") || settingLine.MatchString(body) {
			out[i] = body
		}
	}
	path := filepath.Join(t.TempDir(), "unity-sync.toml")
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("unity-sync.example.toml no longer parses as a manifest: %v", err)
	}
	if len(m.Assets) != 1 {
		t.Fatalf("the example's [[asset]] block decoded to %d entries, want 1", len(m.Assets))
	}
	// Every field the example names must have reached Entry, not merely decoded.
	if e := m.Assets[0]; e.ID == "" || e.Name == "" || !e.Enabled {
		t.Errorf("the example set a field Load did not carry through: %+v", e)
	}
}

// settingLine matches a whole commented-out setting and not the prose around it.
var settingLine = regexp.MustCompile(`^[a-z_]+ *= *(".*"|[0-9]+|true|false)$`)
