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
	"slices"
	"strings"
	"testing"
	"time"

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
	srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
		return false
	})

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
		// The directory, not just the target. The aside copy is removed once the swap has
		// succeeded, and nothing asserted it: dropping that removal leaves a full copy of
		// every superseded binary beside the install, on every POSIX-reachable run of this
		// path, and stayed green. The failing subtest below checks it only after a restore.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if len(names) != 1 || names[0] != "unity-sync" {
			t.Errorf("the directory holds %v, want just the installed binary; a leftover "+
				".old is a full copy of the previous release left behind on every update", names)
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
		srv := releaseServer(t, zipWithBinary(t, body), nil)

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

	srv := releaseServer(t, archive, nil)

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
// releaseServer answers the release API and serves archive as the platform asset. A nil
// archive serves a zip holding a plausible binary, which is what every test that is about
// something other than the bytes wants; intercept may answer a request itself and report
// true to stop there.
//
// It takes the archive because the release JSON — the tag, the asset name resolved through
// PlatformAssetFor, the asset URL pointed back at this server — is the same twenty lines
// in every test that needs one, and the only thing any of them varies is what comes back
// from /asset.
func releaseServer(t *testing.T, archive []byte, intercept func(http.ResponseWriter, *http.Request) bool) *httptest.Server {
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
		if archive == nil {
			archive = zipWithBinary(t, nativeBinary(t, "fresh binary"))
		}
		w.Write(archive)
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
			srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
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
	srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	if _, err := runUpdate(t, srv, "good-token"); err == nil {
		t.Fatal("a 500 from the release API was treated as success")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not report the status the API actually returned", err)
	}
}

// 401, 403 and 404 are each worth one anonymous retry, but only one of the three ever
// means "this credential" — so the marker that decides whether to retry must never decide
// what to report. `unity-sync update 0.2.9` for a version that was never tagged 404s, and
// answering "github rejected the credential" sends a user who has none looking for one,
// with the actual cause buried behind a claim about a thing they do not have.
func TestAStatusIsReportedAsItselfRatherThanAsARejectedCredential(t *testing.T) {
	for _, token := range []string{"", "a-token"} {
		name := "no token"
		if token != "" {
			name = "token that changes nothing"
		}
		t.Run(name, func(t *testing.T) {
			srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
				// 404 whether or not a credential is sent, which is what a version that
				// was never tagged looks like.
				w.WriteHeader(http.StatusNotFound)
				return true
			})
			_, err := runUpdate(t, srv, token)
			if err == nil {
				t.Fatal("a 404 from the release API was treated as success")
			}
			if !strings.Contains(err.Error(), "404") {
				t.Errorf("error %q does not report the status the API returned", err)
			}
			if strings.Contains(err.Error(), "rejected the credential") {
				t.Errorf("error %q blames a credential for a failure that survives removing it", err)
			}
		})
	}
}

// gh answers for whichever host is logged in unless one is named, so `gh auth token` alone
// sends a user authenticated only against their company's GitHub Enterprise that token to
// api.github.com. Nothing goes red when it regresses: the request 401s, the anonymous
// fallback rescues it, and the update succeeds — the safety net is what hides the leak, so
// the argv is the only thing that can catch it.
func TestTheGhFallbackAsksForGitHubComByName(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The stub is a /bin/sh script, and Windows resolves an executable by extension.
		t.Skip("the gh stub is a shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nprintf 'a-token\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Both spellings cleared, or the environment short-circuits the fallback entirely.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", dir)

	if got := selfupdate.Token(context.Background()); got != "a-token" {
		t.Fatalf("Token = %q, want the stub's output; the gh fallback was not reached", got)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("gh was not run: %v", err)
	}
	if !strings.Contains(string(argv), "--hostname github.com") {
		t.Errorf("gh was called as %q, without naming the host; an enterprise-only login "+
			"would have its token sent to api.github.com", strings.TrimSpace(string(argv)))
	}
}

