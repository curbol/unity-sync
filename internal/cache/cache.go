// Package cache is the local package mirror. Its layout is three segments deep —
// <publisher>/<asset>/<asset>.unitypackage — because quarry derives its vendor facet
// from the first path segment and its pack facet from the second, filling the latter
// only when a path has at least three parts. A flat tree would index every package with
// both facets empty.
//
// Writes are two-phase on purpose: Store leaves the bytes in a temp file so the caller's
// semantic checks run before Commit renames anything into place. Nothing unverified ever
// occupies a real cache path, even briefly, because an interrupt in that window would
// strand a rejected body where the next run's adopt scan would take it for genuine.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/curbol/unity-sync/internal/unitypackage"
)

// tempPrefix marks an in-flight download. The sweep looks for it; the adopt scan
// deliberately does not consider it.
const tempPrefix = ".unity-sync-dl-"

const packageExt = ".unitypackage"

// RelPath is an asset's location relative to the library root, in forward slashes so the
// value is portable in a committed lockfile.
func RelPath(publisherSlug, assetSlug string) string {
	return path.Join(publisherSlug, assetSlug, assetSlug+packageExt)
}

// safeSegment rejects anything that is not a single, ordinary path element. Both slugs
// are derived from store-supplied names, so neither is trusted to be a bare word.
func safeSegment(kind, s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) || strings.HasPrefix(s, ".") {
		return fmt.Errorf("unsafe %s %q", kind, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("unsafe %s %q: contains a control character", kind, s)
		}
	}
	return nil
}

// resolve turns a lockfile-supplied relative path into an absolute one, refusing
// anything that would leave the library root. These values arrive from a file that is
// committed and travels between machines, so they are validated rather than trusted.
func resolve(root, rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	return filepath.Join(root, clean), nil
}

// Pending is a fully-written but uncommitted download.
type Pending struct {
	RelPath string
	SHA256  string
	Size    int64

	tempPath string
	final    string
}

// TempPath is where the bytes currently are, so the caller can inspect them before
// deciding to commit.
func (p *Pending) TempPath() string { return p.tempPath }

