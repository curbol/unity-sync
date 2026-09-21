package selfupdate_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	binary, err := selfupdate.DownloadBinary(c, context.Background(), rel)
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
	var said bytes.Buffer
	err := selfupdate.Update(context.Background(), &said, selfupdate.New(srv.URL, ""), "0.1.0", "", target)
	if err != nil {
		t.Fatalf("update with no GitHub credential: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != nativeBinary(t, "fresh binary") {
		t.Fatalf("target holds %q, %v; want the downloaded binary", got, err)
	}
	// Written to the caller's writer rather than straight to os.Stdout, which is what lets
	// the command layer capture it the way it captures every other subcommand's output.
	if !strings.Contains(said.String(), "0.1.0 -> 9.9.9") {
		t.Errorf("the update said %q, which does not name the versions it moved between", said.String())
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
	// Cut to the bash array first, the way install_test.go does. Run over the whole file
	// the pattern would keep matching if the matrix moved somewhere else, as long as some
	// unrelated "a/b/c" string remained — so the test would pass against text that is no
	// longer the contract.
	_, rest, ok := strings.Cut(string(raw), "platforms=(")
	if !ok {
		t.Fatal("release.yml has no platforms=( ... ) list; this guard no longer reads what the workflow builds")
	}
	block, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatal("release.yml's platforms list is unterminated")
	}
	// The platforms array holds "goos/goarch/label" strings, one per line.
	re := regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)
	matches := re.FindAllStringSubmatch(block, -1)
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
		// The magic-byte check is what stops a release that shipped an error page from
		// being renamed over a working binary, and an unknown GOOS is let through by
		// design so an unlisted platform stays updatable. That fail-open is only safe
		// while every platform the release actually builds has a signature: adding one to
		// the workflow without one here turns the last guard before the rename into a
		// no-op, on that platform alone, invisibly.
		if _, known := selfupdate.ExecutableMagicFor(goos); !known {
			t.Errorf("release.yml builds %s but executableMagic has no signature for it, so "+
				"checkExecutable would accept anything there", goos)
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
		// runningImageIsLocked is consulted only inside the error branch of the direct
		// rename, and a plain writable file is renamed over successfully on Windows too —
		// so a file target never reaches replaceAside, and this subtest passed with the
		// whole aside dance deleted. A non-empty directory is a destination os.Rename
		// refuses everywhere while still being renameable itself, which is exactly the
		// shape Windows presents for a running image.
		if err := os.MkdirAll(filepath.Join(target, "occupied"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := selfupdate.Replace(target, []byte("new binary")); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "new binary" {
			t.Fatalf("target holds %q, %v; want the new binary", got, err)
		}
		// The bytes land on every platform; the mode only means something where there
		// are mode bits. Windows reports 0666 for every writable file.
		if runtime.GOOS != "windows" {
			fi, err := os.Stat(target)
			if err != nil {
				t.Fatalf("stat %s: %v", target, err)
			}
			if fi.Mode().Perm()&0o111 == 0 {
				t.Errorf("the installed binary is not executable: mode %v", fi.Mode().Perm())
			}
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
		err := selfupdate.Update(context.Background(), io.Discard, selfupdate.New(srv.URL, ""), "0.1.0", "", target)
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

// The asset URL is a field in a response, and get attaches the user's GitHub token to
// whatever URL it is given. Go strips Authorization on a redirect to another host, which
// covers the hop to the signed CDN — but nothing covers the first request, so the host
// has to be checked before it is made.
func TestAnAssetURLOffTheAPIHostIsRefusedBeforeTheTokenIsSent(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the client sent %s %s with Authorization %q",
			r.Method, r.URL, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	assetName, err := selfupdate.PlatformAsset("9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[{"name":%q,"url":%q}]}`,
			assetName, elsewhere.URL+"/evil.zip")
	}))
	defer api.Close()

	c := selfupdate.New(api.URL, "super-secret")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selfupdate.DownloadBinary(c, context.Background(), rel); err == nil {
		t.Fatal("DownloadBinary followed an asset URL onto another host")
	}
}

// docs/design.md and DownloadBinary both lean on "Go strips the Authorization header on a
// redirect to another host" to cover the hop to the signed CDN; sameHost only covers the
// first request. Nothing asserted the strip, and the redirect test structurally could not:
// shouldCopyHeaderOnRedirect compares hostnames with the port removed, and two httptest
// servers are both on 127.0.0.1, so Go forwards the header there.
//
// The regression this pins is moving the auth out of get() and into a Transport wrapper,
// which is the natural shape for adding a retry or a rate-limit backoff. A RoundTripper
// runs per hop, so the user's token would then reach the CDN on every update.
func TestTheGitHubTokenNeverReachesTheRedirectTarget(t *testing.T) {
	archive := zipWithBinary(t, "#!/bin/true\n")
	var cdnAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(archive)
	}))
	defer cdn.Close()

	var apiBase string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			name, err := selfupdate.PlatformAsset("9.9.9")
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Errorf("the API host did not get the token: %q", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets":   []map[string]string{{"name": name, "url": apiBase + "/assets/2"}},
			})
		case r.URL.Path == "/assets/2":
			if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Errorf("the asset request on the API host did not get the token: %q", got)
			}
			// A different hostname, which is what makes the strip observable: both
			// servers on 127.0.0.1 are the same host to Go and the header is forwarded.
			http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	// Same address, spelled by a name the CDN does not share.
	apiBase = strings.Replace(api.URL, "127.0.0.1", "localhost", 1)
	if apiBase == api.URL {
		t.Skip("httptest did not bind 127.0.0.1; this needs two spellings of one address")
	}

	c := selfupdate.New(apiBase, "secret-token")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := selfupdate.DownloadBinary(c, context.Background(), rel); err != nil {
		t.Fatalf("DownloadBinary: %v", err)
	}
	if cdnAuth != "" {
		t.Errorf("the GitHub token reached the redirect target: %q", cdnAuth)
	}
}