// The environment wins over gh, and an absent credential is an empty string rather than a
// failure: everything this package reads is public.
func TestTokenPrefersTheEnvironmentAndToleratesNeither(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	t.Setenv("GH_TOKEN", "from-gh-token")
	t.Setenv("GITHUB_TOKEN", "from-github-token")
	if got := selfupdate.Token(context.Background()); got != "from-github-token" {
		t.Errorf("Token = %q, want GITHUB_TOKEN to win", got)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if got := selfupdate.Token(context.Background()); got != "from-gh-token" {
		t.Errorf("Token = %q, want GH_TOKEN when GITHUB_TOKEN is unset", got)
	}
	t.Setenv("GH_TOKEN", "")
	if got := selfupdate.Token(context.Background()); got != "" {
		t.Errorf("Token = %q, want empty when nothing supplies one", got)
	}
}

// The credential is only a rate-limit optimisation, so an update that is going to refuse
// itself must not read one. Before this, Run built the client — and therefore called
// `gh auth token` — ahead of the dev-build refusal, so `unity-sync update` on a dev build
// spawned gh and read the user's real credential for nothing. main_test drives exactly
// that path, so every `go test ./...` on a machine with no token in the environment did
// the same, against a suite whose contract is that it needs no session at all.
//
// Nothing about the returned error says whether a lookup happened, which is why the hook
// exists: reordering the two back leaves every other assertion here green.
func TestARefusedUpdateNeverReadsACredential(t *testing.T) {
	var lookups int
	selfupdate.StubTokenLookup(t, func(context.Context) string {
		lookups++
		return "should-never-be-asked-for"
	})
	err := selfupdate.Run(context.Background(), io.Discard, "dev", "")
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("Run on a dev build = %v, want the dev-build refusal", err)
	}
	if lookups != 0 {
		t.Errorf("a refused update consulted the credential %d time(s); the refusal must "+
			"come before the lookup, or `go test` reads the developer's real token", lookups)
	}
}

// Both ceilings exist so an artifact that is not one of the published zips is an error
// naming the size rather than an update the kernel kills. Neither had a test, because
// reaching them for real means moving 256 MB — and each is one plausible edit from
// disappearing: bounding the entry read by its declared size, or dropping the outer limit
// on the grounds that the zip reader bounds the archive anyway (it bounds the central
// directory, and reads nothing else for free).
func TestAnOversizedArchiveIsRefusedRatherThanBuffered(t *testing.T) {
	good := zipWithBinary(t, nativeBinary(t, "fresh binary"))
	selfupdate.LowerArchiveCeiling(t, int64(len(good)/2))

	srv := releaseServer(t, good, nil)
	c := selfupdate.New(srv.URL, "")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = selfupdate.DownloadBinary(c, context.Background(), rel)
	if err == nil {
		t.Fatal("an asset over the ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want an error naming the size limit", err)
	}
}

// checkExecutable resolves against runtime.GOOS, so on any one run only that platform's
// signatures are exercised: the darwin 32-bit and fat-binary arms and the Windows MZ are
// dead code everywhere but the leg that runs them, and the cross matrix covers a single
// Mach-O flavour. A signature dropped from a platform the release builds turns the last
// guard before the rename into a no-op there, and every other leg stays green.
func TestEverySignatureInTheTableIsAcceptedAndOthersAreNot(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		magics, ok := selfupdate.ExecutableMagicFor(goos)
		if !ok {
			t.Errorf("%s has no signature, though the release builds for it", goos)
			continue
		}
		if len(magics) == 0 {
			t.Errorf("%s has an empty signature list, which accepts nothing", goos)
		}
		for _, m := range magics {
			body := append(append([]byte{}, m...), []byte("rest of the binary")...)
			if err := selfupdate.CheckExecutableFor(goos, body); err != nil {
				t.Errorf("%s rejected its own signature %x: %v", goos, m, err)
			}
		}
		// The complement, or a table that accepted everything would pass above. An HTML
		// error page under the right asset name is the failure this exists to catch.
		for _, body := range [][]byte{
			[]byte("<!DOCTYPE html><html>signed out</html>"),
			[]byte("#!/bin/sh\necho nope\n"),
			{},
		} {
			if err := selfupdate.CheckExecutableFor(goos, body); err == nil {
				t.Errorf("%s accepted %q as a native binary", goos, body)
			}
		}
	}

	// An unknown GOOS is let through by design, so an unlisted platform stays updatable.
	if err := selfupdate.CheckExecutableFor("plan9", []byte("whatever this is")); err != nil {
		t.Errorf("an unknown GOOS was refused, which makes that platform un-updatable: %v", err)
	}
	// Empty is refused everywhere, including there: a zero-byte asset is never an update.
	if err := selfupdate.CheckExecutableFor("plan9", nil); err == nil {
		t.Error("an empty asset was accepted on an unknown GOOS")
	}
}

