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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	root string
	// Both are root-relative and in slash space, like every other path this package
	// carries, so the operations that act on them can go back through os.Root.
	tempRel string
	dirRel  string
}

// TempPath is where the bytes currently are, so the caller can inspect them before
// deciding to commit. Store wrote it through the root, so the lexical join names the same
// file the root resolved to.
func (p *Pending) TempPath() string {
	return filepath.Join(p.root, filepath.FromSlash(p.tempRel))
}

// newTemp creates a uniquely named temp inside dirRel, through the root. os.Root has no
// CreateTemp, and the point of this whole path is that it must not step outside one.
func newTemp(rt *os.Root, dirRel string) (string, *os.File, error) {
	for attempt := 0; attempt < 10000; attempt++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		rel := path.Join(dirRel, tempPrefix+hex.EncodeToString(b[:]))
		f, err := rt.OpenFile(filepath.FromSlash(rel), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return rel, f, nil
	}
	return "", nil, fmt.Errorf("could not create a temp file in %s", dirRel)
}

// Store streams r into a temp file beside its eventual destination, hashing as it goes.
// It does not rename: the caller commits or discards.
//
// Every step goes through os.Root, because the write gate has to be no weaker than the
// read gate in both the ways a path can leave the library. Canonical settles the
// spelling; os.Root settles what only the filesystem knows, which is that an ordinary
// segment can still be a symlink out of the tree. Writing through a link the later reads
// refuse is the worst of the three outcomes: the download succeeds and is recorded, then
// Verify, Hash and the adopt scan all refuse the file it just wrote, so the asset
// classifies CacheMissing and re-downloads in full on every subsequent run, forever and
// with nothing said. Refusing here fails that asset once, with a diagnostic.
func Store(root, publisherSlug, assetSlug string, r io.Reader) (*Pending, error) {
	if err := safeSegment("publisher slug", publisherSlug); err != nil {
		return nil, err
	}
	if err := safeSegment("asset slug", assetSlug); err != nil {
		return nil, err
	}
	rel := RelPath(publisherSlug, assetSlug)
	// safeSegment refuses a separator and a device name; Canonical additionally refuses a
	// colon, so a segment carrying one is a path this can create on Linux and macOS and no
	// later run can resolve.
	if _, err := Canonical(rel); err != nil {
		return nil, err
	}
	dirRel := path.Join(publisherSlug, assetSlug)

	// The library root itself is created here rather than inside the root, because there
	// is no root to open until it exists: a first run has nothing on disk yet. It is the
	// user's own --library value, so creating it is not the confinement question — what
	// is confined is everything built underneath it.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rt.Close()

	if err := rt.MkdirAll(filepath.FromSlash(dirRel), 0o755); err != nil {
		// Unwound like every failure below it. MkdirAll makes <publisher>/ and then
		// <asset>/, so a failure on the second — ENOSPC, a per-directory ACL, a
		// segment that is already a regular file — leaves the first behind in the tree
		// quarry walks, on every run for every asset under that publisher. Both levels
		// are asked, because pruneEmptyParents stops at a directory it cannot open and
		// so would not walk up from a leaf that was never created.
		pruneEmptyParents(rt, dirRel)
		pruneEmptyParents(rt, path.Dir(dirRel))
		return nil, confinementError(rt, root, dirRel, err)
	}
	tempRel, tmp, err := newTemp(rt, dirRel)
	if err != nil {
		pruneEmptyParents(rt, dirRel)
		return nil, confinementError(rt, root, dirRel, err)
	}
	// Every failure below also unwinds the directories MkdirAll just made. An asset whose
	// download never succeeds would otherwise leave an empty <publisher>/<asset>/ behind
	// on every attempt, in a tree quarry walks.
	abandon := func(err error) (*Pending, error) {
		rt.Remove(filepath.FromSlash(tempRel))
		pruneEmptyParents(rt, dirRel)
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
		RelPath: rel,
		SHA256:  hex.EncodeToString(h.Sum(nil)),
		Size:    size,
		root:    root,
		tempRel: tempRel,
		dirRel:  dirRel,
	}, nil
}

