// Package syncer orchestrates a run: enumerate, classify, download the delta, and
// rewrite the lockfile. It owns the checks that need both the stored bytes and the
// enumeration metadata — the store layer sees only responses, the cache layer only bytes.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/humanize"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/retry"
	"github.com/curbol/unity-sync/internal/store"
	"github.com/curbol/unity-sync/internal/unitypackage"
)

// sweepGrace is how far back the temp sweep's cutoff is moved so a concurrent run's
// in-flight transfer is spared even when it has stalled long enough to look abandoned.
//
// Derived from the stall window rather than chosen, because that window is the tool's own
// definition of how long a transfer may sit untouched and still be alive. At a flat
// minute it was half of it: a run whose body went quiet at T had its temp unlinked by a
// second run starting at T+70s and then resumed inside its own window, finished into the
// deleted file, and failed the whole transfer — permanently, because an opened-but-deleted
// temp reads as an unparseable package rather than a truncated one. Doubled rather than
// matched, so the margin does not depend on how long enumeration took.
const sweepGrace = 2 * store.DefaultStallTimeout

// ErrEmptyLibrary guards the committed record against a well-formed but wrong
// enumeration. A session whose active org differs returns a legitimately different owned
// set, and the lockfile lives in someone's project.
var ErrEmptyLibrary = errors.New("the store reported no owned assets while the lockfile holds entries; " +
	"refusing to treat that as the truth (check which Unity organisation the session belongs to)")

// scanLibrary, verifyDeep and saveLockfile are indirected so a test can observe the calls
// themselves. What has to hold about all three is not their result but when and how often
// they run, and only a counter notices: a scan per adopt probe is quadratic exactly when
// adoption matters most, a second deep verify of the same file doubles the cost of
// re-hashing 75 GB, and two saves overlapping means the write left the critical section
// that keeps the last writer's snapshot the newest one. Every such regression leaves each
// assertion about outcomes green.
//
// saveLockfile earns the indirection the hard way. Watching the directory for temp files
// was the previous answer, and it could not see the regression it existed for: with the
// save moved out from under mu the sampler simply never landed inside the window, so the
// test passed having observed nothing at all.
var (
	scanLibrary  = cache.Scan
	verifyDeep   = cache.VerifyDeep
	saveLockfile = lockfile.Save
)

// Class is one asset's outcome for this run.
type Class int

const (
	Unchanged Class = iota
	New
	Changed
	DownloadNow // owned, previously recorded but never mirrored, now selected
	CacheMissing
	Adopted
	Undownloadable
)

func (c Class) String() string {
	switch c {
	case New:
		return "new"
	case Changed:
		return "changed"
	case DownloadNow:
		return "download-now"
	case CacheMissing:
		return "cache-missing"
	case Adopted:
		return "adopted"
	case Undownloadable:
		return "undownloadable"
	default:
		return "unchanged"
	}
}

// Classes is every class, in the order a summary should list them: what a run has to do
// first, then what it did, then what it could not.
//
// Exported because the summary is one list and printing it was another, spelled out as
// string literals. String's default arm returns "unchanged", so an eighth class added
// without updating both places either reported as a no-op or dropped out of the per-class
// tally while still counting toward the total — a summary whose lines do not sum, with
// nothing failing.
func Classes() []Class {
	return []Class{New, Changed, DownloadNow, CacheMissing, Adopted, Unchanged, Undownloadable}
}

// needsFetch reports whether a class means bytes must come off the network.
func (c Class) needsFetch() bool {
	switch c {
	case New, Changed, DownloadNow, CacheMissing:
		return true
	}
	return false
}

// classify is pure. The probes are injected so it stays that way: cacheOK is the cheap
// on-disk check for the prior resolution, and adoptable reports a matching file found by
// scanning.
func classify(a model.Asset, prior lockfile.Entry, hasPrior bool, cacheOK, adoptable, adoptableInPlace func() bool) Class {
	resolved := hasPrior && prior.Tracked

	if !a.State.Downloadable() {
		// A copy already mirrored stays usable after the store delists the asset; only
		// one we do not have is a problem worth reporting. The lockfile is not the only
		// thing that knows we have it — a deleted lockfile, or a mirror made on another
		// machine, leaves the bytes on disk with nothing pointing at them — and this is
		// the one class where a download can never make up the difference, so the scan
		// is asked before the asset is called unavailable.
		if resolved && cacheOK() {
			return Unchanged
		}
		if adoptable() {
			return Adopted
		}
		return Undownloadable
	}
	if !resolved {
		if adoptable() {
			return Adopted
		}
		if hasPrior {
			return DownloadNow
		}
		return New
	}
	if prior.ResolvedVersionID != a.Version.ID {
		// A copy of the new build may already be in the library: one user-scoped library
		// serves several project-scoped lockfiles, so another project can have fetched it
		// already, and a branch switch can revert this project's record while the bytes
		// stay put. Restricted to a candidate already at the derived path, so this cannot
		// turn into a relocation the download path does not need.
		if adoptableInPlace() {
			return Adopted
		}
		return Changed
	}
	if !cacheOK() {
		if adoptable() {
			return Adopted
		}
		return CacheMissing
	}
	return Unchanged
}

