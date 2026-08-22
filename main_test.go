package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/config"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/store"
)

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := stdout
	stdout = buf
	t.Cleanup(func() { stdout = old })
	return buf
}

// isolate keeps a developer's real config, session and library out of the tests.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	for _, k := range []string{"UNITY_SYNC_CONFIG_DIR", "UNITY_SYNC_LIBRARY", "UNITY_SYNC_SESSION"} {
		os.Unsetenv(k)
	}
	wd := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
	return wd
}

func TestVersionAndHelpSucceed(t *testing.T) {
	isolate(t)
	out := capture(t)
	code, err := run([]string{"version"})
	if code != 0 || err != nil {
		t.Fatalf("version = %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "unity-sync") {
		t.Errorf("version printed %q", out)
	}
	if code, err := run([]string{"help"}); code != 0 || err != nil {
		t.Errorf("help = %d, %v", code, err)
	}
}

func TestUnknownSubcommandFails(t *testing.T) {
	isolate(t)
	code, err := run([]string{"frobnicate"})
	if code == 0 || err == nil {
		t.Fatalf("unknown subcommand = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error %q does not name the subcommand", err)
	}
}

// update is the one subcommand that takes a positional.
func TestUpdateAcceptsOneVersionAndRejectsTwo(t *testing.T) {
	isolate(t)
	code, err := run([]string{"update", "1.2.3", "4.5.6"})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("update with two versions = %d, %v", code, err)
	}
	// One argument gets past dispatch and fails later, on the dev-build guard.
	_, err = run([]string{"update", "1.2.3"})
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Errorf("update on a dev build = %v, want the dev-build guard", err)
	}
}

func TestCommandsThatNeedAManifestSayHowToMakeOne(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"status", "sync"} {
		code, err := run([]string{cmd})
		if code == 0 || err == nil {
			t.Fatalf("%s with no manifest = %d, %v; want a failure", cmd, code, err)
		}
		if !strings.Contains(err.Error(), "select") {
			t.Errorf("%s: error %q does not tell the user how to create one", cmd, err)
		}
	}
}

// list reads only the lockfile, so it must work with no session and no network.
func TestListNeedsNoSession(t *testing.T) {
	wd := isolate(t)
	out := capture(t)

	manifestPath := filepath.Join(wd, manifest.FileName)
	if err := os.WriteFile(manifestPath, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lf := lockfile.New()
	lf.Assets["quick-outline-115488"] = lockfile.Entry{
		AssetID: "115488", Name: "Quick Outline", Tracked: true,
		Version: lockfile.Version{ID: "683375", Name: "1.1"},
	}
	lf.Assets["owned-only-999"] = lockfile.Entry{
		AssetID: "999", Name: "Owned But Not Mirrored",
		Version: lockfile.Version{ID: "1", Name: "1.0"},
	}
	if err := lockfile.Save(manifest.LockPath(manifestPath), lf); err != nil {
		t.Fatal(err)
	}

	code, err := run([]string{"list"})
	if code != 0 || err != nil {
		t.Fatalf("list = %d, %v", code, err)
	}
	body := out.String()
	for _, want := range []string{"Quick Outline", "Owned But Not Mirrored", "2 owned, 1 mirrored"} {
		if !strings.Contains(body, want) {
			t.Errorf("list output is missing %q:\n%s", want, body)
		}
	}
}

// fakeStore drives the commands that otherwise need a live session, so their happy paths
// are covered rather than only their error paths.
type fakeStore struct {
	owned  []model.Asset
	bodies map[string][]byte
}

func (f *fakeStore) Enumerate(context.Context) ([]model.Asset, error) { return f.owned, nil }

func (f *fakeStore) Lookup(_ context.Context, id string) (model.Asset, bool, error) {
	for _, a := range f.owned {
		if a.ID == id {
			return a, true, nil
		}
	}
	return model.Asset{}, false, nil
}

func (f *fakeStore) Fetch(_ context.Context, id string) (*store.Download, error) {
	body, ok := f.bodies[id]
	if !ok {
		return nil, store.ErrNotDownloadable
	}
	return &store.Download{Body: io.NopCloser(bytes.NewReader(body)), Filename: id + ".unitypackage"}, nil
}

func testPackage(t *testing.T, productID, versionID string, size int) []byte {
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

func ownedAsset(id, name, versionID string, size int64) model.Asset {
	return model.Asset{
		ID: id, Name: name, State: model.StatePublished,
		Publisher:      model.Publisher{ID: "p1", Name: "Pub One"},
		Version:        model.Version{ID: versionID, Name: "1.0"},
		AdvertisedSize: size,
	}
}

func TestStatusThenSyncThroughTheCommandLayer(t *testing.T) {
	wd := isolate(t)
	out := capture(t)

	a := ownedAsset("115488", "Quick Outline", "683375", 500)
	manifestPath := filepath.Join(wd, manifest.FileName)
	if err := manifest.Save(manifestPath, manifest.Manifest{
		Assets: []manifest.Entry{{ID: a.ID, Name: a.Name, Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeStore{owned: []model.Asset{a}, bodies: map[string][]byte{a.ID: testPackage(t, a.ID, "683375", 500)}}
	cfg := config.Config{LibraryPath: filepath.Join(wd, "library"), Concurrency: 1}
	lockPath := manifest.LockPath(manifestPath)

	// status classifies without writing anything.
	code, err := syncOrStatus(context.Background(), fake, cfg, manifestPath, lockPath, "", false, true)
	if code != 0 || err != nil {
		t.Fatalf("status = %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "new") {
		t.Errorf("status did not classify the asset as new:\n%s", out)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("status wrote a lockfile")
	}

	// sync downloads it and records it.
	out.Reset()
	code, err = syncOrStatus(context.Background(), fake, cfg, manifestPath, lockPath, "", false, false)
	if code != 0 || err != nil {
		t.Fatalf("sync = %d, %v", code, err)
	}
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	_, e, ok := lf.FindByAssetID(a.ID)
	if !ok || !e.Tracked {
		t.Fatalf("sync did not record the asset: %+v", e)
	}
	pkgPath := filepath.Join(cfg.LibraryPath, "pub-one", "quick-outline-115488", "quick-outline-115488.unitypackage")
	if _, err := os.Stat(pkgPath); err != nil {
		t.Errorf("package not at its derived path: %v", err)
	}

	// and a second status is a no-op.
	out.Reset()
	code, err = syncOrStatus(context.Background(), fake, cfg, manifestPath, lockPath, "", false, true)
	if code != 0 || err != nil {
		t.Fatalf("second status = %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("second status did not report unchanged:\n%s", out)
	}
}

// select is the only command that writes the manifest, so its refusal to rewrite a
// curated file against an empty enumeration is checked at the command layer.
func TestSelectRefusesToEmptyACuratedManifest(t *testing.T) {
	wd := isolate(t)
	manifestPath := filepath.Join(wd, manifest.FileName)
	if err := manifest.Save(manifestPath, manifest.Manifest{
		Assets: []manifest.Entry{{ID: "115488", Name: "Quick Outline", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(manifestPath)

	err := selectAssets(context.Background(), &fakeStore{}, manifestPath, "127.0.0.1:0")
	if err == nil {
		t.Fatal("select rewrote the manifest against an empty enumeration")
	}
	after, _ := os.ReadFile(manifestPath)
	if string(before) != string(after) {
		t.Error("the refused select changed the manifest anyway")
	}
}
