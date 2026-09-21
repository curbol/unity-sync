// Package selfupdate replaces the running binary with a release build from GitHub.
//
// Its HTTP client follows redirects, unlike every client that talks to the Asset Store.
// That is deliberate: the binary comes from the release *asset* API, which answers with a
// 302 to a signed CDN URL. Inheriting the store's redirect ban here would break updates
// entirely.
package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repo = "curbol/unity-sync"

// maxArchiveBytes bounds a release asset. The published zips are single-digit megabytes,
// so this leaves room to grow by an order of magnitude and still refuses an artifact that
// is plainly not one of them.
const maxArchiveBytes = 256 << 20

// client is the GitHub API surface, injectable so tests need no network.
type client struct {
	http    *http.Client
	apiBase string
	token   string
}

// newClient builds a client. A caller passing an empty base uses api.github.com.
func newClient(apiBase, token string) *client {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	// Bounded on headers rather than on the whole request, for the same reason the store
	// client is: a release archive over a slow link legitimately takes a long time, and a
	// deadline that kills it mid-body makes the update impossible on exactly the
	// connections that most need it. The caller's context supplies cancellation.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &client{
		// This client follows redirects: the asset endpoint 302s to a signed CDN URL.
		http:    &http.Client{Transport: transport},
		apiBase: strings.TrimSuffix(apiBase, "/"),
		token:   token,
	}
}

// Token resolves a GitHub credential from the environment, falling back to the gh CLI.
//
// It is opportunistic: the releases this reads are public, and an empty token means the
// requests go out unauthenticated, which works. What a token buys is GitHub's authenticated
// rate limit, 5000 requests an hour against 60 for an anonymous address.
//
// --hostname github.com because `gh auth token` otherwise answers for the default host,
// which is $GH_HOST or whichever single host happens to be logged in. A user authenticated
// only against their company's GitHub Enterprise would have that token sent to
// api.github.com, where it is worth nothing and turns a working request into a 401.
func token(ctx context.Context) string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// platformAssetForHost is the release archive name for the running platform.
func platformAssetForHost(version string) (string, error) {
	return platformAsset(runtime.GOOS, runtime.GOARCH, version)
}

// platformAsset maps a platform onto the archive name .github/workflows/release.yml
// publishes for it. The two are a contract across files that no compiler checks, so the
// parameters are explicit: a test can drive every platform the workflow builds rather
// than only the one it happens to run on.
func platformAsset(goos, goarch, version string) (string, error) {
	var os_, arch string
	switch goos {
	case "darwin":
		os_ = "mac"
	case "linux":
		os_ = "linux"
	case "windows":
		os_ = "win"
	default:
		return "", fmt.Errorf("unsupported OS %s", goos)
	}
	switch goarch {
	case "amd64":
		arch = "intel"
	case "arm64":
		if os_ == "mac" {
			arch = "apple"
		} else {
			arch = "arm64"
		}
	default:
		return "", fmt.Errorf("unsupported architecture %s", goarch)
	}
	// The workflow builds one Windows target and labels it without an architecture, so
	// every Windows arch resolves to it. Windows on arm64 runs the amd64 image.
	if os_ == "win" {
		return fmt.Sprintf("unity-sync-%s-win.zip", version), nil
	}
	return fmt.Sprintf("unity-sync-%s-%s-%s.zip", version, os_, arch), nil
}

// sameHost refuses a URL that does not belong to the API this client was pointed at, so
// a field in a response cannot redirect the credential somewhere else.
func (c *client) sameHost(raw string) error {
	want, err := url.Parse(c.apiBase)
	if err != nil {
		return err
	}
	got, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("release asset URL %q is unparseable: %w", raw, err)
	}
	if got.Scheme != want.Scheme || got.Host != want.Host {
		return fmt.Errorf("release asset URL %q is not on %s", raw, c.apiBase)
	}
	return nil
}

// get fetches a release resource, authenticated when a credential is available.
//
// A credential that the API rejects falls back to an anonymous request rather than failing
// the update. The token is opportunistic — everything read here is public — so an expired
// PAT left in GITHUB_TOKEN, or a fine-grained one never granted public-repository read,
// would otherwise kill the documented upgrade path with "status 401: Bad credentials" or,
// worse, a 404 that reads as "no release exists". The one thing that cannot be recovered
// that way is a network failure, which is returned as itself.
func (c *client) get(ctx context.Context, url, accept string) (*http.Response, error) {
	resp, err := c.getWith(ctx, url, accept, c.token)
	if err == nil || c.token == "" || !rejectedCredential(err) {
		return resp, err
	}
	anon, anonErr := c.getWith(ctx, url, accept, "")
	if anonErr != nil {
		// The authenticated error is the more informative of the two, and the one that
		// names what the user can change.
		return nil, err
	}
	return anon, nil
}