// priorEntry is a prior lockfile entry together with the key it was filed under. The two
// travel as a pair because a rename re-keys an entry, so the key is not derivable from the
// entry: carrying a resolution forward means carrying its key forward with it, or the key
// and the cachePath drift apart between runs.
type priorEntry struct {
	key   string
	entry lockfile.Entry
}

// indexByAssetID indexes a lockfile by product id, which is the identity classification
// uses. Built once per run and shared, because the alternative is a full map walk per
// lookup: the classification loop needs one per owned asset, and the pool rebuilds the
// lockfile after every download inside the critical section, so a three-thousand-asset
// account paid for the walk twice over. One index also means one lookup decides both the
// entry and its key, which two separate walks over a hand-merged duplicate could answer
// differently — and lockfile.Load refuses such a file for that reason, since this index
// would otherwise keep whichever entry ranged last.
func indexByAssetID(lf lockfile.Lockfile) map[string]priorEntry {
	index := make(map[string]priorEntry, len(lf.Assets))
	for k, e := range lf.Assets {
		index[e.AssetID] = priorEntry{key: k, entry: e}
	}
	return index
}

// checkDistinctPaths refuses a prior lockfile in which two entries record the same
// cachePath.
//
// lockfile.Load already refuses two entries for one asset id; this is the same accident
// from the other side, and the committed file admits it by the same route — a merge that
// kept both sides of a rename, or a hand-edit after moving a file. With asset A's
// cachePath naming asset B's package, B verifies against its own entry and carries
// forward Unchanged, A's verify fails on size and downloads, and the superseded-copy
// cleanup then deletes B's bytes: a run that exits 0, a lockfile claiming B is mirrored
// with a digest at a path holding nothing, and B re-downloaded in full on every later run.
// RemoveStale now refuses that delete on the descriptor, but this catches the state before
// the run mutates anything at all, and names both entries rather than surfacing as a
// warning about a file the user never asked about.
//
// Compared with SamePath, never as strings: the whole reason the state is reachable is
// that the file is hand-edited, so the two spellings need not match.
func checkDistinctPaths(lf lockfile.Lockfile) error {
	type seen struct{ key, path string }
	var recorded []seen
	keys := make([]string, 0, len(lf.Assets))
	for k := range lf.Assets {
		keys = append(keys, k)
	}
	// Sorted, or which of the two entries gets named changes between runs over one file.
	sort.Strings(keys)
	for _, k := range keys {
		p := lf.Assets[k].CachePath
		if p == "" {
			continue
		}
		for _, prior := range recorded {
			if cache.SamePath(prior.path, p) {
				return fmt.Errorf("entries %q and %q both record the cache path %s; "+
					"delete whichever is stale, most likely the one whose key no longer "+
					"matches its name", prior.key, k, p)
			}
		}
		recorded = append(recorded, seen{key: k, path: p})
	}
	return nil
}

// Store is the part of the Asset Store client a run needs.
type Store interface {
	Enumerate(ctx context.Context) ([]model.Asset, error)
	Lookup(ctx context.Context, id string) (model.Asset, bool, error)
	Fetch(ctx context.Context, id string) (*store.Download, error)
}

// Options configures a run.
type Options struct {
	LibraryRoot string
	Selected    map[string]bool

	// OnlyGlob narrows a run further, matched against an asset's slug rather than
	// against Selected's ids or against any path.
	OnlyGlob string

	DryRun      bool
	FullVerify  bool
	Concurrency int
	Now         func() time.Time

	// Progress is called from every download goroutine as well as the main pass, so an
	// implementation that does more than one write needs its own synchronisation.
	Progress func(string)

	// Retry governs download attempts. Downloads get their own budget rather than the
	// API's: re-transferring a multi-gigabyte body is not the same kind of cheap as
	// re-issuing a 2 KB query.
	Retry retry.Policy

	// Manifest is consulted for reporting only; a run never writes it.
	Manifest manifest.Manifest
}

// Result is one asset's outcome.
type Result struct {
	Asset   model.Asset
	Class   Class
	Err     error
	Warning string

	// NotAttempted means the pool was already cancelled when this asset's turn came, so
	// nothing was tried. It is kept apart from Err because the cause is the run's, not
	// the asset's: one expired session would otherwise name every remaining asset as a
	// failure of its own and bury the one line the user can act on.
	NotAttempted bool

	// Unrecorded means the bytes are in place and the lockfile entry for them is not.
	// Kept apart from Err because the asset did not fail — the work was done, it is the
	// record of it that was lost, and the record is what the next run reads.
	Unrecorded bool
}

