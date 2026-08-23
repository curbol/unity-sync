// Package syncer orchestrates a run: enumerate, classify, download the delta, and
// rewrite the lockfile. It owns the checks that need both the stored bytes and the
// enumeration metadata — the store layer sees only responses, the cache layer only bytes.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/retry"
	"github.com/curbol/unity-sync/internal/store"
	"github.com/curbol/unity-sync/internal/unitypackage"
)

// ErrEmptyLibrary guards the committed record against a well-formed but wrong
// enumeration. A session whose active org differs returns a legitimately different owned
// set, and the lockfile lives in someone's project.
var ErrEmptyLibrary = errors.New("the store reported no owned assets while the lockfile holds entries; " +
	"refusing to treat that as the truth (check which Unity organisation the session belongs to)")

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

// NeedsFetch reports whether a class means bytes must come off the network.
func (c Class) NeedsFetch() bool {
	switch c {
	case New, Changed, DownloadNow, CacheMissing:
		return true
	}
	return false
}

// classify is pure. The probes are injected so it stays that way: cacheOK is the cheap
// on-disk check for the prior resolution, and adoptable reports a matching file found by
// scanning.
func classify(a model.Asset, prior lockfile.Entry, hasPrior bool, cacheOK, adoptable func() bool) Class {
	resolved := hasPrior && prior.Tracked

	if !a.State.Downloadable() {
		// A copy already mirrored stays usable after the store delists the asset; only
		// one we do not have is a problem worth reporting.
		if resolved && cacheOK() {
			return Unchanged
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
	OnlyGlob    string
	DryRun      bool
	FullVerify  bool
	Concurrency int
	Now         func() time.Time
	Progress    func(string)

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
}

// Report is what a run produced.
type Report struct {
	// Owned is every asset the account holds, not only the selected ones, so a run with an
	// empty allowlist can still say what there is to choose from.
	Owned int

	Results  []Result
	Removed  []lockfile.Entry
	Unknown  []manifest.Entry
	Swept    int
	Freed    int64
	Lockfile lockfile.Lockfile

	// Retryable counts failures a later run might fix. A permanently gone asset is
	// reported but does not make the run exit non-zero, or one dead asset would fail
	// every future run forever.
	Retryable int
	Permanent int
}

// Failed reports whether the run should exit non-zero.
func (r Report) Failed() bool { return r.Retryable > 0 }

// resolution is what a successful fetch or adoption produced.
type resolution struct {
	cachePath          string
	sha                string
	size               int64
	resolvedVersionID  string
	deliveredVersionID string
	downloadedAt       string
	storeFilename      string
}

// Run executes a sync, or a status when DryRun. It returns the report even alongside an
// error, so a caller can show what did happen.
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
		if _, err := filepath.Match(opts.OnlyGlob, ""); err != nil {
			return Report{}, fmt.Errorf("bad --only pattern %q: %w", opts.OnlyGlob, err)
		}
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
		n, freed, err := cache.SweepTemps(opts.LibraryRoot, started)
		if err != nil {
			return report, err
		}
		report.Swept, report.Freed = n, freed
		if n > 0 {
			opts.Progress(fmt.Sprintf("reclaimed %d abandoned download(s), %s", n, humanBytes(freed)))
		}
	}

	resolutions := map[string]resolution{}
	// priorPaths remembers where each asset's bytes used to live, so a download that lands
	// somewhere else can clean up after itself.
	priorPaths := map[string]string{}
	var mu sync.Mutex

	// Classify everything selected, then fetch what needs fetching.
	var pending []Result
	for _, a := range owned {
		if !selected(a, opts) {
			continue
		}
		_, prev, hasPrev := prior.FindByAssetID(a.ID)
		derived := cache.RelPath(a.PublisherSlug(), a.Slug())

		cacheOK := func() bool {
			if prev.CachePath == "" {
				return false
			}
			if opts.FullVerify {
				return cache.VerifyDeep(opts.LibraryRoot, prev.CachePath, prev.SHA256)
			}
			return cache.Verify(opts.LibraryRoot, prev.CachePath, prev.SizeBytes, prev.DeliveredVersionID)
		}
		var found cache.Candidate
		var foundOK bool
		// excludeRel is set when a recorded file exists but failed verification. Adoption
		// must not reach for that same file: a truncation or a mid-file flip leaves the
		// descriptor intact and can clear the size floor, so the scan would re-adopt the
		// damaged bytes and record them as truth — the outcome every other guard exists to
		// prevent, arriving through the one door that skips them.
		var excludeRel string
		adoptable := func() bool {
			found, foundOK = cache.Locate(opts.LibraryRoot, a.ID, derived, excludeRel)
			if !foundOK {
				return false
			}
			// The same floor a download must clear. Without it, a truncated package
			// left in the library enters through the one door that skips the download
			// path and is then hashed and recorded as truth.
			if belowFloor(found.Size, a.AdvertisedSize) {
				foundOK = false
				return false
			}
			return found.Metadata.VersionID == a.Version.ID
		}

		if hasPrev && prev.Tracked {
			priorPaths[a.ID] = prev.CachePath
			if prev.CachePath != "" && !cacheOK() {
				excludeRel = prev.CachePath
			}
		}
		class := classify(a, prev, hasPrev, cacheOK, adoptable)
		res := Result{Asset: a, Class: class}

		switch class {
		case Adopted:
			if opts.DryRun {
				break
			}
			r, err := adopt(opts, a, found, derived)
			if err != nil {
				res.Err = err
				report.Retryable++
			} else {
				resolutions[a.ID] = r
			}
		case Unchanged:
			// Keep the prior resolution, but move it if the slug changed under it.
			if !opts.DryRun && hasPrev && prev.Tracked && prev.CachePath != derived {
				if err := cache.Relocate(opts.LibraryRoot, prev.CachePath, derived); err != nil {
					res.Warning = err.Error()
				} else {
					r := fromEntry(prev)
					r.cachePath = derived
					resolutions[a.ID] = r
				}
			}
		}
		if class.NeedsFetch() {
			pending = append(pending, res)
			continue
		}
		report.Results = append(report.Results, res)
	}

	if opts.DryRun {
		report.Results = append(report.Results, pending...)
		report.Lockfile = build(owned, prior, resolutions, opts, &report)
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
				res.Err = poolCtx.Err()
				done[i] = res
				return
			}
			opts.Progress(fmt.Sprintf("fetching %s (%s)", res.Asset.Name, humanBytes(res.Asset.AdvertisedSize)))
			var (
				r        resolution
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
				if old := priorPaths[res.Asset.ID]; old != "" && old != r.cachePath {
					if rmErr := cache.RemoveStale(opts.LibraryRoot, old); rmErr != nil {
						res.Warning = strings.TrimSpace(res.Warning + " " +
							fmt.Sprintf("(could not remove the superseded copy at %s: %v)", old, rmErr))
					}
				}
				mu.Lock()
				resolutions[res.Asset.ID] = r
				// Persisting per download is what keeps a run that dies at asset 90 of
				// 100 from discarding the 89 it already fetched.
				snapshot := build(owned, prior, resolutions, opts, nil)
				mu.Unlock()
				if err := lockfile.Save(lockPath, snapshot); err != nil {
					res.Err = fmt.Errorf("persisting progress: %w", err)
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
		if res.Err != nil {
			if errors.Is(res.Err, store.ErrNotDownloadable) {
				report.Permanent++
			} else {
				report.Retryable++
			}
		}
		report.Results = append(report.Results, res)
	}

	report.Lockfile = build(owned, prior, resolutions, opts, &report)
	if err := lockfile.Save(lockPath, report.Lockfile); err != nil {
		return report, err
	}
	return report, nil
}

// adopt records a package already on disk, relocating it to where the layout puts it so
// the cache does not drift and quarry's facets stay right.
func adopt(opts Options, a model.Asset, found cache.Candidate, derived string) (resolution, error) {
	if err := cache.Relocate(opts.LibraryRoot, found.RelPath, derived); err != nil {
		return resolution{}, err
	}
	sha, size, err := cache.Hash(opts.LibraryRoot, derived)
	if err != nil {
		return resolution{}, err
	}
	return resolution{
		cachePath: derived,
		sha:       sha,
		size:      size,
		// The bytes are verified to be this version, so the diff key is known even
		// though nothing was fetched. Leaving it empty would make every adopted asset
		// classify Changed on the next run and re-download.
		resolvedVersionID:  a.Version.ID,
		deliveredVersionID: found.Metadata.VersionID,
		// downloadedAt stays empty: the tool found the file, it did not fetch it.
	}, nil
}

// download fetches one asset and runs every semantic guard against the temp file before
// committing it. The bool reports whether anything was stored: a republish discovered
// mid-transfer is a warning, not a failure, and resolves nothing this run.
func download(ctx context.Context, s Store, opts Options, a model.Asset) (resolution, string, bool, error) {
	dl, err := s.Fetch(ctx, a.ID)
	if err != nil {
		return resolution{}, "", false, err
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
		return resolution{}, "", false, err
	}

	meta, metaErr := unitypackage.ReadFile(pending.TempPath())
	switch {
	case metaErr != nil && !errors.Is(metaErr, unitypackage.ErrNoMetadata):
		pending.Discard()
		return resolution{}, "", false, retry.Permanent(fmt.Errorf("%s: %w", a.Name, metaErr))
	case metaErr == nil && meta.ID != a.ID:
		pending.Discard()
		return resolution{}, "", false, retry.Permanent(
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

	if belowFloor(pending.Size, a.AdvertisedSize) {
		// A short body is either a truncated transfer or a republish that moved the
		// advertised size out from under us. One re-read of this product settles it.
		if republished(ctx, s, a) {
			pending.Discard()
			return resolution{}, fmt.Sprintf(
				"%s: republished mid-download; nothing stored, the next run will fetch the new build",
				a.Name), false, nil
		}
		pending.Discard()
		return resolution{}, "", false, fmt.Errorf("%s: received %d bytes against an advertised %d; body ended early",
			a.Name, pending.Size, a.AdvertisedSize)
	}
	if a.AdvertisedSize > 0 && (pending.Size > a.AdvertisedSize || pending.Size < a.AdvertisedSize-64) {
		warning = fmt.Sprintf("%s: received %d bytes, advertised %d", a.Name, pending.Size, a.AdvertisedSize)
	}

	if err := pending.Commit(); err != nil {
		return resolution{}, warning, false, err
	}
	return resolution{
		cachePath:          pending.RelPath,
		sha:                pending.SHA256,
		size:               pending.Size,
		resolvedVersionID:  a.Version.ID,
		deliveredVersionID: meta.VersionID,
		downloadedAt:       opts.Now().UTC().Format(time.RFC3339),
		storeFilename:      dl.Filename,
	}, warning, true, nil
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
func build(owned []model.Asset, prior lockfile.Lockfile, resolutions map[string]resolution,
	opts Options, report *Report) lockfile.Lockfile {

	out := lockfile.New()
	kept := map[string]bool{}

	for _, a := range owned {
		_, prev, hasPrev := prior.FindByAssetID(a.ID)
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
		if r, ok := resolutions[a.ID]; ok {
			e.Tracked = true
			e.ResolvedVersionID = r.resolvedVersionID
			e.DeliveredVersionID = r.deliveredVersionID
			e.SizeBytes = r.size
			e.SHA256 = r.sha
			e.CachePath = r.cachePath
			e.DownloadedAt = r.downloadedAt
			e.StoreFilename = r.storeFilename
		} else if hasPrev {
			e.Tracked = prev.Tracked
			e.ResolvedVersionID = prev.ResolvedVersionID
			e.DeliveredVersionID = prev.DeliveredVersionID
			e.SizeBytes = prev.SizeBytes
			e.SHA256 = prev.SHA256
			e.CachePath = prev.CachePath
			e.DownloadedAt = prev.DownloadedAt
			e.StoreFilename = prev.StoreFilename
		}

		// An asset the run did not resolve keeps its prior key, so the key and the
		// cachePath cannot drift apart between runs.
		key := a.Slug()
		if _, resolvedNow := resolutions[a.ID]; !resolvedNow && hasPrev {
			if prevKey, _, ok := prior.FindByAssetID(a.ID); ok {
				key = prevKey
			}
		}
		out.Assets[key] = e
	}

	if report != nil {
		for _, e := range prior.Assets {
			if !kept[e.AssetID] {
				report.Removed = append(report.Removed, e)
			}
		}
	}
	return out
}

func fromEntry(e lockfile.Entry) resolution {
	return resolution{
		cachePath:          e.CachePath,
		sha:                e.SHA256,
		size:               e.SizeBytes,
		resolvedVersionID:  e.ResolvedVersionID,
		deliveredVersionID: e.DeliveredVersionID,
		downloadedAt:       e.DownloadedAt,
		storeFilename:      e.StoreFilename,
	}
}

func selected(a model.Asset, opts Options) bool {
	if !opts.Selected[a.ID] {
		return false
	}
	if opts.OnlyGlob == "" {
		return true
	}
	ok, _ := filepath.Match(opts.OnlyGlob, a.Slug())
	return ok
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
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
		return humanBytes(read)
	}
	return fmt.Sprintf("%s of %s (%d%%)", humanBytes(read), humanBytes(total), read*100/total)
}
