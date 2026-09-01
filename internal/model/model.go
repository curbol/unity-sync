// Package model holds the domain types shared across unity-sync and, more importantly,
// the identity rules everything else depends on: which of the store's several ids is
// the real one, and how an asset's slug is derived from it.
package model

import (
	"regexp"
	"strings"
	"unicode"
)

// State is the store's lifecycle value for a product. Deprecated assets still download;
// disabled ones answer 404, so they are owned but not fetchable.
type State string

const (
	StatePublished  State = "published"
	StateDeprecated State = "deprecated"
	StateDisabled   State = "disabled"
)

// Downloadable reports whether the store will serve bytes for an asset in this state.
// An unrecognised state is treated as downloadable: the store adding a value must not
// make a whole library unmirrorable, and a wrong guess surfaces as a loud 404.
func (s State) Downloadable() bool { return s != StateDisabled }

// Publisher is the store's publisher record. Only the id is stable; the name can change
// and is used for display and for the cache's vendor directory.
type Publisher struct {
	ID   string
	Name string
}

// Version is one published build of an asset. ID is what a diff compares; Name is the
// publisher's display string and is not required to be orderly or even to change.
type Version struct {
	ID            string
	Name          string
	PublishedDate string
}

// Asset is one owned Asset Store product as the enumeration reports it.
//
// ID is the *store product id* — the value /api/downloads/{id} takes and the value the
// package's own metadata carries. The API also returns a `productId` field holding a
// different 12-digit number that no endpoint here accepts; this type deliberately does
// not carry it, so it cannot be reached for by mistake.
type Asset struct {
	ID        string
	Name      string
	State     State
	Publisher Publisher
	Version   Version

	// AdvertisedSize is the store's `downloadSize`. It arrives as a JSON string and is
	// only approximate — measured deltas run 0-16 bytes above the bytes delivered — so
	// it bounds a transfer but never checksums one.
	AdvertisedSize int64

	// ThumbnailURL is protocol-relative as the store returns it (`//host/path`).
	ThumbnailURL string
}

// Slug is the asset's key in the lockfile and its directory name in the cache. The id
// suffix keeps it unique and stable across publisher renames; the name prefix keeps
// lockfile diffs and the cache tree readable. The slug is not the identity — a rename
// changes it — so everything that must survive a rename matches on ID instead.
func (a Asset) Slug() string {
	base := slugify(a.Name)
	if base == "" {
		return a.ID
	}
	return base + "-" + a.ID
}

// PublisherSlug is the cache's vendor directory. It carries no id suffix, so the
// directory reads as a name and quarry's vendor facet stays legible. Two names cannot be
// used as they stand, and both fall back to the id: one written entirely in a non-Latin
// script folds to nothing, and an empty path segment would collapse the layout to two
// components and empty quarry's pack facet; one that folds to a name Windows reserves
// cannot be created there at all.
//
// Slug needs neither fallback for the reserved case, because its id suffix means it can
// never be a bare reserved word.
func (a Asset) PublisherSlug() string {
	if s := slugify(a.Publisher.Name); s != "" && !ReservedSegment(s) {
		return s
	}
	if a.Publisher.ID != "" {
		return "publisher-" + a.Publisher.ID
	}
	return "publisher-unknown"
}

// reservedSegments are the names Windows refuses as a path component, in any case and
// with any extension. A publisher called "Con", or anything that folds to it, would
// otherwise derive a directory MkdirAll cannot create, failing that asset on every
// Windows run and on no other platform.
var reservedSegments = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// ReservedSegment reports whether s names a Windows device rather than a file. Windows
// matches these before the first dot, so "con.unitypackage" is reserved too.
//
// It is exported because a slug has to be a usable directory name, and cache enforces
// that same rule at the filesystem boundary for anything that reaches it another way.
func ReservedSegment(s string) bool {
	base, _, _ := strings.Cut(s, ".")
	return reservedSegments[strings.ToLower(base)]
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases ASCII and turns everything else into a separator, so runs of
// non-ASCII collapse to a single hyphen rather than being transliterated: "Bézier Path
// Creator" becomes "b-zier-path-creator", which is also what the store itself does. A name
// with no ASCII at all therefore slugifies to the empty string, which is why callers need
// a fallback.
func slugify(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < unicode.MaxASCII:
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
}