// Report is what a run produced.
type Report struct {
	// Owned is every asset the account holds, not only the selected ones, so a run with an
	// empty allowlist can still say what there is to choose from.
	Owned int

	// Selected is how many assets the allowlist and --only between them asked for, which
	// is not len(Results): an interrupted classification pass leaves the assets it never
	// reached without a Result at all. Counted separately rather than derived, because a
	// Result appended for each of them would carry Class's zero value and tally as
	// Unchanged — a no-op line for an asset the run never looked at.
	Selected int

	Results  []Result
	Removed  []lockfile.Entry
	Unknown  []manifest.Entry
	Swept    int
	Lockfile lockfile.Lockfile

	// Retryable counts failures a later run might fix. A permanently gone asset is
	// reported but does not make the run exit non-zero, or one dead asset would fail
	// every future run forever.
	Retryable int
	Permanent int

	// NotAttempted counts assets the pool was cancelled before reaching. Nothing about
	// them is known, so they are summarised rather than named — but the run did not do
	// what it was asked, so they still keep the exit status non-zero.
	NotAttempted int

	// Unrecorded counts assets whose bytes are in place but whose lockfile entry could
	// not be written. The lockfile is what the next run reads, so a lost write means the
	// work was done and will be done again: a relocation the record does not know about
	// classifies CacheMissing and re-downloads in full, and a run that reported this as a
	// warning alone exited 0 while the library it had just rearranged became unreadable
	// to the next run. Counted apart from Retryable because the asset did not fail, and
	// counted at all because all three places that persist have to agree — two of them
	// used to warn and exit 0 while the third called it a failure.
	Unrecorded int
}

// Failed reports whether the run should exit non-zero.
func (r Report) Failed() bool { return r.Retryable > 0 || r.NotAttempted > 0 || r.Unrecorded > 0 }

