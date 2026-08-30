// Package selfupdate replaces the running binary with a release build from GitHub.
//
// Its HTTP client follows redirects, unlike every client that talks to the Asset Store.
// That is deliberate: the repository is private, so the binary comes from the release
// *asset* API, which answers with a 302 to a signed CDN URL. Inheriting the store's
// redirect ban here would break updates entirely.
package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repo = "curbol/unity-sync"

// Client is the GitHub API surface, injectable so tests need no network.
type Client struct {
	http    *http.Client
	apiBase string
	token   string
}

// New builds a client. A caller passing an empty base uses api.github.com.
func New(apiBase, token string) *Client {
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return &Client{
		// This client follows redirects: the asset endpoint 302s to a signed CDN URL.
		http:    &http.Client{Timeout: 10 * time.Minute},
		apiBase: strings.TrimSuffix(apiBase, "/"),
		token:   token,
	}
}

// Token resolves a GitHub credential from the environment, falling back to the gh CLI.
// The repository is private, so an unauthenticated update cannot even list releases.
func Token(ctx context.Context) string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
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

// PlatformAsset is the release archive name for the running platform.
func PlatformAsset(version string) (string, error) {
	var os_, arch string
	switch runtime.GOOS {
	case "darwin":
		os_ = "mac"
	case "linux":
		os_ = "linux"
	case "windows":
		os_ = "win"
	default:
		return "", fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		arch = "intel"
	case "arm64":
		if os_ == "mac" {
			arch = "apple"
		} else {
			arch = "arm64"
		}
	default:
		return "", fmt.Errorf("unsupported architecture %s", runtime.GOARCH)
	}
	if os_ == "win" {
		return fmt.Sprintf("unity-sync-%s-win.zip", version), nil
	}
	return fmt.Sprintf("unity-sync-%s-%s-%s.zip", version, os_, arch), nil
}

func (c *Client) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "unity-sync")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// Resolve finds a release: the latest, or a specific version when one is named.
func (c *Client) Resolve(ctx context.Context, version string) (release, error) {
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

// DownloadBinary fetches the platform archive for a release and returns the binary
// inside it.
func (c *Client) DownloadBinary(ctx context.Context, rel release) ([]byte, error) {
	want, err := PlatformAsset(strings.TrimPrefix(rel.TagName, "v"))
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
	// The asset API answers with a 302 to a signed CDN URL; this client follows it.
	resp, err := c.get(ctx, assetURL, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
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
		return io.ReadAll(rc)
	}
	return nil, fmt.Errorf("release asset contains no unity-sync binary")
}

// Replace swaps the running executable for the given bytes, writing beside the target so
// the rename is atomic and cannot leave a half-written binary on PATH.
func Replace(targetPath string, binary []byte) error {
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
	if err := tmp.Chmod(0o755); err != nil {
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
func runningImageIsLocked() bool { return runtime.GOOS == "windows" }

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
		os.Rename(aside, targetPath)
		os.Remove(newPath)
		return err
	}
	// Fails while the old image is still running, which is the normal case here. The
	// leftover is named beside the binary and is replaced by the next update.
	os.Remove(aside)
	return nil
}

// Run performs an update of the running binary.
func Run(ctx context.Context, current, version string) error {
	if current == "dev" {
		return fmt.Errorf("this is a dev build; install a release first")
	}
	token := Token(ctx)
	if token == "" {
		return fmt.Errorf("no GitHub credential: set GITHUB_TOKEN or run `gh auth login` (the repo is private)")
	}
	c := New("", token)
	rel, err := c.Resolve(ctx, version)
	if err != nil {
		return err
	}
	target := strings.TrimPrefix(rel.TagName, "v")
	if target == current {
		fmt.Printf("already on %s\n", current)
		return nil
	}
	binary, err := c.DownloadBinary(ctx, rel)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if err := Replace(self, binary); err != nil {
		return err
	}
	fmt.Printf("updated %s -> %s\n", current, target)
	return nil
}
