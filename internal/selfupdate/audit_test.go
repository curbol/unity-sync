package selfupdate_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/selfupdate"
)

// Failure models this package must keep pinned.

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

// The release-asset API answers with a 302 to a signed CDN URL, so the test server really
// redirects. This package is the one deliberate exception to the tree-wide redirect ban,
// and a client that inherited the Asset Store's CheckRedirect fails exactly here.
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

// The releases this reads are public, so an update has to work with no credential at all.
// Requiring one failed `unity-sync update` for every user who installed a binary and never
// set GITHUB_TOKEN or logged in with gh, which is the tool's own documented upgrade path.
func TestAnUpdateWorksWithNoGitHubCredential(t *testing.T) {
	var sawAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			asset, err := selfupdate.PlatformAssetFor(runtime.GOOS, runtime.GOARCH, "9.9.9")
			if err != nil {
				t.Errorf("PlatformAsset: %v", err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets":   []any{map[string]any{"name": asset, "url": "http://" + r.Host + "/asset"}},
			})
			return
		}
		w.Write(zipWithBinary(t, nativeBinary(t, "fresh binary")))
	}))
	defer srv.Close()

	// Driven through the whole update, not just the client, because the refusal that
	// broke this lived above both calls and a client-level test walks straight past it.
	dir := t.TempDir()
	target := filepath.Join(dir, "unity-sync")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := selfupdate.Update(context.Background(), selfupdate.New(srv.URL, ""), "0.1.0", "", target)
	if err != nil {
		t.Fatalf("update with no GitHub credential: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != nativeBinary(t, "fresh binary") {
		t.Fatalf("target holds %q, %v; want the downloaded binary", got, err)
	}
	for _, seen := range sawAuth {
		if seen != "" {
			t.Errorf("an empty token still sent Authorization: %q", seen)
		}
	}
}

// The archive names are a contract between this package and the release workflow that no
// compiler checks. Rename a label in release.yml and both CI jobs stay green, the release
// publishes, and every user on that platform gets "release has no asset ..." with no way
// to update — visible only after the tag exists.
func TestPlatformAssetNamesMatchWhatTheReleaseWorkflowPublishes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// The platforms array holds "goos/goarch/label" strings, one per line.
	re := regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)
	matches := re.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("no platform triples found in release.yml; this test can no longer see the contract")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		goos, goarch, label := m[1], m[2], m[3]
		seen[goos+"/"+goarch] = true
		got, err := selfupdate.PlatformAssetFor(goos, goarch, "1.2.3")
		if err != nil {
			t.Errorf("PlatformAsset(%s, %s): %v; the workflow builds a target the updater "+
				"cannot name", goos, goarch, err)
			continue
		}
		if want := "unity-sync-1.2.3-" + label + ".zip"; got != want {
			t.Errorf("PlatformAsset(%s, %s) = %q, want %q", goos, goarch, got, want)
		}
	}
	// And the platform this binary was built for has to be one the workflow publishes.
	if !seen[runtime.GOOS+"/"+runtime.GOARCH] && runtime.GOOS != "windows" {
		t.Errorf("release.yml publishes nothing for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// Windows will not let a running image be replaced, so the update renames it aside and
// takes its name. Neither the success path nor the recovery from a failed second rename
// ran on any platform: on Linux Replace returns before this function is reached, and the
// one Windows test only covers both renames failing.
func TestReplaceAsideRecoversTheBinaryWhenTheSwapFails(t *testing.T) {
	t.Run("success leaves the new binary in place and the old one beside it", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "unity-sync")
		if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(dir, ".unity-sync-update-x")
		if err := os.WriteFile(fresh, []byte("new binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := selfupdate.ReplaceAside(fresh, target, errors.New("in-place rename refused")); err != nil {
			t.Fatalf("replaceAside: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "new binary" {
			t.Fatalf("target holds %q, %v; want the new binary", got, err)
		}
	})

	t.Run("a failed swap puts the working binary back", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "unity-sync")
		if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		// A source that does not exist: the second rename cannot succeed.
		missing := filepath.Join(dir, ".unity-sync-update-gone")
		err := selfupdate.ReplaceAside(missing, target, errors.New("in-place rename refused"))
		if err == nil {
			t.Fatal("replaceAside reported success with nothing to install")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil || string(got) != "old binary" {
			t.Fatalf("target holds %q, %v; the working binary was not put back — this is the "+
				"one outcome an updater must never produce", got, readErr)
		}
		if _, err := os.Stat(target + ".old"); err == nil {
			t.Error("the aside copy was left behind after a successful restore")
		}
	})

	t.Run("Replace reaches the aside path when the image is locked", func(t *testing.T) {
		selfupdate.ForceImageLocked(t)
		dir := t.TempDir()
		target := filepath.Join(dir, "unity-sync")
		if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := selfupdate.Replace(target, []byte("new binary")); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "new binary" {
			t.Fatalf("target holds %q, %v; want the new binary", got, err)
		}
		fi, err := os.Stat(target)
		if err != nil || fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("the installed binary is not executable: mode %v, %v", fi.Mode().Perm(), err)
		}
	})
}

// The zip reader verifies each entry's CRC, so a corrupted asset is already caught. This
// is the other way an update goes wrong: a release that shipped something which is not a
// binary at all, under the right filename. Replace would rename it into place, chmod it
// executable and print "updated", leaving nothing runnable on PATH — the one outcome an
// updater must never produce. The check therefore runs before Replace, not after.
func TestAnAssetThatIsNotAnExecutableIsRefusedBeforeTheSwap(t *testing.T) {
	if _, known := selfupdate.ExecutableMagicFor(runtime.GOOS); !known {
		t.Skipf("no executable signature is checked on %s", runtime.GOOS)
	}
	for _, body := range []string{
		"<!doctype html><title>Not Found</title>", // an error page saved under the asset name
		"#!/bin/sh\necho wrong artifact\n",        // a script rather than a build
		"",                                        // an empty file the build step never wrote
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/releases/latest") {
				asset, err := selfupdate.PlatformAssetFor(runtime.GOOS, runtime.GOARCH, "9.9.9")
				if err != nil {
					t.Errorf("PlatformAsset: %v", err)
				}
				json.NewEncoder(w).Encode(map[string]any{
					"tag_name": "v9.9.9",
					"assets":   []any{map[string]any{"name": asset, "url": "http://" + r.Host + "/asset"}},
				})
				return
			}
			w.Write(zipWithBinary(t, body))
		}))

		target := filepath.Join(t.TempDir(), "unity-sync")
		if err := os.WriteFile(target, []byte(nativeBinary(t, "the working one")), 0o755); err != nil {
			t.Fatal(err)
		}
		err := selfupdate.Update(context.Background(), selfupdate.New(srv.URL, ""), "0.1.0", "", target)
		srv.Close()

		if err == nil {
			t.Errorf("an asset holding %q installed successfully", body)
			continue
		}
		if !strings.Contains(err.Error(), "executable") && !strings.Contains(err.Error(), "empty") {
			t.Errorf("asset %q was refused for some other reason than not being a binary: %v", body, err)
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("the working binary is gone after a refused update: %v", readErr)
		}
		if string(got) != nativeBinary(t, "the working one") {
			t.Errorf("the working binary was replaced by %q", got)
		}
	}
}
