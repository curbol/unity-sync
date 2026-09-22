// Package lockfile reads and writes unity-sync.lock.json, the committed record of what
// an account owns and what is mirrored. It lives beside the project manifest and is
// meant to be diffed: a month's diff should read like a changelog.
//
// Each entry has two halves that are deliberately not mixed. The advertised half is what
// the store currently says and is refreshed every run. The resolution half describes the
// file on disk and is rewritten only when a run actually resolves that asset.
package lockfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// Resolution describes the file on disk. It is rewritten only when a run actually
// resolves that asset, and is otherwise carried forward verbatim.
//
// It is one type rather than eight fields spelled out wherever an entry is built,
// because the carry-forward branch and the resolved branch have to stay in step: a
// field added to one and missed in the other is silently dropped from every entry a
// run does not resolve, which on a no-op run is the whole committed file.
type Resolution struct {
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

	// Embedded last on purpose: encoding/json emits an embedded struct's fields at the
	// embedded field's own index position, so the resolution half keeps the byte
	// placement it had when these were eight fields declared here.
	Resolution
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
	if err := lf.checkUnique(); err != nil {
		return Lockfile{}, fmt.Errorf("%s: %w", path, err)
	}
	return lf, nil
}

// checkUnique refuses two entries for one product. A run never writes such a file — the
// key is derived from the id — but this one is committed, and a rename changes an entry's
// key by construction, so a merge that keeps both sides of one leaves a duplicate. A
// lookup by product id then picks between them at random, because both the index a run
// builds and FindByAssetID range over a map: the same checkout classifies the asset
// Unchanged on one run and Changed on the next, re-fetching gigabytes on a coin flip, and
// the entry not picked is dropped without a word.
func (lf Lockfile) checkUnique() error {
	seen := map[string]string{}
	for key, e := range lf.Assets {
		if e.AssetID == "" {
			// Refused rather than skipped. Classification indexes by product id, so an
			// entry with none is filed under "" and reached by no lookup — the store
			// never yields an empty id — which drops it from the next build and reports
			// it as "no longer owned: <name>, left in place" for an asset the account may
			// still hold. It arrives the way a duplicate does, from a hand-edit or a merge
			// of this committed file, and it is as silent as one is loud.
			return fmt.Errorf("entry %q records no assetId; the key is derived from the id "+
				"and is not the identity, so an entry without one cannot be matched to an "+
				"owned asset", key)
		}
		if first, dup := seen[e.AssetID]; dup {
			a, b := key, first
			if a > b {
				a, b = b, a
			}
			return fmt.Errorf("entries %q and %q both record asset %s; "+
				"delete whichever is stale, most likely the one whose key no longer matches its name",
				a, b, e.AssetID)
		}
		seen[e.AssetID] = key
	}
	return nil
}

// tempPrefix marks a lockfile write in flight. SweepTemps is what reclaims one a killed
// run left behind, so the prefix and its reader live together rather than the prefix
// being exported for a caller to match on.
const tempPrefix = ".unity-sync-lock-"

// syncFile is indirected so a test can watch the flush happen, and happen before the
// rename. Nothing observable distinguishes a Save that skipped it from one that did not —
// the file is correct either way until the machine loses power — so deleting the call left
// every assertion in this package green while removing the thing that makes a run's
// per-asset progress survive a crash.
var syncFile = (*os.File).Sync

// SweepTemps removes lockfile temps a killed run left beside the destination, returning
// how many. Every error path in Save unlinks its own, but a SIGKILL or a power loss
// between CreateTemp and Rename cannot, and Save runs once per resolved asset — so a large
// sync spends a lot of windows there. The leftovers land in the directory the user commits
// and nothing else would ever remove them.
//
// It spares anything newer than the cutoff, so a concurrent run's in-flight write
// survives, and it fails silently for the reason the cache sweep does: housekeeping must
// not stop a run.
func SweepTemps(dir string, olderThan time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(olderThan) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// Save writes the lockfile atomically. encoding/json sorts map keys, so the output is
// stable across runs and a diff shows only what actually changed.
//
// HTML escaping is off. json.Marshal escapes &, < and > by default, which is for embedding
// in a script tag and does nothing for a file on disk — it turned every ampersand in an
// asset name into &, and six of the names in the committed fixtures carry one. The
// file's stated purpose is that a month's diff reads like a changelog, so that cost is
// paid on exactly the thing it is for. Encode appends its own newline, and neither key
// order nor byte stability changes.
func Save(path string, lf Lockfile) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(lf); err != nil {
		return err
	}
	raw := buf.Bytes()
	tmp, err := os.CreateTemp(filepath.Dir(path), tempPrefix+"*")
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
	if err := syncFile(tmp); err != nil {
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
// Identity is the product id rather than the key, so a renamed asset — whose key changes
// by construction — is still recognised as the same thing.
//
// This is the one-off lookup. A run indexes the whole file by id once instead, because it
// needs a lookup per owned asset and another per save; this walk is for a caller with a
// single question to ask.
func (lf Lockfile) FindByAssetID(id string) (key string, e Entry, ok bool) {
	for k, entry := range lf.Assets {
		if entry.AssetID == id {
			return k, entry, true
		}
	}
	return "", Entry{}, false
}
