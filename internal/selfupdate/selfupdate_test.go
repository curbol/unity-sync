package selfupdate_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestResolveSendsTheTokenAndCanPinAVersion(t *testing.T) {
	var seenAuth, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3"})
	}))
	defer srv.Close()

	c := selfupdate.New(srv.URL, "secret-token")
	if _, err := c.Resolve(context.Background(), "1.2.3"); err != nil {
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
	rel, err := c.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DownloadBinary(context.Background(), rel)
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
	// Windows carries no mode bits; a binary is executable there by its .exe suffix.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
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
