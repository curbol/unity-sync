package model_test

import (
	"testing"

	"github.com/curbol/unity-sync/internal/model"
)

func TestSlugMatchesTheStoresOwnSlugForOrdinaryNames(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
	}{
		{"Quirky Series - Animals Mega Pack Vol.1", "137259", "quirky-series-animals-mega-pack-vol-1-137259"},
		{"Quick Outline", "115488", "quick-outline-115488"},
		{"RPG_Animations - Torch,Lantern,Enemy Attack,Hit,Magic", "309182",
			"rpg-animations-torch-lantern-enemy-attack-hit-magic-309182"},
		{"UI Toolkit Bundle 1", "262163", "ui-toolkit-bundle-1-262163"},
	} {
		a := model.Asset{ID: tc.id, Name: tc.name}
		if got := a.Slug(); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A rename changes the slug by construction. The test exists to pin that the id
// survives inside it, since the id is what classification actually matches on.
func TestSlugChangesOnRenameButKeepsTheId(t *testing.T) {
	before := model.Asset{ID: "309177", Name: "RPG_Animations - One Hand Base"}
	after := model.Asset{ID: "309177", Name: "Ultimate One Hand Locomotion"}
	if before.Slug() == after.Slug() {
		t.Fatal("a rename must change the slug")
	}
	for _, s := range []string{before.Slug(), after.Slug()} {
		if got := s[len(s)-len("309177"):]; got != "309177" {
			t.Errorf("slug %q does not end in the product id", s)
		}
	}
}

func TestSlugFallsBackToTheIdWhenTheNameFoldsAway(t *testing.T) {
	a := model.Asset{ID: "424242", Name: "日本語アセット"}
	if got := a.Slug(); got != "424242" {
		t.Errorf("Slug() = %q, want the bare id; an empty slug would be an unsafe path element", got)
	}
}

func TestPublisherSlugCarriesNoIdSuffixButFallsBackToOne(t *testing.T) {
	named := model.Asset{Publisher: model.Publisher{ID: "99592", Name: "DoubleL"}}
	if got := named.PublisherSlug(); got != "doublel" {
		t.Errorf("PublisherSlug() = %q, want %q — the vendor facet should read as a name", got, "doublel")
	}
	folded := model.Asset{Publisher: model.Publisher{ID: "12345", Name: "Кириллица"}}
	if got := folded.PublisherSlug(); got != "publisher-12345" {
		t.Errorf("PublisherSlug() = %q, want %q; an empty segment collapses the layout to two "+
			"components and empties quarry's pack facet", got, "publisher-12345")
	}
}

func TestSlugsAreUniquePerProductEvenWhenNamesCollide(t *testing.T) {
	a := model.Asset{ID: "111", Name: "Forest Pack"}
	b := model.Asset{ID: "222", Name: "Forest Pack!"}
	if a.Slug() == b.Slug() {
		t.Errorf("distinct products produced the same slug %q", a.Slug())
	}
}

func TestOnlyDisabledIsUndownloadable(t *testing.T) {
	for state, want := range map[model.State]bool{
		model.StatePublished:           true,
		model.StateDeprecated:          true, // deprecated assets still serve bytes
		model.StateDisabled:            false,
		model.State("retired-someday"): true, // an unknown state must not brick a library
	} {
		if got := state.Downloadable(); got != want {
			t.Errorf("State(%q).Downloadable() = %v, want %v", state, got, want)
		}
	}
}

// A partially non-ASCII name is the interesting case: the run of non-ASCII becomes one
// separator rather than disappearing, which is also what the store's own slug does.
func TestPartiallyNonASCIINamesCollapseRatherThanTransliterate(t *testing.T) {
	a := model.Asset{ID: "136082", Name: "Bézier Path Creator"}
	if got, want := a.Slug(), "b-zier-path-creator-136082"; got != want {
		t.Errorf("Slug() = %q, want %q", got, want)
	}
	// Whatever the folding does, the result must stay a single safe path element.
	for _, r := range a.Slug() {
		if r == '/' || r == '\\' || r < 0x20 {
			t.Fatalf("slug %q contains a path-unsafe rune", a.Slug())
		}
	}
}