// errUnauthorized marks the statuses that can mean "this credential", not "this request".
var errUnauthorized = errors.New("github rejected the credential")

func rejectedCredential(err error) bool { return errors.Is(err, errUnauthorized) }

func (c *client) getWith(ctx context.Context, url, accept, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "unity-sync")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		status := fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
		// 404 is in the set on purpose: a fine-grained token with no public-repository
		// read gets one for a release that plainly exists, and it is indistinguishable
		// from the real thing until the anonymous request answers.
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return nil, fmt.Errorf("%w: %w", errUnauthorized, status)
		}
		return nil, status
	}
	return resp, nil
}

// resolve finds a release: the latest, or a specific version when one is named.
func (c *client) resolve(ctx context.Context, version string) (release, error) {
	url := c.apiBase + "/repos/" + repo + "/releases/latest"
	if version != "" {
		url = c.apiBase + "/repos/" + repo + "/releases/tags/v" + strings.TrimPrefix(version, "v")
	}
	resp, err := c.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return release{}, fmt.Errorf("release has no tag")
	}
	return rel, nil
}

// downloadBinary fetches the platform archive for a release and returns the binary
// inside it.
func (c *client) downloadBinary(ctx context.Context, rel release) ([]byte, error) {
	want, err := platformAssetForHost(strings.TrimPrefix(rel.TagName, "v"))
	if err != nil {
		return nil, err
	}
	var assetURL string
	for _, a := range rel.Assets {
		if a.Name == want {
			assetURL = a.URL
			break
		}
	}
	if assetURL == "" {
		return nil, fmt.Errorf("release %s has no asset %s", rel.TagName, want)
	}
	// The URL comes out of the release JSON, and get attaches the user's token to
	// whatever it is handed. Go drops the Authorization header on a redirect to another
	// host, so the signed CDN never sees it — but that only covers the second hop, and
	// this is the first.
	if err := c.sameHost(assetURL); err != nil {
		return nil, err
	}
	// The asset API answers with a 302 to a signed CDN URL; this client follows it.
	resp, err := c.get(ctx, assetURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Bounded: a release that shipped the wrong artifact under the right name would
	// otherwise be buffered whole, and an update that is OOM-killed is a worse way to
	// find that out than an error naming the size.
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(archive)) > maxArchiveBytes {
		return nil, fmt.Errorf("release asset %s is larger than %d bytes", want, maxArchiveBytes)
	}
	return binaryFromZip(archive)
}

func binaryFromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("release asset is not a zip: %w", err)
	}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if name != "unity-sync" && name != "unity-sync.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		// The declared size is the archive's claim about itself, so the read is bounded
		// rather than trusted: a small zip can declare an entry that expands to more
		// memory than the machine has.
		binary, err := io.ReadAll(io.LimitReader(rc, maxArchiveBytes+1))
		if err != nil {
			return nil, err
		}
		if int64(len(binary)) > maxArchiveBytes {
			return nil, fmt.Errorf("the binary in the release asset is larger than %d bytes", maxArchiveBytes)
		}
		return binary, nil
	}
	return nil, fmt.Errorf("release asset contains no unity-sync binary")
}

// executableMagic is the leading signature of a native binary per platform. The zip
// reader has already verified the entry's CRC, so this catches the other way an update
// goes wrong: a release that shipped something which is not a binary at all — an error
// page, a script, the wrong artifact — landing on top of a working install and reporting
// success.
var executableMagic = map[string][][]byte{
	"linux":   {[]byte("\x7fELF")},
	"darwin":  {{0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}},
	"windows": {[]byte("MZ")},
}

// checkExecutable refuses bytes that are not a native binary for this platform. An
// unknown GOOS has no signature to check and is let through rather than made
// un-updatable.
func checkExecutable(binary []byte) error {
	if len(binary) == 0 {
		return fmt.Errorf("the release asset is empty")
	}
	magics, known := executableMagic[runtime.GOOS]
	if !known {
		return nil
	}
	for _, m := range magics {
		if bytes.HasPrefix(binary, m) {
			return nil
		}
	}
	return fmt.Errorf("the release asset is not a %s executable", runtime.GOOS)
}

