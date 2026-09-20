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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/curbol/unity-sync/internal/model"
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
	// Windows refuses to create a file or directory named for a device, so a segment
	// that reaches here as one would fail the asset on that platform alone. model keeps
	// the derived slugs clear of these; this is the gate for anything that does not
	// come from there.
	if model.ReservedSegment(s) {
		return fmt.Errorf("unsafe %s %q: Windows reserves this name for a device", kind, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("unsafe %s %q: contains a control character", kind, s)
		}
	}
	return nil
}

// Canonical is the one spelling of a cache-relative path, in the forward slashes a
// lockfile records. Two values naming the same file inside the root canonicalise to the
// same string, and anything that would leave the root is refused instead.
//
// It is exported because comparing a recorded path against a derived one is not a string
// comparison. The lockfile is committed, hand-editable and travels between machines, so
// "./pub/a/a.unitypackage" has to compare equal to the "pub/a/a.unitypackage" a run
// derives; a caller that compares them raw decides two names for one file are two files.
//
// The whole check runs in slash space, so it answers identically on every platform. A
// backslash and a colon are refused rather than interpreted: filepath.Clean strips a
// Windows volume name before resolving "..", then puts it back, so "Z:../../x" cleans to
// itself there and slips past a leading-".." test that catches it everywhere else. The
// same committed value would then confine on the machine that wrote it and escape on the
// machine that read it.
func Canonical(rel string) (string, error) {
	if rel == "" || strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	clean := path.Clean(rel)
	if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe cache path %q", rel)
	}
	for _, seg := range strings.Split(clean, "/") {
		if model.ReservedSegment(seg) {
			return "", fmt.Errorf("unsafe cache path %q: %q is a Windows device name", rel, seg)
		}
	}
	return clean, nil
}

// SamePath reports whether two cache-relative paths name the same file. A value that
// cannot be canonicalised matches nothing, including another unsafe value: the caller is
// deciding whether to delete or move a file, and two paths it cannot resolve are not
// grounds for treating them as one.
func SamePath(a, b string) bool {
	ca, err := Canonical(a)
	if err != nil {
		return false
	}
	cb, err := Canonical(b)
	if err != nil {
		return false
	}
	return ca == cb
}

