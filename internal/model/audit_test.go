package model_test

import (
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/model"
)

// Failure models the identity rules must keep pinned. A slug is a directory name, so a
// slug this package will happily return but the filesystem will not accept is a download
// that fails on one platform and nowhere else.

// Windows refuses to create a file or directory named for a device. PublisherSlug carries
// no id suffix, so it is the one derived segment that can come out as a bare word: a
// publisher called "Con" would fail every one of its assets on every Windows run, with an
// MkdirAll error rather than anything naming the cause, and a failed download fails only
// its asset — so the run would look like a partial success forever.
func TestPublisherSlugNeverReturnsAWindowsReservedName(t *testing.T) {
	for _, name := range []string{
		"CON", "Con", "con", "PRN", "Aux", "nul",
		"COM1", "com9", "LPT1", "lpt9",
		// The folding gets there from names that are not reserved as written: outer
		// punctuation is trimmed and every non-alphanumeric run collapses.
		"Con!", "-AUX-", "nul.", "  PRN  ", "(com1)",
	} {
		a := model.Asset{Publisher: model.Publisher{ID: "4242", Name: name}}
		got := a.PublisherSlug()
		if model.ReservedSegment(got) {
			t.Errorf("PublisherSlug(%q) = %q, which Windows reserves for a device", name, got)
		}
		if got != "publisher-4242" {
			t.Errorf("PublisherSlug(%q) = %q, want the id fallback", name, got)
		}
	}
}

// Slug appends the product id, so it can never be a bare reserved word. Pinned because
// dropping that suffix would silently reintroduce the case above on the asset segment,
// where there is no fallback at all.
func TestSlugIsNeverReservedBecauseOfItsIdSuffix(t *testing.T) {
	for _, name := range []string{"CON", "aux", "com1", "nul"} {
		a := model.Asset{ID: "115488", Name: name}
		if got := a.Slug(); model.ReservedSegment(got) {
			t.Errorf("Slug(%q) = %q, which Windows reserves for a device", name, got)
		}
	}
}

// Windows matches a device name before the first dot, so the extension does not rescue it.
func TestReservedSegmentIgnoresAnExtension(t *testing.T) {
	for _, s := range []string{"con", "CON.unitypackage", "nul.txt", "aux."} {
		if !model.ReservedSegment(s) {
			t.Errorf("ReservedSegment(%q) = false; Windows matches the name before the dot", s)
		}
	}
	for _, s := range []string{"console", "con-115488", "context.unitypackage", "publisher-99592"} {
		if model.ReservedSegment(s) {
			t.Errorf("ReservedSegment(%q) = true; only the exact device names are reserved", s)
		}
	}
}

// Both slugs fall back rather than yield an empty segment, and the ASCII inputs that fold
// away take a different branch of slugify than the non-Latin ones the other tests use:
// these never reach the non-ASCII case at all. An empty segment collapses the three-deep
// layout and empties quarry's pack facet.
func TestBothSlugsFallBackForEveryNameThatFoldsAway(t *testing.T) {
	for _, name := range []string{"", "   ", "!!!", "---", "...", "@#$%", "\t\n"} {
		asset := model.Asset{ID: "115488", Name: name}
		if got := asset.Slug(); got != "115488" {
			t.Errorf("Slug(%q) = %q, want the bare id", name, got)
		}
		pub := model.Asset{Publisher: model.Publisher{ID: "99592", Name: name}}
		if got := pub.PublisherSlug(); got != "publisher-99592" {
			t.Errorf("PublisherSlug(%q) = %q, want the id fallback", name, got)
		}
	}
}

// A publisher with neither a usable name nor an id still has to produce a segment: the
// alternative is an empty one, which is the thing every fallback here exists to avoid.
func TestPublisherSlugFallsBackAgainWhenThereIsNoIdEither(t *testing.T) {
	for _, name := range []string{"", "CON", "日本語"} {
		a := model.Asset{Publisher: model.Publisher{Name: name}}
		got := a.PublisherSlug()
		if got == "" || model.ReservedSegment(got) || strings.ContainsAny(got, `/\`) {
			t.Errorf("PublisherSlug(%q) = %q, which is not a usable path segment", name, got)
		}
	}
}