// Run executes a sync, or a status when DryRun. It returns the report even alongside an
// error, so a caller can show what did happen.
//
// prior must hold at most one entry per asset id. lockfile.Load enforces that; a caller
// building one by hand and leaving two entries for one asset gets one of them at random,
// because entries are found by walking the map.
func Run(ctx context.Context, s Store, prior lockfile.Lockfile, lockPath string, opts Options) (Report, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Progress == nil {
		opts.Progress = func(string) {}
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Retry.Attempts < 1 {
		opts.Retry = retry.Policy{Attempts: 2, Base: 2 * time.Second}
	}
	if opts.OnlyGlob != "" {
		if _, err := path.Match(opts.OnlyGlob, ""); err != nil {
			return Report{}, fmt.Errorf("bad --only pattern %q: %w", opts.OnlyGlob, err)
		}
	}
	// Before the enumeration, and so before the sweep and everything after it: this is a
	// state the run cannot act safely on, and the point of catching it is to refuse while
	// nothing has been moved or deleted yet.
	if err := checkDistinctPaths(prior); err != nil {
		return Report{}, err
	}
	started := opts.Now()

	opts.Progress("enumerating owned assets…")
	owned, err := s.Enumerate(ctx)
	if err != nil {
		return Report{}, err
	}
	if len(owned) == 0 && len(prior.Assets) > 0 {
		return Report{}, ErrEmptyLibrary
	}

	report := Report{Owned: len(owned), Unknown: opts.Manifest.UnknownIDs(owned)}

	// Sweeping before classification matters: an abandoned partial left in the tree is
	// otherwise a candidate the adopt scan could reach.
	if !opts.DryRun {
		// The cutoff is backdated because started is captured before enumeration, which is
		// several round trips. A concurrent run whose transfer has stalled has not touched
		// its temp since before this run began, and sweeping it kills a live download.
		n, freed := cache.SweepTemps(ctx, opts.LibraryRoot, started.Add(-sweepGrace))
		report.Swept = n
		if n > 0 {
			opts.Progress(fmt.Sprintf("reclaimed %d abandoned download(s), %s", n, humanize.Bytes(freed)))
		}
		// The lockfile's own temps, which land in the directory the user commits. Save
		// unlinks its own on every error path, but a kill between the create and the
		// rename cannot, and Save runs once per resolved asset. Same backdated cutoff, so
		// a concurrent run's in-flight write survives.
		lockfile.SweepTemps(filepath.Dir(lockPath), started.Add(-sweepGrace))
	}

	// One index for the whole run: the classification loop below and every build() the
	// pool triggers read it, and it is never written after this point.
	priorByID := indexByAssetID(prior)

	resolutions := map[string]lockfile.Resolution{}
	// priorPaths remembers where each asset's bytes used to live, so a download that lands
	// somewhere else can clean up after itself.
	priorPaths := map[string]string{}
	var mu sync.Mutex

	// persist records one resolved asset and rewrites the lockfile, both under mu. The two
	// steps are one critical section rather than two statements a later edit can separate:
	// with the write outside the lock, two goroutines reach the rename in the order
	// opposite to how they built their snapshots, and the older one wins — losing exactly
	// the record this write exists to keep.
	//
	// Every pass that mutates the library calls it, not only the downloads. The
	// classification pass relocates and deletes whole packages before anything is
	// persisted, so a run that adopts and fetches nothing would otherwise ride entirely on
	// the closing save: lose that one write and the library has moved while the lockfile
	// still describes where the file used to be, with a digest that no longer matches the
	// bytes at the path it names. Nothing but --verify ever looks again.
	persist := func(assetID string, r lockfile.Resolution) error {
		mu.Lock()
		defer mu.Unlock()
		resolutions[assetID] = r
		return saveLockfile(lockPath, build(owned, prior, priorByID, resolutions, nil))
	}

	// One scan of the library serves every adopt probe below, built on first use and only
	// from this loop, which is sequential. Scanning eagerly would make the ordinary run —
	// where everything is already current and nothing asks — pay for a walk it never uses;
	// scanning per probe would walk the whole tree once per asset.
	var library *cache.Index
	scan := func() *cache.Index {
		if library == nil {
			library = scanLibrary(ctx, opts.LibraryRoot)
		}
		return library
	}

	// Classify everything selected, then fetch what needs fetching.
	var pending []Result
	for i, a := range owned {
		// This pass hashes, relocates and deletes whole packages, and under --verify it
		// re-reads the entire library, so it is minutes of work on a large mirror.
		// Without this check a run told to stop keeps going — and keeps mutating — while
		// main's signal handler has already taken SIGINT's default action away, so the
		// second and third Ctrl-C do nothing either.
		//
		// Break rather than return: the tail still builds and saves the lockfile, so the
		// adoptions and relocations already performed are recorded rather than left on
		// disk with nothing pointing at them.
		if ctx.Err() != nil {
			for _, rest := range owned[i:] {
				if selected(rest, opts) {
					report.Selected++
					report.NotAttempted++
				}
			}
			break
		}
		if !selected(a, opts) {
			continue
		}
		report.Selected++
		recorded, hasPrev := priorByID[a.ID]
		prev := recorded.entry
		derived := cache.RelPath(a.PublisherSlug(), a.Slug())

		// Memoized: classify calls this, and so does the excludeRel decision below. Under
		// --verify each call is a full re-hash, so letting it run twice would double the
		// cost of verifying a 75 GB library.
		cacheOK := memoize(func() bool {
			switch {
			case prev.CachePath == "":
				return false
			case opts.FullVerify:
				return verifyDeep(ctx, opts.LibraryRoot, prev.CachePath, prev.SHA256)
			default:
				return cache.Verify(opts.LibraryRoot, prev.CachePath, prev.SizeBytes, prev.DeliveredVersionID)
			}
		})
		var found cache.Candidate
		// excludeRel is set when a recorded file exists but failed verification. Adoption
		// must not reach for that same file: a truncation or a mid-file flip leaves the
		// descriptor intact and can clear the size floor, so the scan would re-adopt the
		// damaged bytes and record them as truth — the outcome every other guard exists to
		// prevent, arriving through the one door that skips them.
		var excludeRel string
		adoptable := func() bool {
			// Decided here rather than before classify: classify short-circuits to
			// Changed without ever asking, and under --verify the probe is a full
			// re-hash of a file that is about to be replaced by the download.
			if hasPrev && prev.Tracked && prev.CachePath != "" && !cacheOK() {
				excludeRel = prev.CachePath
			}
			// The two gates go to Find rather than being applied to what it hands
			// back, because Find picks one candidate out of however many claim this
			// product. Checked afterwards, they rejected that one copy while a copy
			// that would have passed sat unexamined: a stale or truncated build at the
			// derived path wins the preference, masks an intact copy elsewhere in the
			// library, and the asset re-downloads in full — up to 23 GB, which is the
			// outcome adoption exists to avoid.
			//
			// The size floor is the same one a download must clear: without it, a
			// truncated package left in the library enters through the one door that
			// skips the download path and is then hashed and recorded as truth.
			c, ok := scan().Find(a.ID, derived, func(c cache.Candidate) bool {
				return !belowFloor(c.Size, a.AdvertisedSize) && c.Metadata.VersionID == a.Version.ID
			}, excludeRel)
			if !ok {
				return false
			}
			// Assigned only once the gates have passed, which is the precondition the
			// adopt call site reads it under.
			found = c
			return true
		}

		// adoptableInPlace is the probe the out-of-date branch gets, and it differs from
		// the one above in exactly one way: the candidate has to be sitting at the derived
		// path already.
		//
		// library_path is user-scoped while the manifest and lockfile are project-scoped,
		// so two projects share one library and keep separate lockfiles. Project A syncs
		// an asset to v2; project B, whose lockfile still records v1, classified Changed
		// and re-transferred the whole package over a byte-identical file already in place
		// carrying a v2 descriptor. A branch switch or a merge that reverts a lockfile
		// does the same thing. New, DownloadNow and CacheMissing all ask before fetching;
		// this was the one class that never did.
		//
		// Two things it deliberately does not do. It does not call adoptable, which sets
		// excludeRel behind !cacheOK() — under --verify that is a full re-hash of a file
		// about to be replaced, and it would exclude the very copy worth adopting. And it
		// does not permit a relocation: a Changed asset whose derived path holds a stale
		// copy downloads cleanly today because Commit overwrites, whereas an adopt that
		// had to move would meet Relocate's occupied-destination refusal and fail the
		// asset instead. Restricting to a candidate already in place keeps adopt's
		// Relocate a no-op and its RemoveStale unreached.
		//
		// The three gates still run inside Find, so this does not re-open the hole where a
		// rejected candidate masked an acceptable one: only the location is checked
		// outside, and Find already prefers the copy at derived.
		adoptableInPlace := func() bool {
			c, ok := scan().Find(a.ID, derived, func(c cache.Candidate) bool {
				return !belowFloor(c.Size, a.AdvertisedSize) && c.Metadata.VersionID == a.Version.ID
			})
			if !ok {
				return false
			}
			if !cache.SamePath(c.RelPath, derived) &&
				!cache.SameFile(opts.LibraryRoot, c.RelPath, derived) {
				return false
			}
			found = c
			return true
		}

		if hasPrev && prev.Tracked {
			priorPaths[a.ID] = prev.CachePath
		}
		class := classify(a, prev, hasPrev, cacheOK, adoptable, adoptableInPlace)
		res := Result{Asset: a, Class: class}

		switch class {
		case Adopted:
			if opts.DryRun {
				break
			}
			r, err := adopt(ctx, opts, a, found, derived, excludeRel)
			switch {
			case err != nil && ctx.Err() != nil && errors.Is(err, context.Canceled):
				// The run's outcome, not the asset's, and recorded the way the pool
				// records the assets it never reached. adopt hashes the whole package —
				// up to 23 GB, and context-aware precisely so an interrupt lands inside
				// it — so this is where a Ctrl-C during a large adoption comes back.
				// Counted as a failure it prints "failed: <name>: context canceled",
				// which reads as a corrupt package rather than as the interrupt the user
				// just typed.
				res.NotAttempted = true
				report.NotAttempted++
			case err != nil:
				res.Err = err
				report.Retryable++
			default:
				// Same rule the download branch applies: the entry's own prior copy is
				// superseded by what just landed at the derived path, and nothing else
				// will ever mention it again — the summary names only assets that left
				// the account, so an orphan here is one the tool made and never reports.
				res.Warning = joinWarning(res.Warning,
					removeSuperseded(opts.LibraryRoot, prev.CachePath, r.CachePath, a.ID))
				if err := persist(a.ID, r); err != nil {
					res.Warning = joinWarning(res.Warning,
						fmt.Sprintf("the adoption is on disk but could not be recorded: %v", err))
					res.Unrecorded = true
					report.Unrecorded++
				}
			}
		case Unchanged:
			// Keep the prior resolution, but move it if the slug changed under it.
			if !opts.DryRun && hasPrev && prev.Tracked && !cache.SamePath(prev.CachePath, derived) {
				if err := cache.Relocate(opts.LibraryRoot, prev.CachePath, derived); err != nil {
					res.Warning = err.Error()
				} else {
					r := prev.Resolution
					r.CachePath = derived
					if err := persist(a.ID, r); err != nil {
						res.Warning = joinWarning(res.Warning,
							fmt.Sprintf("the package moved but the move could not be recorded: %v", err))
						res.Unrecorded = true
						report.Unrecorded++
					}
				}
			}
		}
		if class.needsFetch() {
			pending = append(pending, res)
			continue
		}
		report.Results = append(report.Results, res)
	}

	if opts.DryRun {
		report.Results = append(report.Results, pending...)
		report.Lockfile = build(owned, prior, priorByID, resolutions, &report)
		return report, nil
	}

	// Downloads run bounded, and a failure fails its asset rather than the run: one
	// delisted or corrupt package must not stop a 75 GB mirror.
	// The pool stops early only for a run-fatal error: an expired session makes every
	// remaining download pointless, while one corrupt or delisted asset does not.
	poolCtx, cancelPool := context.WithCancel(ctx)
	defer cancelPool()

	var (
		wg   sync.WaitGroup
		sem  = make(chan struct{}, opts.Concurrency)
		done = make([]Result, len(pending))
	)
	for i, res := range pending {
		wg.Add(1)
		go func(i int, res Result) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if poolCtx.Err() != nil {
				res.NotAttempted = true
				done[i] = res
				return
			}
			opts.Progress(fmt.Sprintf("fetching %s (%s)", res.Asset.Name, humanize.Bytes(res.Asset.AdvertisedSize)))
			var (
				r        lockfile.Resolution
				warning  string
				resolved bool
			)
			// Retry wraps the fetch and the write together, so every attempt necessarily
			// opens a fresh temp file and a fresh hasher. Appending a retried response to
			// a partial one would survive every guard here and then be hashed and
			// recorded as its own truth.
			err := retry.Do(poolCtx, opts.Retry, func(int) error {
				var attemptErr error
				r, warning, resolved, attemptErr = download(poolCtx, s, opts, res.Asset)
				// Neither of these improves on a second attempt: the asset is gone, or
				// the session is.
				if errors.Is(attemptErr, store.ErrExpiredSession) ||
					errors.Is(attemptErr, store.ErrNotDownloadable) {
					return retry.Permanent(attemptErr)
				}
				return attemptErr
			})
			res.Warning, res.Err = warning, err
			if err == nil && !resolved {
				// A republish mid-download: nothing was stored, and the next run picks up
				// the new build. Not a failure.
				done[i] = res
				return
			}
			if err == nil {
				// A rename that also bumped the version downloads to the new derived
				// path, so the prior directory would otherwise be left holding a
				// superseded copy of the same asset.
				res.Warning = joinWarning(res.Warning,
					removeSuperseded(opts.LibraryRoot, priorPaths[res.Asset.ID], r.CachePath, res.Asset.ID))
				if err := persist(res.Asset.ID, r); err != nil {
					res.Warning = joinWarning(res.Warning,
						fmt.Sprintf("the package is in the cache but could not be recorded: %v", err))
					res.Unrecorded = true
				}
			}
			if errors.Is(res.Err, store.ErrExpiredSession) {
				cancelPool()
			}
			done[i] = res
		}(i, res)
	}
	wg.Wait()

	for _, res := range done {
		// A goroutine already inside retry.Do when the pool was cancelled comes back with
		// context.Canceled, which is the run's outcome rather than the asset's — the same
		// thing NotAttempted records for the ones that never started. Counted as a failure
		// it is printed as "failed: <name>: context canceled", so an expired session at
		// asset 5 of 300 buries its one actionable line under a Concurrency-1 pile of
		// them. The stall guard is the only other cancellation on this path and store
		// renames that to ErrStalled, so nothing real is being swallowed here.
		if poolCtx.Err() != nil && errors.Is(res.Err, context.Canceled) {
			res.Err, res.NotAttempted = nil, true
		}
		switch {
		case res.NotAttempted:
			report.NotAttempted++
		case errors.Is(res.Err, store.ErrNotDownloadable):
			report.Permanent++
		case res.Err != nil:
			report.Retryable++
		}
		// Not an arm of the switch above: a download can both succeed and go unrecorded,
		// which is the whole case this counts.
		if res.Unrecorded {
			report.Unrecorded++
		}
		report.Results = append(report.Results, res)
	}

	report.Lockfile = build(owned, prior, priorByID, resolutions, &report)
	if err := saveLockfile(lockPath, report.Lockfile); err != nil {
		return report, err
	}
	return report, nil
}

