package selfupdate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/selfupdate"
)

// Failure models this package must keep pinned.

// The private-repo asset API answers with a 302 to a signed CDN URL, so the test server
// really redirects. A client that inherited the Asset Store's redirect ban fails here.
// The one outcome an updater must never produce is no working binary. When the swap
// cannot happen, the binary already on PATH has to be exactly as it was, and the scratch
// file must not be left behind for the next run to trip over.
func TestAFailedReplaceLeavesTheWorkingBinaryAndNoScratch(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the target path: the rename cannot succeed onto it.
	target := filepath.Join(dir, "unity-sync")
	if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Where the running image cannot be replaced in place, Replace falls back to moving
	// the old binary to <target>.old and taking its name, which would succeed against a
	// directory. Occupying that slot as well leaves the fallback nowhere to go, so the
	// failure this pins is reachable on every platform rather than only on POSIX.
	if err := os.MkdirAll(filepath.Join(target+".old", "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := selfupdate.Replace(target, []byte("new binary")); err == nil {
		t.Fatal("Replace reported success against a target it could not take")
	}
	if _, err := os.Stat(filepath.Join(target, "occupied")); err != nil {
		t.Errorf("the existing target was disturbed by a failed replace: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".unity-sync-update-") {
			t.Errorf("a failed replace left the scratch file %q beside the binary", e.Name())
		}
	}
}

func TestDownloadFollowsTheAssetRedirect(t *testing.T) {
	archive := zipWithBinary(t, "#!/bin/true\n")
	var cdn *httptest.Server
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(archive)
	}))
	defer cdn.Close()

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			name, err := selfupdate.PlatformAsset("9.9.9")
			if err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": "unrelated.txt", "url": api.URL + "/assets/1"},
					{"name": name, "url": api.URL + "/assets/2"},
				},
			})
		case r.URL.Path == "/assets/2":
			if got := r.Header.Get("Accept"); got != "application/octet-stream" {
				t.Errorf("asset request Accept = %q", got)
			}
			http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	c := selfupdate.New(api.URL, "token")
	rel, err := c.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	binary, err := c.DownloadBinary(context.Background(), rel)
	if err != nil {
		t.Fatalf("DownloadBinary: %v — a client with the store's redirect ban fails exactly here", err)
	}
	if string(binary) != "#!/bin/true\n" {
		t.Errorf("binary = %q", binary)
	}
}
