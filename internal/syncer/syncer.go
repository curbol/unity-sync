// Package syncer orchestrates a run: enumerate, classify, download the delta, and
// rewrite the lockfile. It owns the checks that need both the stored bytes and the
// enumeration metadata — the store layer sees only responses, the cache layer only bytes.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/curbol/unity-sync/internal/cache"
	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
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

	report := Report{Unknown: opts.Manifest.UnknownIDs(owned)}

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
	var mu sync.Mutex

	// Classify everything selected, then fetch what needs fetching.
	var pending []Result
	for _, a := range owned {
		if !selected(a, opts) {
			continue
		}
		prevKey, prev, hasPrev := prior.FindByAssetID(a.ID)
		_ = prevKey
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
		adoptable := func() bool {
			found, foundOK = cache.Locate(opts.LibraryRoot, a.ID, derived)
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
			if ctx.Err() != nil {
				res.Err = ctx.Err()
				done[i] = res
				return
			}
			opts.Progress(fmt.Sprintf("fetching %s (%s)", res.Asset.Name, humanBytes(res.Asset.AdvertisedSize)))
			r, warning, err := download(ctx, s, opts, res.Asset)
			res.Warning, res.Err = warning, err
			if err == nil {
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
// committing it.
func download(ctx context.Context, s Store, opts Options, a model.Asset) (resolution, string, error) {
	dl, err := s.Fetch(ctx, a.ID)
	if err != nil {
		return resolution{}, "", err
	}
	defer dl.Body.Close()

	pending, err := cache.Store(opts.LibraryRoot, a.PublisherSlug(), a.Slug(), dl.Body)
	if err != nil {
		return resolution{}, "", err
	}

	meta, metaErr := unitypackage.ReadFile(pending.TempPath())
	switch {
	case metaErr != nil && !errors.Is(metaErr, unitypackage.ErrNoMetadata):
		pending.Discard()
		return resolution{}, "", fmt.Errorf("%s: %w", a.Name, metaErr)
	case metaErr == nil && meta.ID != a.ID:
		pending.Discard()
		return resolution{}, "", fmt.Errorf("%s: the store served product %s, not %s", a.Name, meta.ID, a.ID)
	}

	var warning string
	if metaErr != nil {
		warning = fmt.Sprintf("%s: package carries no store metadata, so later checks fall back to size alone", a.Name)
	}

	if belowFloor(pending.Size, a.AdvertisedSize) {
		// A short body is either a truncated transfer or a republish that moved the
		// advertised size out from under us. One re-read of this product settles it.
		if republished(ctx, s, a) {
			pending.Discard()
			return resolution{}, "", fmt.Errorf("%s: republished mid-download; the next run will fetch the new build", a.Name)
		}
		pending.Discard()
		return resolution{}, "", fmt.Errorf("%s: received %d bytes against an advertised %d; body ended early",
			a.Name, pending.Size, a.AdvertisedSize)
	}
	if a.AdvertisedSize > 0 && (pending.Size > a.AdvertisedSize || pending.Size < a.AdvertisedSize-64) {
		warning = fmt.Sprintf("%s: received %d bytes, advertised %d", a.Name, pending.Size, a.AdvertisedSize)
	}

	if err := pending.Commit(); err != nil {
		return resolution{}, warning, err
	}
	return resolution{
		cachePath:          pending.RelPath,
		sha:                pending.SHA256,
		size:               pending.Size,
		resolvedVersionID:  a.Version.ID,
		deliveredVersionID: meta.VersionID,
		downloadedAt:       opts.Now().UTC().Format(time.RFC3339),
		storeFilename:      dl.Filename,
	}, warning, nil
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