// adopt records a package already on disk, relocating it to where the layout puts it so
// the cache does not drift and quarry's facets stay right.
func adopt(ctx context.Context, opts Options, a model.Asset, found cache.Candidate, derived, damagedRel string) (lockfile.Resolution, error) {
	// Relocate refuses an occupied destination, which is what stops it certifying bytes
	// nothing checked. The one occupant that must not stop it is this asset's own recorded
	// copy after it failed verification: a download would rename straight over that file,
	// so a verified copy of the same package may replace it too. Without this the good copy
	// can never move in, nothing is resolved, and every later run repeats the refusal.
	if damagedRel != "" &&
		(cache.SamePath(damagedRel, derived) || cache.SameFile(opts.LibraryRoot, damagedRel, derived)) {
		if err := cache.RemoveStale(opts.LibraryRoot, damagedRel, a.ID); err != nil {
			return lockfile.Resolution{}, err
		}
	}
	if err := cache.Relocate(opts.LibraryRoot, found.RelPath, derived); err != nil {
		return lockfile.Resolution{}, err
	}
	sha, size, err := cache.Hash(ctx, opts.LibraryRoot, derived)
	if err != nil {
		return lockfile.Resolution{}, err
	}
	return lockfile.Resolution{
		Tracked:   true,
		CachePath: derived,
		SHA256:    sha,
		SizeBytes: size,
		// The bytes are verified to be this version, so the diff key is known even
		// though nothing was fetched. Leaving it empty would make every adopted asset
		// classify Changed on the next run and re-download.
		ResolvedVersionID:  a.Version.ID,
		DeliveredVersionID: found.Metadata.VersionID,
	}, nil
}