// SameFile reports whether two cache-relative paths name one file on disk.
//
// SamePath cannot answer that everywhere. It compares canonical spellings, and on the
// two case-insensitive filesystems this ships to — Windows, and macOS as it is usually
// configured — "Pub/a.unitypackage" and "pub/a.unitypackage" are one file that SamePath
// calls two. The callers are deciding whether to delete or move, so the difference is a
// run removing the package it just wrote as though it were a superseded copy.
//
// It answers false when either path is unsafe or absent, so a caller pairs it with
// SamePath rather than replacing it: SamePath settles the spelling without touching the
// disk, and this settles what only the filesystem knows.
func SameFile(root, a, b string) bool {
	fa, err := statRel(root, a)
	if err != nil {
		return false
	}
	fb, err := statRel(root, b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func statRel(root, rel string) (os.FileInfo, error) {
	r, name, err := rooted(root, rel)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Stat(name)
}

// rooted opens the library as an os.Root and resolves rel inside it, returning the name to
// hand the root's own methods.
//
// Canonical checks a spelling, and a spelling cannot see the whole question: a path whose
// every segment is an ordinary name still leaves the tree when one of those segments is a
// symlink, because a link is followed like any other directory. These values arrive from
// the lockfile, which is committed, hand-editable and read on other machines, and
// RemoveStale deletes what it is given — so the confinement has to be the filesystem's
// rather than the string's, and it has to be enforced by the same syscall that acts, which
// is what leaves no window between the check and the use.
func rooted(root, rel string) (*os.Root, string, error) {
	clean, err := Canonical(rel)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	return r, filepath.FromSlash(clean), nil
}

// resolve joins a cache-relative path onto the root for the callers that need the name
// rather than the file: a map key, or the directory pruning walks up from.
//
// It confines the spelling and nothing more. Every operation on a recorded path — opening
// it, moving it, removing it — goes through rooted instead, because a lexically confined
// path can still resolve through a link that leaves the library.
func resolve(root, rel string) (string, error) {
	clean, err := Canonical(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

// Pending is a fully-written but uncommitted download.
type Pending struct {
	RelPath string
	SHA256  string
	Size    int64

	root     string
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
	// The write gate has to be no weaker than the read gate. safeSegment refuses a
	// separator and a device name; Canonical additionally refuses a colon, so a segment
	// carrying one is a path this can create on Linux and macOS and no later run can
	// resolve — the entry verifies false, the exclusion it becomes refuses every adopt
	// candidate, and the asset re-downloads in full on every run with no error saying why.
	if _, err := Canonical(rel); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, publisherSlug, assetSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		pruneEmptyParents(root, dir)
		return nil, err
	}
	// Every failure below also unwinds the directories MkdirAll just made. An asset whose
	// download never succeeds would otherwise leave an empty <publisher>/<asset>/ behind
	// on every attempt, in a tree quarry walks.
	abandon := func(err error) (*Pending, error) {
		os.Remove(tmp.Name())
		pruneEmptyParents(root, dir)
		return nil, err
	}
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return abandon(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return abandon(err)
	}
	if err := tmp.Close(); err != nil {
		return abandon(err)
	}
	return &Pending{
		RelPath:  rel,
		SHA256:   hex.EncodeToString(h.Sum(nil)),
		Size:     size,
		root:     root,
		tempPath: tmp.Name(),
		final:    filepath.Join(dir, assetSlug+packageExt),
	}, nil
}

// Commit renames the pending bytes into place.
//
// It is the one write here that deliberately lands on an occupied path: the occupant is
// this asset's own superseded version, and refusing it the way Relocate does would make
// every re-download fail.
func (p *Pending) Commit() error {
	// CreateTemp makes the file 0600 and the rename carries that over, so a downloaded
	// package would be owner-only inside a 0755 tree while an adopted one keeps the 0644
	// it arrived with — one asset changing mode depending on how it got here. An
	// existing destination's mode wins, so a library deliberately locked down stays so.
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(p.final); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(p.tempPath, mode); err != nil {
		return p.abandon(err)
	}
	if err := os.Rename(p.tempPath, p.final); err != nil {
		// Unwound like every other failure that removes the temp. A rename can fail with
		// the destination held open — an editor, an on-access scanner — and without this
		// the empty <publisher>/<asset>/ Store created stays in the tree quarry walks.
		return p.abandon(err)
	}
	return nil
}

// abandon drops the temp and the directories Store made for it, returning the error that
// caused it.
func (p *Pending) abandon(err error) error {
	os.Remove(p.tempPath)
	pruneEmptyParents(p.root, filepath.Dir(p.tempPath))
	return err
}

// Discard removes the pending bytes, and the directories Store created for them if the
// removal leaves them empty. Callers use it whenever a check fails, so a rejected body
// never reaches a real cache path.
func (p *Pending) Discard() error {
	err := os.Remove(p.tempPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pruneEmptyParents(p.root, filepath.Dir(p.tempPath))
	return nil
}

// Verify is the cheap check: the file exists, its size is exactly what was recorded, and
// — when a delivered version id was recorded — its own metadata still says so. Exact
// recorded size is what makes truncation detectable without hashing, since the metadata
// block sits in the leading bytes and survives a truncation.
//
// An entry with no delivered id is verified on size alone. Requiring a metadata match
// there would make a package that simply has no descriptor re-download on every run.
func Verify(root, rel string, wantSize int64, wantDeliveredID string) bool {
	r, name, err := rooted(root, rel)
	if err != nil {
		return false
	}
	defer r.Close()
	fi, err := r.Stat(name)
	// IsDir as well as size: a hand-edited cachePath missing its filename segment names
	// the asset's own directory, which always exists once a run has written there.
	if err != nil || fi.IsDir() || fi.Size() != wantSize {
		return false
	}
	if wantDeliveredID == "" {
		return true
	}
	f, err := r.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	m, err := unitypackage.Read(f)
	if err != nil {
		return false
	}
	return m.VersionID == wantDeliveredID
}

// VerifyDeep re-hashes the file. It is opt-in because the library runs to tens of
// gigabytes, and it is the only check that sees a mid-file corruption.
//
// It takes no size or version id, and does not need them: a digest match implies both.
func VerifyDeep(root, rel, wantSHA string) bool {
	sha, _, err := Hash(root, rel)
	return err == nil && sha == wantSHA
}

// Hash returns a cached file's digest and size, for adopting a file the tool did not
// download itself.
func Hash(root, rel string) (string, int64, error) {
	r, name, err := rooted(root, rel)
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	f, err := r.Open(name)
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

// Index is one pass over the library, grouping every package by the product id its own
// descriptor claims.
//
// It exists because adoption is a per-asset question asked against a whole-tree answer.
// Probing per asset means one walk and one header parse per package per asset, which is
// quadratic exactly when adoption matters most: a lost lockfile makes every owned asset
// ask, over a library that already holds them all.
type Index struct {
	root      string
	byProduct map[string][]Candidate
}

// Scan reads the library once. It is the caller's job to scan after any sweep of
// abandoned temps and before the adopt probes that consult it.
//
// Only files ending .unitypackage and not starting with a dot are considered, so an
// abandoned download temp can never be adopted: a partial can be large enough to clear a
// size floor while still carrying an intact descriptor.
//
// Nothing here fails. An unreadable subtree, a file whose header will not parse, or a root
// that does not exist yet on a first run simply yields no candidate, and the caller falls
// back to a download, where the full set of guards applies.
func Scan(ctx context.Context, root string) *Index {
	ix := &Index{root: root, byProduct: map[string][]Candidate{}}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		// A header parse per package over a 75 GB library is long enough that a run
		// cancelled here would otherwise go on reading for minutes after being told to
		// stop. A short index is safe: a candidate the walk never reached is one the
		// caller falls back to downloading, where the full set of guards applies.
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, packageExt) {
			return nil
		}
		m, err := unitypackage.ReadFile(p)
		if err != nil || m.ID == "" {
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
		ix.byProduct[m.ID] = append(ix.byProduct[m.ID],
			Candidate{RelPath: filepath.ToSlash(rel), Size: fi.Size(), Metadata: m})
		return nil
	})
	return ix
}

// Find returns a package whose own metadata claims the given product id. It answers from
// the scan rather than probing the derived path, because the whole point of adoption is a
// file that is not where the current layout would put it — after a rename, say.
//
// When several files claim the same product, the one already at preferRel wins, so an
// adopt that is really a no-op does not turn into a relocation conflict. Paths in
// excludeRel are skipped entirely: a file that just failed verification is not a candidate
// for adoption, however intact its descriptor still looks.
func (ix *Index) Find(productID, preferRel string, excludeRel ...string) (Candidate, bool) {
	// Resolved, not compared as strings: excludeRel comes from the lockfile, which is
	// hand-editable and travels between machines, so "./pub/a/a.unitypackage" has to skip
	// the same file "pub/a/a.unitypackage" names. Missing the match would re-offer a file
	// that just failed verification as a candidate to adopt.
	r, err := os.OpenRoot(ix.root)
	if err != nil {
		return Candidate{}, false
	}
	defer r.Close()

	skip := map[string]bool{}
	// Two spellings of one file can differ in more than punctuation: on a
	// case-insensitive filesystem they differ in case, which no canonical form
	// collapses. The identity the filesystem reports is checked alongside the string,
	// for the exclusions that name a file actually on disk.
	var skipIDs []os.FileInfo
	for _, e := range excludeRel {
		if e == "" {
			continue
		}
		full, err := resolve(ix.root, e)
		if err != nil {
			// The caller is naming a file that must not be adopted and this cannot tell
			// which one it is. Refusing every candidate falls back to a re-download,
			// where the full set of guards applies; guessing would let the excluded file
			// back in through the one door that skips them.
			return Candidate{}, false
		}
		skip[full] = true
		if fi, err := r.Stat(filepath.FromSlash(path.Clean(e))); err == nil {
			skipIDs = append(skipIDs, fi)
		}
	}
	var found []Candidate
	for _, c := range ix.byProduct[productID] {
		full, err := resolve(ix.root, c.RelPath)
		if err != nil || skip[full] {
			continue
		}
		// Re-checked against the filesystem, because a run relocates and removes packages
		// while it classifies: the scan is a snapshot, and handing back a path that has
		// since moved would fail an adopt that a re-scan would have completed.
		fi, err := r.Stat(filepath.FromSlash(path.Clean(c.RelPath)))
		if err != nil || sameAsAny(fi, skipIDs) {
			continue
		}
		found = append(found, c)
	}
	if len(found) == 0 {
		return Candidate{}, false
	}
	// Canonically, like the exclusions above and for the same reason: preferRel is the
	// path the current layout derives, and a caller holding a differently-spelled one
	// would silently lose "the copy already in place wins" and get a relocation conflict
	// where an adopt was really a no-op.
	for _, c := range found {
		if SamePath(c.RelPath, preferRel) || SameFile(ix.root, c.RelPath, preferRel) {
			return c, true
		}
	}
	return found[0], true
}

func sameAsAny(fi os.FileInfo, others []os.FileInfo) bool {
	for _, o := range others {
		if os.SameFile(fi, o) {
			return true
		}
	}
	return false
}

// Relocate moves a package to where the current layout puts it, creating parents and
// pruning directories the move empties.
//
// It is a no-op when the file is already there, and it refuses a destination holding a
// different file rather than renaming over it: the caller records the digest of whatever
// ends up at that path, so a silent overwrite would certify the wrong bytes.
//
// "Already there" is a question about files, not about spellings. Windows and macOS as it
// is usually configured ignore case, so a recorded "Pub/a.unitypackage" and a derived
// "pub/a.unitypackage" are one file that no canonical form collapses — and refusing that
// as an occupied destination fails the adopt of a package already exactly where it
// belongs, on those platforms alone and identically on every later run.
func Relocate(root, fromRel, toRel string) error {
	from, err := Canonical(fromRel)
	if err != nil {
		return err
	}
	to, err := Canonical(toRel)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	fromName, toName := filepath.FromSlash(from), filepath.FromSlash(to)

	switch dst, statErr := r.Stat(toName); {
	case statErr == nil:
		if src, err := r.Stat(fromName); err == nil && os.SameFile(src, dst) {
			return nil
		}
		return fmt.Errorf("refusing to move %s onto %s: destination already holds a file", fromRel, toRel)
	case !os.IsNotExist(statErr):
		return statErr
	}
	if dir := filepath.Dir(toName); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := r.Rename(fromName, toName); err != nil {
		return err
	}
	pruneEmptyParents(root, filepath.Dir(filepath.Join(root, fromName)))
	return nil
}

// pruneEmptyParents removes directories the move emptied, walking up but never past the
// library root.
//
// Confinement is filepath.Rel, not a string prefix. The root arrives however the user
// spelled it, while every path it is compared against has been through resolve, which
// joins and cleans — so a prefix test has to reproduce that cleaning, and gets at least
// one spelling wrong every time. A bare "." cleans to ".", but Join(".", "pub/a") cleans
// to "pub/a", which carries no "./" for the prefix to match: every prune site is
// silently dead for `--library .`. Rel cleans both sides itself and answers the same for
// ".", "./lib", "lib/", "/" and a drive root alike.
func pruneEmptyParents(root, dir string) {
	for {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
//
// Nothing here fails: a subtree that cannot be read, or a root that does not exist yet on
// a first run, is skipped. One unreadable directory must not stop a 75 GB mirror over a
// housekeeping pass.
func SweepTemps(ctx context.Context, root string, olderThan time.Time) (int, int64) {
	var count int
	var bytes int64
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		// Housekeeping, so a cancelled run stops here rather than finishing a walk of the
		// whole library first. Whatever is left is swept by the next run.
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
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
	return count, bytes
}

// RemoveStale deletes a package this tool mirrored and is now replacing with another copy
// of the same asset, and prunes the directories the removal empties. It is only ever
// called with a path the lockfile itself recorded, never with a file the tool did not
// write.
func RemoveStale(root, rel string) error {
	r, name, err := rooted(root, rel)
	if err != nil {
		// A library that is not there yet has nothing to remove; anything else is real.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer r.Close()
	if err := r.Remove(name); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pruneEmptyParents(root, filepath.Dir(filepath.Join(root, name)))
	return nil
}