// Store streams r into a temp file beside its eventual destination, hashing as it goes.
// It does not rename: the caller commits or discards.
func Store(root, publisherSlug, assetSlug string, r io.Reader) (*Pending, error) {
	if err := safeSegment("publisher slug", publisherSlug); err != nil {
		return nil, err
	}
	if err := safeSegment("asset slug", assetSlug); err != nil {
		return nil, err
	}
	rel := RelPath(publisherSlug, assetSlug)
	dir := filepath.Join(root, publisherSlug, assetSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	return &Pending{
		RelPath:  rel,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
		Size:     size,
		tempPath: tmp.Name(),
		final:    filepath.Join(dir, assetSlug+packageExt),
	}, nil
}

// Commit renames the pending bytes into place.
func (p *Pending) Commit() error {
	if err := os.Rename(p.tempPath, p.final); err != nil {
		os.Remove(p.tempPath)
		return err
	}
	return nil
}

// Discard removes the pending bytes. Callers use it whenever a check fails, so a
// rejected body never reaches a real cache path.
func (p *Pending) Discard() error {
	err := os.Remove(p.tempPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Verify is the cheap check: the file exists, its size is exactly what was recorded, and
// — when a delivered version id was recorded — its own metadata still says so. Exact
// recorded size is what makes truncation detectable without hashing, since the metadata
// block sits in the leading bytes and survives a truncation.
//
// An entry with no delivered id is verified on size alone. Requiring a metadata match
// there would make a package that simply has no descriptor re-download on every run.
func Verify(root, rel string, wantSize int64, wantDeliveredID string) bool {
	full, err := resolve(root, rel)
	if err != nil {
		return false
	}
	fi, err := os.Stat(full)
	if err != nil || fi.Size() != wantSize {
		return false
	}
	if wantDeliveredID == "" {
		return true
	}
	m, err := unitypackage.ReadFile(full)
	if err != nil {
		return false
	}
	return m.VersionID == wantDeliveredID
}

// VerifyDeep re-hashes the file. It is opt-in because the library runs to tens of
// gigabytes, and it is the only check that sees a mid-file corruption.
func VerifyDeep(root, rel, wantSHA string) bool {
	full, err := resolve(root, rel)
	if err != nil {
		return false
	}
	f, err := os.Open(full)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == wantSHA
}

// Hash returns a cached file's digest and size, for adopting a file the tool did not
// download itself.
func Hash(root, rel string) (string, int64, error) {
	full, err := resolve(root, rel)
	if err != nil {
		return "", 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// Candidate is a package found on disk during an adopt scan.
type Candidate struct {
	RelPath  string
	Size     int64
	Metadata unitypackage.Metadata
}

// Locate scans the library for a package whose own metadata claims the given product id.
// It scans rather than probing the derived path because the whole point of adoption is a
// file that is not where the current layout would put it — after a rename, say.
//
// Only files ending .unitypackage and not starting with a dot are considered, so an
// abandoned download temp can never be adopted: a partial can be large enough to clear a
// size floor while still carrying an intact descriptor.
//
// When several files claim the same product, the one already at preferRel wins, so an
// adopt that is really a no-op does not turn into a relocation conflict. Paths in
// excludeRel are skipped entirely: a file that just failed verification is not a candidate
// for adoption, however intact its descriptor still looks.
func Locate(root, productID, preferRel string, excludeRel ...string) (Candidate, bool) {
	// Resolved, not compared as strings: excludeRel comes from the lockfile, which is
	// hand-editable and travels between machines, so "./pub/a/a.unitypackage" has to skip
	// the same file "pub/a/a.unitypackage" names. Missing the match would re-offer a file
	// that just failed verification as a candidate to adopt.
	skip := map[string]bool{}
	for _, e := range excludeRel {
		if full, err := resolve(root, e); err == nil {
			skip[full] = true
		}
	}
	var found []Candidate
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, packageExt) {
			return nil
		}
		m, err := unitypackage.ReadFile(p)
		if err != nil || m.ID != productID {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if skip[filepath.Clean(p)] {
			return nil
		}
		found = append(found, Candidate{RelPath: filepath.ToSlash(rel), Size: fi.Size(), Metadata: m})
		return nil
	})
	if len(found) == 0 {
		return Candidate{}, false
	}
	for _, c := range found {
		if c.RelPath == preferRel {
			return c, true
		}
	}
	return found[0], true
}

// Relocate moves a package to where the current layout puts it, creating parents and
// pruning directories the move empties.
//
// It is a no-op when the file is already there, and it refuses a destination holding a
// different file rather than renaming over it: the caller records the digest of whatever
// ends up at that path, so a silent overwrite would certify the wrong bytes.
func Relocate(root, fromRel, toRel string) error {
	from, err := resolve(root, fromRel)
	if err != nil {
		return err
	}
	to, err := resolve(root, toRel)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("refusing to move %s onto %s: destination already holds a file", fromRel, toRel)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return err
	}
	pruneEmptyParents(root, filepath.Dir(from))
	return nil
}

// pruneEmptyParents removes directories the move emptied, walking up but never past the
// library root.
func pruneEmptyParents(root, dir string) {
	for {
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// SweepTemps removes abandoned download temps anywhere in the tree, returning how many
// and how many bytes. It walks rather than scanning the root, because temps live beside
// their destinations, and it spares anything newer than the cutoff so a concurrent run's
// in-flight transfer survives.
func SweepTemps(root string, olderThan time.Time) (int, int64, error) {
	var count int
	var bytes int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), tempPrefix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.ModTime().Before(olderThan) {
			return nil
		}
		if os.Remove(p) == nil {
			count++
			bytes += fi.Size()
		}
		return nil
	})
	return count, bytes, err
}

// RemoveStale deletes a package this tool mirrored and is now replacing with another copy
// of the same asset, and prunes the directories the removal empties. It is only ever
// called with a path the lockfile itself recorded, never with a file the tool did not
// write.
func RemoveStale(root, rel string) error {
	full, err := resolve(root, rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pruneEmptyParents(root, filepath.Dir(full))
	return nil
}