// download fetches one asset and runs every semantic guard against the temp file before
// committing it. The bool reports whether anything was stored: a republish discovered
// mid-transfer is a warning, not a failure, and resolves nothing this run.
func download(ctx context.Context, s Store, opts Options, a model.Asset) (lockfile.Resolution, string, bool, error) {
	dl, err := s.Fetch(ctx, a.ID)
	if err != nil {
		return lockfile.Resolution{}, "", false, err
	}
	defer dl.Body.Close()

	// A 23 GB package would otherwise report nothing between "fetching" and "done", so
	// progress is counted off the body as it streams rather than announced up front.
	body := &progressReader{
		r:     dl.Body,
		total: a.AdvertisedSize,
		report: func(read, total int64) {
			opts.Progress(fmt.Sprintf("  %s: %s", a.Name, progressLine(read, total)))
		},
	}
	pending, err := cache.Store(opts.LibraryRoot, a.PublisherSlug(), a.Slug(), body)
	if err != nil {
		return lockfile.Resolution{}, "", false, err
	}

	// The floor is asked before the descriptor, because the descriptor is the first ~350
	// bytes and a transfer that dropped inside them fails to parse as gzip at all. That
	// error is not ErrNoMetadata, so the switch below marks it permanent — and the
	// identical fault a few hundred bytes later reaches the floor instead, which returns
	// unmarked and gets the full retry budget. One transport failure, two outcomes,
	// decided only by where the connection happened to drop, with the early one reported
	// as "not a readable gzip stream" and blamed on the store. A short body's diagnosis
	// is "truncated", which is retryable and has its own discriminator; only a body long
	// enough to be a package has anything for the descriptor guards to say about it.
	if belowFloor(pending.Size, a.AdvertisedSize) {
		// A short body is either a truncated transfer or a republish that moved the
		// advertised size out from under us. One re-read of this product settles it.
		if republished(ctx, s, a) {
			pending.Discard()
			return lockfile.Resolution{}, fmt.Sprintf(
				"%s: republished mid-download; nothing stored, the next run will fetch the new build",
				a.Name), false, nil
		}
		pending.Discard()
		return lockfile.Resolution{}, "", false, fmt.Errorf("%s: received %d bytes against an advertised %d; body ended early",
			a.Name, pending.Size, a.AdvertisedSize)
	}

	meta, metaErr := unitypackage.ReadFile(pending.TempPath())
	switch {
	case metaErr != nil && !errors.Is(metaErr, unitypackage.ErrNoMetadata):
		pending.Discard()
		return lockfile.Resolution{}, "", false, retry.Permanent(fmt.Errorf("%s: %w", a.Name, metaErr))
	case metaErr == nil && meta.ID != a.ID:
		pending.Discard()
		return lockfile.Resolution{}, "", false, retry.Permanent(
			fmt.Errorf("%s: the store served product %s, not %s", a.Name, meta.ID, a.ID))
	}

	var warning string
	switch {
	case metaErr != nil:
		warning = fmt.Sprintf("%s: package carries no store metadata, so later checks fall back to size alone", a.Name)
	case meta.VersionID != a.Version.ID:
		// Steady state for a few products: the store advertises one build and serves
		// another. Both ids are recorded; the run says so once rather than silently
		// papering over the difference.
		warning = fmt.Sprintf("%s: store advertises version %s but served %s; both recorded",
			a.Name, a.Version.ID, meta.VersionID)
	}

	if a.AdvertisedSize > 0 && (pending.Size > a.AdvertisedSize || pending.Size < a.AdvertisedSize-64) {
		// A package can both be served at a different version than advertised and land
		// outside the window, and the version notice is the one the lockfile's two ids
		// need explaining.
		warning = joinWarning(warning,
			fmt.Sprintf("%s: received %d bytes, advertised %d", a.Name, pending.Size, a.AdvertisedSize))
	}

	if err := pending.Commit(); err != nil {
		return lockfile.Resolution{}, warning, false, err
	}
	return lockfile.Resolution{
		Tracked:            true,
		CachePath:          pending.RelPath,
		SHA256:             pending.SHA256,
		SizeBytes:          pending.Size,
		ResolvedVersionID:  a.Version.ID,
		DeliveredVersionID: meta.VersionID,
		DownloadedAt:       opts.Now().UTC().Format(time.RFC3339),
		StoreFilename:      dl.Filename,
	}, warning, true, nil
}