// confinementError says which segment is a symlink when one is, because os.Root reports
// only "path escapes from parent" and the stdlib exports no sentinel to test for. The
// cause is found by looking rather than by reading the message: the first segment that
// Lstat calls a link is the one that took the path out of the library. Symlinking a
// publisher directory onto another disk is a reasonable thing to do to a 75 GB library,
// and it is worth saying so rather than leaving the user with a bare refusal.
func confinementError(rt *os.Root, root, dirRel string, err error) error {
	link := firstSymlink(rt, dirRel)
	if link == "" {
		return fmt.Errorf("creating %s in the library at %s: %w", dirRel, root, err)
	}
	return fmt.Errorf("%s is a symlink, so %s leaves the library at %s: every read and "+
		"write is confined to the library, so a package stored through the link could "+
		"never be found again and would re-download on every run (point --library at the "+
		"real directory, or replace the link with a bind mount): %w",
		link, dirRel, root, err)
}

// firstSymlink returns the first segment of relDir that is a symbolic link, or "" when
// none is. Each segment is Lstat'd in turn, so the link itself is always reachable even
// though anything under it is not.
func firstSymlink(rt *os.Root, relDir string) string {
	var walked string
	for _, seg := range strings.Split(path.Clean(relDir), "/") {
		walked = path.Join(walked, seg)
		fi, err := rt.Lstat(filepath.FromSlash(walked))
		if err != nil {
			return ""
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return walked
		}
	}
	return ""
}

// Commit renames the pending bytes into place.
//
// It is the one write here that deliberately lands on an occupied path: the occupant is
// this asset's own superseded version, and refusing it the way Relocate does would make
// every re-download fail.
func (p *Pending) Commit() error {
	rt, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer rt.Close()

	tempName := filepath.FromSlash(p.tempRel)
	finalName := filepath.FromSlash(p.RelPath)
	// The temp is created 0600 and the rename carries that over, so a downloaded
	// package would be owner-only inside a 0755 tree while an adopted one keeps the 0644
	// it arrived with — one asset changing mode depending on how it got here. An
	// existing destination's mode wins, so a library deliberately locked down stays so.
	mode := os.FileMode(0o644)
	if fi, err := rt.Stat(finalName); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := rt.Chmod(tempName, mode); err != nil {
		return p.abandon(rt, err)
	}
	if err := rt.Rename(tempName, finalName); err != nil {
		// Unwound like every other failure that removes the temp. A rename can fail with
		// the destination held open — an editor, an on-access scanner — and without this
		// the empty <publisher>/<asset>/ Store created stays in the tree quarry walks.
		return p.abandon(rt, err)
	}
	return nil
}

// abandon drops the temp and the directories Store made for it, returning the error that
// caused it.
func (p *Pending) abandon(rt *os.Root, err error) error {
	rt.Remove(filepath.FromSlash(p.tempRel))
	pruneEmptyParents(rt, p.dirRel)
	return err
}

