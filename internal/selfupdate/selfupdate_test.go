package selfupdate_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/selfupdate"
)

func zipWithBinary(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("unity-sync")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(body))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The private-repo asset API answers with a 302 to a signed CDN URL, so the test server
// really redirects. A client that inherited the Asset Store's redirect ban fails here.
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
	rel, err := c.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	binary, err := c.DownloadBinary(rel)
	if err != nil {
		t.Fatalf("DownloadBinary: %v — a client with the store's redirect ban fails exactly here", err)
	}
	if string(binary) != "#!/bin/true\n" {
		t.Errorf("binary = %q", binary)
	}
}

func TestResolveSendsTheTokenAndCanPinAVersion(t *testing.T) {
	var seenAuth, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3"})
	}))
	defer srv.Close()

	c := selfupdate.New(srv.URL, "secret-token")
	if _, err := c.Resolve("1.2.3"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if seenAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q; the repo is private and an unauthenticated call cannot even list releases", seenAuth)
	}
	if !strings.HasSuffix(seenPath, "/releases/tags/v1.2.3") {
		t.Errorf("path = %q, want the pinned tag", seenPath)
	}
}

func TestMissingPlatformAssetIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v9.9.9",
			"assets":   []map[string]string{{"name": "unity-sync-9.9.9-somethingelse.zip", "url": "http://x"}},
		})
	}))
	defer srv.Close()

	c := selfupdate.New(srv.URL, "t")
	rel, err := c.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DownloadBinary(rel)
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Errorf("DownloadBinary = %v, want a complaint naming the missing asset", err)
	}
}

func TestReplaceIsAtomicAndExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unity-sync")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.Replace(target, []byte("new binary")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("content = %q", got)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want an executable bit", fi.Mode().Perm())
	}
	// The swap must not leave scratch files on PATH.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the binary", names)
	}
}

func TestPlatformAssetNamesAreVersioned(t *testing.T) {
	got, err := selfupdate.PlatformAsset("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "unity-sync-1.2.3-") || !strings.HasSuffix(got, ".zip") {
		t.Errorf("PlatformAsset = %q", got)
	}
}