// removeSuperseded deletes the copy this asset's own new bytes replace, reporting what
// went wrong rather than failing the asset over housekeeping.
//
// The two paths are compared canonically, not as strings: old comes out of the lockfile,
// which is committed and hand-editable, so "./pub/a/a.unitypackage" and
// "pub/a/a.unitypackage" name one file. Comparing them raw makes the run delete the copy
// it just wrote and then record a digest for a path with nothing on it.
//
// productID goes to RemoveStale, which refuses a file whose own descriptor names a
// different asset. Nothing stops a hand-merged lockfile from pointing one entry's
// cachePath at another entry's package, and this is the path that would then delete it.
func removeSuperseded(root, old, current, productID string) string {
	// SameFile as well as SamePath: on a case-insensitive filesystem two spellings that
	// differ only in case are one file, which no canonical form collapses, and this is
	// about to delete.
	if old == "" || cache.SamePath(old, current) || cache.SameFile(root, old, current) {
		return ""
	}
	if err := cache.RemoveStale(root, old, productID); err != nil {
		return fmt.Sprintf("could not remove the superseded copy at %s: %v", old, err)
	}
	return ""
}

// joinWarning collects an asset's warnings into the single field Result carries. One asset
// can legitimately produce several: a version mismatch, a size outside the tight window,
// and a superseded copy that would not delete.
func joinWarning(existing, add string) string {
	switch {
	case add == "":
		return existing
	case existing == "":
		return add
	default:
		return existing + "; " + add
	}
}