// The response-header timeout bounds a server that never answers, and there is no
// whole-request deadline by design. Neither covers the case in between: headers arrive,
// the body stops without the connection closing, and the read parks forever — a
// `unity-sync update` that prints nothing, diagnoses nothing, and only ends when the user
// interrupts it. The store carries a guard for exactly this; the updater had none.
//
// Failing this test looks like the whole package timing out rather than a red assertion,
// because the regression is a read that never returns.
func TestAReleaseBodyThatGoesSilentFailsRatherThanHanging(t *testing.T) {
	selfupdate.ShortenAssetStall(t, 100*time.Millisecond)

	release := make(chan struct{})
	srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/asset") {
			return false
		}
		// Headers and a first chunk, then silence: the connection stays open and the
		// body simply stops, which is what a captive portal or a wedged proxy produces.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "PK\x03\x04")
		w.(http.Flusher).Flush()
		<-release
		return true
	})
	// Registered after releaseServer, so it runs before that helper's srv.Close: cleanups
	// are LIFO, and Close waits for the handler this unblocks. The other order deadlocks
	// the whole package rather than failing this test.
	t.Cleanup(func() { close(release) })

	c := selfupdate.New(srv.URL, "")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, dlErr := selfupdate.DownloadBinary(c, context.Background(), rel)
		done <- dlErr
	}()
	select {
	case dlErr := <-done:
		if dlErr == nil {
			t.Fatal("a body that stopped mid-transfer was accepted")
		}
		if !errors.Is(dlErr, selfupdate.ErrStalled) {
			t.Errorf("err = %v, want it to name the stall rather than the cancellation under it", dlErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download never returned; the stall guard is not bounding the body")
	}
}

// The other half, which is what keeps the guard honest: a transfer that is slow but never
// silent has to survive. A guard that bounded slowness rather than silence would make the
// update impossible on exactly the connections that most need it.
func TestASlowButLiveReleaseBodyIsNotCutOff(t *testing.T) {
	selfupdate.ShortenAssetStall(t, time.Second)

	archive := zipWithBinary(t, nativeBinary(t, "fresh binary"))
	srv := releaseServer(t, nil, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/asset") {
			return false
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Four gaps at a fifth of the window each: far longer in total than the window,
		// never silent for the whole of it.
		for chunk := range slices.Chunk(archive, len(archive)/4+1) {
			time.Sleep(200 * time.Millisecond)
			w.Write(chunk)
			w.(http.Flusher).Flush()
		}
		return true
	})

	c := selfupdate.New(srv.URL, "")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := selfupdate.DownloadBinary(c, context.Background(), rel)
	if err != nil {
		t.Fatalf("a slow but live body was cut off: %v", err)
	}
	if len(got) == 0 {
		t.Error("the slow transfer yielded no binary")
	}
}

// The inner ceiling, which is the one a zip bomb reaches: the archive is small and its
// entry expands past the limit. The declared size is the archive's claim about itself, so
// the read has to be bounded rather than trusted.
func TestAnEntryThatExpandsPastTheCeilingIsRefused(t *testing.T) {
	// Deflates to a few hundred bytes, so the archive itself clears the ceiling set below
	// and only the entry read can catch it.
	big := zipWithBinary(t, "\x7fELF"+strings.Repeat("\x00", 1<<20))
	selfupdate.LowerArchiveCeiling(t, int64(len(big))*4)

	srv := releaseServer(t, big, nil)
	c := selfupdate.New(srv.URL, "")
	rel, err := selfupdate.Resolve(c, context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	_, err = selfupdate.DownloadBinary(c, context.Background(), rel)
	if err == nil {
		t.Fatal("an entry expanding past the ceiling was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want an error naming the size limit", err)
	}
}
