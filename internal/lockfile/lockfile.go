// Package lockfile reads and writes unity-sync.lock.json, the committed record of what
// an account owns and what is mirrored. It lives beside the project manifest and is
// meant to be diffed: a month's diff should read like a changelog.
//
// Each entry has two halves that are deliberately not mixed. The advertised half is what
// the store currently says and is refreshed every run. The resolution half describes the
// file on disk and is rewritten only when a run actually resolves that asset.
package lockfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Version records one build. ID is what a diff compares.
type Version struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	PublishedDate string `json:"publishedDate,omitempty"`
}

// Publisher is carried for display and for the cache's vendor directory.
type Publisher struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Entry is one owned asset. Every owned asset gets one, whether or not it is selected,
// because the file is the record of what is *owned*, not of what happens to be mirrored.
type Entry struct {
	// AssetID is the store product id. It is deliberately not called productId: the API
	// has a field of that exact name holding a different, unusable value, and this is
	// the one place a reader compares the two documents side by side.
	AssetID string `json:"assetId"`

	// Advertised half, refreshed on every run for every owned asset.
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Publisher      Publisher `json:"publisher"`
	Version        Version   `json:"version"`
	AdvertisedSize int64     `json:"advertisedSize"`

	// Resolution half, rewritten only when a run resolves this asset.
	//
	// Tracked means "bytes have been mirrored at some point", not "selected right now":
	// an asset stays tracked after it is disabled in the manifest, because the file is
	// still there.
	Tracked bool `json:"tracked"`

	// ResolvedVersionID is the advertised id the cached file was fetched against, and
	// it — not Version.ID — is what classification compares. Keeping them apart is what
	// stops a refreshed advertised id from being written next to an older file and
	// making it look current forever.
	ResolvedVersionID string `json:"resolvedVersionId,omitempty"`

	// DeliveredVersionID is what the stored file's own descriptor says. It can differ
	// from the advertised id: some products are advertised at one version and served at
	// another, steadily.
	DeliveredVersionID string `json:"deliveredVersionId,omitempty"`

	SizeBytes int64  `json:"sizeBytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	CachePath string `json:"cachePath,omitempty"`

	// DownloadedAt is when bytes were last fetched. An adopted entry leaves it empty:
	// the tool found the file, it did not fetch it.
	DownloadedAt string `json:"downloadedAt,omitempty"`

	// StoreFilename is what the store called the package. Recorded for reference; it
	// never determines a path.
	StoreFilename string `json:"storeFilename,omitempty"`
}

// Lockfile is the whole document.
type Lockfile struct {
	Assets map[string]Entry `json:"assets"`
}

// New returns an empty lockfile.
func New() Lockfile { return Lockfile{Assets: map[string]Entry{}} }

// Load reads a lockfile, returning an empty one when the path does not exist.
func Load(path string) (Lockfile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return Lockfile{}, fmt.Errorf("read lockfile: %w", err)
	}
	var lf Lockfile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return Lockfile{}, fmt.Errorf("parse lockfile: %w", err)
	}
	if lf.Assets == nil {
		lf.Assets = map[string]Entry{}
	}
	return lf, nil
}

// Save writes the lockfile atomically. encoding/json sorts map keys, so the output is
// stable across runs and a diff shows only what actually changed.
func Save(path string, lf Lockfile) error {
	raw, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".unity-sync-lock-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	// Flushed before the rename, not merely written: a run persists this file after every
	// download so a crash at asset 90 of 100 keeps the 89 already fetched, and bytes still
	// sitting in the page cache when the rename returns are exactly the record that loses.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// CreateTemp makes the file 0600 and the rename carries that over, which would quietly
	// strip group and other from a file this design wants committed and hand-edited.
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	// Cleaned up like every other failure above. On Windows the rename fails outright
	// when the destination is held open — an editor, an on-access scanner — and this one
	// is written once per download, so the orphans pile up in a directory that is
	// committed.
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// FindByAssetID returns the entry recorded for a product id, whatever key it sits under.
// Classification uses this rather than the key, so a renamed asset — whose key changes
// by construction — is still recognised as the same thing.
func (lf Lockfile) FindByAssetID(id string) (key string, e Entry, ok bool) {
	for k, entry := range lf.Assets {
		if entry.AssetID == id {
			return k, entry, true
		}
	}
	return "", Entry{}, false
}