// Both CLAUDE.md and docs/design.md rest the magic-byte argument on "the zip reader has
// already verified each entry's CRC, so what this catches is the *other* failure". That is
// true today only because io.ReadAll happens to drain the entry to EOF, which is where
// archive/zip's checksumReader compares the digest. It is not a property anything asserts.
//
// Bounding the read by the declared size instead, switching to OpenRaw to avoid
// decompressing twice, or breaking early at the ceiling are each a plausible edit, each
// compiles, and each silently removes CRC verification — after which a bit-flipped release
// asset whose first bytes are still a valid signature is renamed over the working binary.
func TestAnAssetWhoseCRCDoesNotMatchIsRefusedBeforeTheSwap(t *testing.T) {
	if _, known := selfupdate.ExecutableMagicFor(runtime.GOOS); !known {
		t.Skipf("no executable signature is checked on %s", runtime.GOOS)
	}
	// Stored rather than deflated, so a byte can be flipped in the archive without
	// disturbing anything but the payload and its checksum. The flip lands past the magic
	// so the signature check still passes and the CRC is the only thing left to catch it.
	good := nativeBinary(t, "the new build")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "unity-sync", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(good)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	at := bytes.Index(archive, []byte("the new build"))
	if at < 0 {
		t.Fatal("stored entry not found in the archive; the payload was compressed after all")
	}
	archive[at] ^= 0xff

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
		w.Write(archive)
	}))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "unity-sync")
	working := nativeBinary(t, "the working one")
	if err := os.WriteFile(target, []byte(working), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := selfupdate.Update(context.Background(), io.Discard, selfupdate.New(srv.URL, ""), "0.1.0", "", target); err == nil {
		t.Error("an asset whose CRC does not match installed successfully")
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("the working binary is gone after a refused update: %v", readErr)
	}
	if string(got) != working {
		t.Errorf("the working binary was replaced by corrupt bytes: %q", got)
	}
}

// releaseServer serves a release and its asset, letting a test answer a request itself
// first. The handler returns true when it has written the whole response.
func releaseServer(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if intercept != nil && intercept(w, r) {
			return
		}
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
	t.Cleanup(srv.Close)
	return srv
}

// runUpdate drives a whole update against srv with the given credential and returns what
// it said. End to end rather than at the client, because the fallback being tested sits
// under both API calls and a client-level test walks past the second one.
func runUpdate(t *testing.T, srv *httptest.Server, token string) (target string, err error) {
	t.Helper()
	target = filepath.Join(t.TempDir(), "unity-sync")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	err = selfupdate.Update(context.Background(), &said,
		selfupdate.New(srv.URL, token), "0.1.0", "", target)
	return target, err
}

// The token is opportunistic: everything this package reads is public, and a run with no
// credential at all already works. A credential the API rejects therefore must not be
// worse than none — an expired PAT left in GITHUB_TOKEN, or a fine-grained one never
// granted public-repository read, would otherwise kill the documented upgrade path with
// "status 401: Bad credentials", or with a 404 that reads as "no release exists".
func TestACredentialGitHubRejectsFallsBackToAnAnonymousRequest(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var anonymous int
			srv := releaseServer(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.Header.Get("Authorization") != "" {
					w.WriteHeader(status)
					return true
				}
				anonymous++
				return false
			})
			target, err := runUpdate(t, srv, "stale-token")
			if err != nil {
				t.Fatalf("update failed with a token the API rejected: %v", err)
			}
			if anonymous == 0 {
				t.Error("no anonymous request was made, so the rejected token was never retried without")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != nativeBinary(t, "fresh binary") {
				t.Errorf("target holds %q, %v; want the downloaded binary", got, err)
			}
		})
	}
}

// A failure that is not about the credential must still be reported. Retrying anonymously
// and reporting the second error would replace a real diagnosis with a worse one.
func TestAFailureThatIsNotTheCredentialIsReportedAsItself(t *testing.T) {
	srv := releaseServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	if _, err := runUpdate(t, srv, "good-token"); err == nil {
		t.Fatal("a 500 from the release API was treated as success")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not report the status the API actually returned", err)
	}
}