// replace swaps the running executable for the given bytes, writing beside the target so
// the rename is atomic and cannot leave a half-written binary on PATH.
func replace(targetPath string, binary []byte) error {
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, ".unity-sync-update-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	// The mode the target already has wins, so an install the user locked down stays that
	// way: chmod 700 on a shared machine, then `unity-sync update`, otherwise handed group
	// and other read and execute back and reported success. The execute bit is forced on
	// regardless, because a binary that is not executable is the one thing this must never
	// leave on PATH; 0755 is the fallback when there is no target to read a mode from.
	mode := os.FileMode(0o755)
	if fi, statErr := os.Stat(targetPath); statErr == nil {
		mode = fi.Mode().Perm() | 0o100
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	// Flushed before the rename: the rename publishes this file onto PATH, and a crash
	// with the bytes still in the page cache would publish a truncated binary.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, targetPath); err != nil {
		if !runningImageIsLocked() {
			os.Remove(name)
			return err
		}
		return replaceAside(name, targetPath, err)
	}
	return nil
}

// runningImageIsLocked reports whether this platform refuses to replace the executable
// image of a running process. Windows does; every platform this ships to otherwise
// renames over it happily, so the aside dance below never runs there.
//
// A variable rather than a function so a test can reach replaceAside on a machine that
// would never take that branch. It is the one path where a mistake leaves nothing on
// PATH, and the CI job that runs the platform it exists for cannot exercise a rename
// failure on demand.
var runningImageIsLocked = func() bool { return runtime.GOOS == "windows" }

// replaceAside is the Windows path. Windows will not let a running image be replaced or
// deleted, but it does allow that image to be renamed, so the update moves the old binary
// out of the way and takes its name. A failure puts the working binary back rather than
// leaving nothing on PATH, which is the one outcome an updater must never produce.
func replaceAside(newPath, targetPath string, direct error) error {
	aside := targetPath + ".old"
	os.Remove(aside)
	if err := os.Rename(targetPath, aside); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("%w (and could not move the running binary aside: %v)", direct, err)
	}
	if err := os.Rename(newPath, targetPath); err != nil {
		if restore := os.Rename(aside, targetPath); restore != nil {
			// Nothing is on PATH now. The one thing the user can do about that is rename
			// the aside file back, so the message has to name it; returning only the
			// rename error leaves them with a missing command and no clue where it went.
			os.Remove(newPath)
			return fmt.Errorf("%w; the previous binary could not be put back (%v) and is at %s",
				err, restore, aside)
		}
		os.Remove(newPath)
		return err
	}
	// Fails while the old image is still running, which is the normal case here. The
	// leftover is named beside the binary and is replaced by the next update.
	os.Remove(aside)
	return nil
}

// Run performs an update of the running binary.
//
// The token is opportunistic: get omits the Authorization header when it is empty, and
// GitHub serves a public repository's releases and assets anonymously. Requiring one here
// failed the update for every user who installed a release binary and never set one.
func Run(ctx context.Context, w io.Writer, current, version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	return update(ctx, w, newClient("", token(ctx)), current, version, self)
}

// update is Run with the client and the binary it replaces supplied, which is the only
// seam a test can drive: Run replaces whatever is running, and under `go test` that is the
// test binary.
func update(ctx context.Context, w io.Writer, c *client, current, version, target string) error {
	if current == "dev" {
		return fmt.Errorf("this is a dev build; install a release first")
	}
	rel, err := c.resolve(ctx, version)
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if latest == current {
		fmt.Fprintf(w, "already on %s\n", current)
		return nil
	}
	binary, err := c.downloadBinary(ctx, rel)
	if err != nil {
		return err
	}
	// Before replace, not after: past that rename the working binary is already gone,
	// and leaving nothing usable on PATH is the one outcome an updater must never produce.
	if err := checkExecutable(binary); err != nil {
		return fmt.Errorf("refusing to install %s: %w", latest, err)
	}
	if err := replace(target, binary); err != nil {
		return err
	}
	fmt.Fprintf(w, "updated %s -> %s\n", current, latest)
	return nil
}