// Discard removes the pending bytes, and the directories Store created for them if the
// removal leaves them empty. Callers use it whenever a check fails, so a rejected body
// never reaches a real cache path.
func (p *Pending) Discard() error {
	rt, err := os.OpenRoot(p.root)
	if err != nil {
		return err
	}
	defer rt.Close()
	err = rt.Remove(filepath.FromSlash(p.tempRel))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pruneEmptyParents(rt, p.dirRel)
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
func VerifyDeep(ctx context.Context, root, rel, wantSHA string) bool {
	sha, _, err := Hash(ctx, root, rel)
	return err == nil && sha == wantSHA
}

// Hash returns a cached file's digest and size, for adopting a file the tool did not
// download itself.
//
// It takes a context for the same reason Scan and SweepTemps do. A single package reaches
// 23 GB, so one call is minutes of reading, and main's signal handler has already taken
// SIGINT's default action away for the life of the run: without this a Ctrl-C during
// `sync --verify` is ignored until the whole file is read, and the second and third do
// nothing either.
func Hash(ctx context.Context, root, rel string) (string, int64, error) {
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
	size, err := io.Copy(h, &ctxReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// ctxReader ends a long read when the run does. The check is per Read rather than per
// byte, so the granularity is io.Copy's buffer rather than the file.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
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
//
// The walk goes through the root's own FS rather than over the path, because
// filepath.WalkDir begins with an Lstat and stops at anything that is not a directory:
// point library_path at a symlink — reasonable for a 75 GB mirror, and what a Windows
// junction is — and it visits the link, descends nothing, and returns an empty index.
// Every other operation here keeps working, since os.OpenRoot resolves the link like any
// other directory, so the failure is silent: adoption finds nothing and re-downloads the
// whole library, a delisted asset already on disk is reported unavailable, and abandoned
// temps are never reclaimed. fs.WalkDir stats "." through the FS instead, so the case
// cannot arise. A symlinked directory *inside* the library is still not descended, which
// is the behaviour the confinement rule depends on.
func Scan(ctx context.Context, root string) *Index {
	ix := &Index{root: root, byProduct: map[string][]Candidate{}}
	rt, err := os.OpenRoot(root)
	if err != nil {
		return ix
	}
	defer rt.Close()

	fs.WalkDir(rt.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		// A header parse per package over a 75 GB library is long enough that a run
		// cancelled here would otherwise go on reading for minutes after being told to
		// stop. A short index is safe: a candidate the walk never reached is one the
		// caller falls back to downloading, where the full set of guards applies.
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, packageExt) {
			return nil
		}
		m, err := descriptorAt(rt, p)
		if err != nil || m.ID == "" {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		// fs.WalkDir already yields a slash-separated path relative to the root, which
		// is the spelling a lockfile records.
		ix.byProduct[m.ID] = append(ix.byProduct[m.ID],
			Candidate{RelPath: p, Size: fi.Size(), Metadata: m})
		return nil
	})
	return ix
}

// descriptorAt reads one candidate's store descriptor through the root, so a package that
// is itself a symlink out of the library is refused rather than followed and indexed.
func descriptorAt(rt *os.Root, rel string) (unitypackage.Metadata, error) {
	f, err := rt.Open(filepath.FromSlash(rel))
	if err != nil {
		return unitypackage.Metadata{}, err
	}
	defer f.Close()
	return unitypackage.Read(f)
}

// FindOptions narrows what Find will hand back.
type FindOptions struct {
	// Prefer is the path a copy wins from when several files claim the product, so an
	// adopt that is really a no-op does not turn into a relocation conflict.
	Prefer string

	// Exclude is a path that is never a candidate: a file that just failed verification
	// is not one to adopt, however intact its descriptor still looks. It comes from the
	// lockfile, so it is resolved rather than compared as a string, and one that cannot be
	// resolved refuses every candidate rather than being ignored.
	Exclude string

	// Accept gates each candidate inside the selection rather than being applied to what
	// comes back; nil takes anything. The gates that can reject one are the caller's — the
	// size floor and the advertised version id — and applied afterwards they rejected the
	// copy this had already chosen while another that would have passed sat unexamined two
	// files along: a stale or truncated build at the derived path masked an intact copy and
	// the asset re-downloaded in full.
	Accept func(Candidate) bool
}

// Find returns a package whose own metadata claims the given product id. It answers from
// the scan rather than probing the derived path, because the whole point of adoption is a
// file that is not where the current layout would put it — after a rename, say.
//
// The options are a struct rather than positional parameters because Prefer and Exclude
// are both root-relative paths with opposite meanings: as two adjacent strings they
// compiled either way round, and the wrong way round skips the copy already in place and
// adopts the one the caller named as damaged — a wrong file entering through the one door
// that skips the download guards. The exclusion was also variadic while exactly one was
// ever passed, advertising a plural nothing exercised.
func (ix *Index) Find(productID string, opts FindOptions) (Candidate, bool) {
	preferRel, accept := opts.Prefer, opts.Accept
	var excludeRel []string
	if opts.Exclude != "" {
		excludeRel = []string{opts.Exclude}
	}
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
	if accept == nil {
		accept = func(Candidate) bool { return true }
	}
	var found []Candidate
	for _, c := range ix.byProduct[productID] {
		full, err := resolve(ix.root, c.RelPath)
		if err != nil || skip[full] {
			continue
		}
		if !accept(c) {
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
			// Unwound for the reason the rename below unwinds: a half-made destination
			// leaves an empty <publisher>/ in the tree quarry walks, and a relocation
			// failure is only ever reported as a per-asset warning.
			pruneEmptyParents(r, path.Dir(to))
			pruneEmptyParents(r, path.Dir(path.Dir(to)))
			return err
		}
	}
	if err := r.Rename(fromName, toName); err != nil {
		// Unwound the way Store and Commit unwind theirs. MkdirAll has already made the
		// destination's parents, and a rename fails with the source held open — an editor,
		// an on-access scanner, which on Windows is a refusal rather than a retry — so
		// without this every attempt leaves an empty <publisher>/<asset>/ in the tree
		// quarry walks, and the move is only ever reported as a per-asset warning.
		pruneEmptyParents(r, path.Dir(to))
		return err
	}
	pruneEmptyParents(r, path.Dir(from))
	return nil
}

// pruneEmptyParents removes directories the move emptied, walking up but never past the
// library root.
//
// It takes a root-relative path and acts through os.Root, so confinement is the
// filesystem's rather than a string's: the walk stops at "." whatever the user spelled
// the library as, and a parent reached through a symlink is refused rather than removed.
// Removing one would take out the link itself and leave the directory it pointed at, so a
// user who moved a publisher onto another disk would find their layout quietly rearranged.
func pruneEmptyParents(rt *os.Root, relDir string) {
	for rel := path.Clean(relDir); rel != "." && rel != "/" && !strings.HasPrefix(rel, ".."); rel = path.Dir(rel) {
		name := filepath.FromSlash(rel)
		f, err := rt.Open(name)
		if err != nil {
			return
		}
		entries, err := f.ReadDir(1)
		f.Close()
		// ReadDir(1) reports io.EOF for a directory with nothing in it, which is the one
		// case worth acting on; anything else leaves the directory alone.
		if len(entries) > 0 || (err != nil && err != io.EOF) {
			return
		}
		if err := rt.Remove(name); err != nil {
			return
		}
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
//
// It walks and deletes through the root for the reason Scan walks through it: over a path,
// a symlinked library_path stops the walk at the link and reclaims nothing, silently, on
// exactly the libraries big enough to be moved onto another disk.
func SweepTemps(ctx context.Context, root string, olderThan time.Time) (int, int64) {
	var count int
	var bytes int64
	rt, err := os.OpenRoot(root)
	if err != nil {
		return 0, 0
	}
	defer rt.Close()

	fs.WalkDir(rt.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		// Housekeeping, so a cancelled run stops here rather than finishing a walk of the
		// whole library first. Whatever is left is swept by the next run.
		if ctx.Err() != nil {
			return fs.SkipAll
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
		if rt.Remove(filepath.FromSlash(p)) == nil {
			count++
			bytes += fi.Size()
		}
		return nil
	})
	return count, bytes
}

// RemoveStale deletes a package this tool mirrored and is now replacing with another copy
// of the same asset, and prunes the directories the removal empties.
//
// productID is the asset whose copy this is meant to be, and the file's own descriptor has
// to agree before anything is unlinked. The path always came out of the lockfile, which
// used to be the whole of the argument — but that file is committed, hand-editable and
// merged across machines, and nothing refuses two entries naming one path. With asset A's
// cachePath pointing at asset B's package, B verified against its own entry and classified
// Unchanged, A's verify failed on size and downloaded, and the superseded-copy cleanup
// then unlinked B: a run that exits 0, a lockfile claiming B is mirrored with a digest at
// a path holding nothing, and B re-downloaded in full on the next run. Asking the bytes
// what they are costs one header parse on a file about to be deleted, and it makes the
// rule this function is named for something it enforces rather than something its callers
// promise.
//
// A package carrying no descriptor is removed: some genuinely have none, and refusing
// those would make them undeletable forever.
func RemoveStale(root, rel, productID string) error {
	r, name, err := rooted(root, rel)
	if err != nil {
		// A library that is not there yet has nothing to remove; anything else is real.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer r.Close()

	switch m, err := descriptorAt(r, filepath.ToSlash(name)); {
	case err == nil && m.ID != "" && productID != "" && m.ID != productID:
		return fmt.Errorf("refusing to remove %s: it is product %s, not %s", rel, m.ID, productID)
	case err != nil && !errors.Is(err, unitypackage.ErrNoMetadata):
		// Unreadable, or not a package at all. Either way it is not this asset's
		// superseded copy, and the run has no business deleting it.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("refusing to remove %s: %w", rel, err)
	}

	if err := r.Remove(name); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pruneEmptyParents(r, path.Dir(filepath.ToSlash(name)))
	return nil
}