// belowFloor is the hard short-body rule. The tolerance is absolute because the gap it
// forgives is a ceil-to-16 alignment artifact, and clamped so it cannot swallow a small
// package whole.
func belowFloor(received, advertised int64) bool {
	if advertised <= 0 {
		return false
	}
	allowance := int64(4096)
	if eighth := advertised / 8; eighth < allowance {
		allowance = eighth
	}
	return received < advertised-allowance
}

// republished reports whether the store now advertises something different from what the
// enumeration saw. Keying on "delivered id differs from advertised id" instead would be
// wrong: for some products that difference is a steady state, and the floor would be
// switched off permanently for exactly those.
func republished(ctx context.Context, s Store, a model.Asset) bool {
	fresh, ok, err := s.Lookup(ctx, a.ID)
	if err != nil || !ok {
		return false
	}
	return fresh.Version.ID != a.Version.ID || fresh.AdvertisedSize != a.AdvertisedSize
}

// build produces the new lockfile: every owned asset gets an entry, advertised fields are
// refreshed, and any resolution this run did not touch is carried forward verbatim.
// prior is still needed alongside index for the Removed pass, which walks entries by key
// rather than by id.
func build(owned []model.Asset, prior lockfile.Lockfile, index map[string]priorEntry,
	resolutions map[string]lockfile.Resolution, report *Report) lockfile.Lockfile {

	out := lockfile.New()
	kept := map[string]bool{}

	for _, a := range owned {
		prev, hasPrev := index[a.ID]
		if hasPrev {
			kept[a.ID] = true
		}
		e := lockfile.Entry{
			AssetID:        a.ID,
			Name:           a.Name,
			State:          string(a.State),
			Publisher:      lockfile.Publisher{ID: a.Publisher.ID, Name: a.Publisher.Name},
			Version:        lockfile.Version{ID: a.Version.ID, Name: a.Version.Name, PublishedDate: a.Version.PublishedDate},
			AdvertisedSize: a.AdvertisedSize,
		}
		r, resolvedNow := resolutions[a.ID]
		switch {
		case resolvedNow:
			e.Resolution = r
		case hasPrev:
			e.Resolution = prev.entry.Resolution
		}

		// An asset the run did not resolve keeps its prior key, so the key and the
		// cachePath cannot drift apart between runs.
		key := a.Slug()
		if !resolvedNow && hasPrev {
			key = prev.key
		}
		out.Assets[key] = e
	}

	if report != nil {
		for _, e := range prior.Assets {
			if !kept[e.AssetID] {
				report.Removed = append(report.Removed, e)
			}
		}
		// Map order otherwise, which would reorder the summary between two runs that
		// found the same thing.
		sort.Slice(report.Removed, func(i, j int) bool {
			if report.Removed[i].Name != report.Removed[j].Name {
				return report.Removed[i].Name < report.Removed[j].Name
			}
			return report.Removed[i].AssetID < report.Removed[j].AssetID
		})
	}
	return out
}

func selected(a model.Asset, opts Options) bool {
	if !opts.Selected[a.ID] {
		return false
	}
	if opts.OnlyGlob == "" {
		return true
	}
	// path.Match, not filepath.Match: the pattern is matched against a slug, and on
	// Windows filepath.Match reads a backslash as a separator rather than as an escape, so
	// the same --only would behave differently there.
	ok, _ := path.Match(opts.OnlyGlob, a.Slug())
	return ok
}

// progressReader reports how far a transfer has got, at most once a second, so a
// multi-gigabyte download is visibly alive without flooding the terminal.
type progressReader struct {
	r      io.Reader
	total  int64
	read   int64
	last   time.Time
	report func(read, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if now := time.Now(); now.Sub(p.last) >= time.Second {
		p.last = now
		p.report(p.read, p.total)
	}
	return n, err
}

func progressLine(read, total int64) string {
	if total <= 0 {
		return humanize.Bytes(read)
	}
	return fmt.Sprintf("%s of %s (%d%%)", humanize.Bytes(read), humanize.Bytes(total), read*100/total)
}

// memoize runs a probe at most once. The probes it wraps read or hash whole packages, so
// a caller asking twice is not merely redundant, it doubles the work.
func memoize(probe func() bool) func() bool {
	var done, result bool
	return func() bool {
		if !done {
			done, result = true, probe()
		}
		return result
	}
}
