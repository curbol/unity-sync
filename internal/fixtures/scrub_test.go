package fixtures_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/fixtures"
	"github.com/curbol/unity-sync/internal/store"
)

const oneRowCapture = `[{"data":{"searchMyAssets":{"total":1,"results":[
  {"id":"20066949412942","product":{"id":"115488","name":"Quick Outline"}}
]}}}]`

func TestScrubDropsTheEntitlementIdAndKeepsTheProduct(t *testing.T) {
	out, err := fixtures.Scrub([]byte(oneRowCapture))
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if strings.Contains(string(out), "20066949412942") {
		t.Error("scrubbed output still carries the entitlement id")
	}

	var batch []map[string]any
	if err := json.Unmarshal(out, &batch); err != nil {
		t.Fatalf("scrubbed output is not valid JSON: %v", err)
	}
	rows := batch[0]["data"].(map[string]any)["searchMyAssets"].(map[string]any)["results"].([]any)
	row := rows[0].(map[string]any)
	if _, present := row["id"]; present {
		t.Error("row still has an id field")
	}
	product, ok := row["product"].(map[string]any)
	if !ok {
		t.Fatal("row lost its product object")
	}
	if product["id"] != "115488" {
		t.Errorf("product id = %v, want 115488 (the product id must survive)", product["id"])
	}
}

// A fixture is meant to be shaped exactly like a response to the pinned query, so the
// image sizes a capture carries but SearchDocument never asks for come out. Nothing else
// checks this: the guard test only looks for account data, and the scrubber cannot be run
// end to end without raw captures, so a broken strip would reach testdata unnoticed.
func TestScrubDropsImageSizesThePinnedQueryDoesNotAskFor(t *testing.T) {
	capture := `[{"data":{"searchMyAssets":{"total":1,"results":[
  {"id":"20066949412942","product":{"id":"115488","mainImage":{
    "icon75":"//cdn/i75.png","icon":"//cdn/i.png","big":"//cdn/b.png",
    "small":"//cdn/s.png","facebook":"//cdn/f.png"}}}
]}}}]`
	out, err := fixtures.Scrub([]byte(capture))
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	var batch []map[string]any
	if err := json.Unmarshal(out, &batch); err != nil {
		t.Fatalf("scrubbed output is not valid JSON: %v", err)
	}
	rows := batch[0]["data"].(map[string]any)["searchMyAssets"].(map[string]any)["results"].([]any)
	img := rows[0].(map[string]any)["product"].(map[string]any)["mainImage"].(map[string]any)

	if img["icon75"] != "//cdn/i75.png" {
		t.Errorf("icon75 = %v, want it kept: it is the only image field the query asks for", img["icon75"])
	}
	for _, dropped := range []string{"icon", "big", "small", "facebook"} {
		if _, present := img[dropped]; present {
			t.Errorf("mainImage still carries %q, which the pinned query never requests", dropped)
		}
	}
}

// The terminator page has an empty results list. Scrubbing it must succeed, because it
// is a fixture the pagination tests need.
func TestScrubAcceptsAnEmptyResultsPage(t *testing.T) {
	if _, err := fixtures.Scrub([]byte(`[{"data":{"searchMyAssets":{"total":176,"results":[]}}}]`)); err != nil {
		t.Fatalf("Scrub on terminator page: %v", err)
	}
}

func TestScrubRefusesPayloadsItDoesNotUnderstand(t *testing.T) {
	for name, body := range map[string]string{
		"not an array":      `{"data":{}}`,
		"empty batch":       `[]`,
		"no data":           `[{"errors":[]}]`,
		"no searchMyAssets": `[{"data":{"product":{}}}]`,
		"no results field":  `[{"data":{"searchMyAssets":{"total":0}}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixtures.Scrub([]byte(body)); err == nil {
				t.Error("Scrub accepted a payload it cannot have scrubbed correctly")
			}
		})
	}
}

// The scrub is an allowlist, so a field the pinned query never asked for cannot reach a
// fixture whether or not anybody thought to name it. That matters because the only
// supported way to regenerate is to capture again from a signed-in session, and the
// natural capture is the storefront's own query, which asks for far more per row.
func TestScrubKeepsOnlyWhatThePinnedQueryAsksFor(t *testing.T) {
	capture := `[{"data":{"searchMyAssets":{"total":1,"results":[
	  {"id":"20066949412942","entitlementId":"999","product":{"id":"115488","name":"Quick Outline",
	   "orderId":"abc","productId":"123456789012","currentVersion":{"id":"905463","seatId":"s1"},
	   "publisher":{"id":"7","name":"Chris Nolet","email":"someone@example.com"}}}
	]}}}]`
	out, err := fixtures.Scrub([]byte(capture))
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	for _, unwanted := range []string{
		"entitlementId", "orderId", "seatId", "email", "someone@example.com",
		"20066949412942", "productId", "123456789012",
	} {
		if strings.Contains(string(out), unwanted) {
			t.Errorf("a field the pinned query never asked for survived the scrub: %q", unwanted)
		}
	}
	for _, wanted := range []string{"115488", "Quick Outline", "905463", "Chris Nolet"} {
		if !strings.Contains(string(out), wanted) {
			t.Errorf("the scrub dropped %q, which the pinned query does ask for", wanted)
		}
	}
}

// A row the projection does not recognise is the one row whose fields nobody has looked
// at, so it fails rather than passing through untouched.
func TestScrubRefusesAResultRowThatIsNotAnObject(t *testing.T) {
	bad := `[{"data":{"searchMyAssets":{"total":1,"results":["not an object"]}}}]`
	if _, err := fixtures.Scrub([]byte(bad)); err == nil {
		t.Error("Scrub accepted a result row that is not an object")
	}
}

// The projection has to reach the whole operation, not just the rows. A capture can carry
// an errors or extensions array beside data — the client parses exactly that shape — or a
// second root field beside searchMyAssets when the capture came from the storefront's own
// wider query. Those sit above the row projection, so nothing else in the scrub looks at
// them, and whatever they hold is committed verbatim.
func TestScrubDropsWhatSitsBesideTheRowsToo(t *testing.T) {
	raw := []byte(`[{
	  "data": {
	    "searchMyAssets": {"total": 1, "results": []},
	    "purchaseSummary": {"invoiceEmail": "someone@example.com", "orgId": "4242"}
	  },
	  "extensions": {"traceId": "abc-123"},
	  "errors": []
	}]`)

	out, err := fixtures.Scrub(raw)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	for _, gone := range []string{"purchaseSummary", "invoiceEmail", "example.com", "4242", "extensions", "traceId"} {
		if strings.Contains(string(out), gone) {
			t.Errorf("scrubbed output still carries %q:\n%s", gone, out)
		}
	}
	if !strings.Contains(string(out), "searchMyAssets") {
		t.Errorf("the scrub dropped the payload it exists to keep:\n%s", out)
	}
}

// The parser turns anything it does not recognise into a leaf, and prune keeps a leaf's
// value whole — so an unhandled construct in the pinned query widens the scrub instead of
// narrowing it. It has to refuse rather than guess.
func TestTheQueryParserRefusesConstructsItCannotProject(t *testing.T) {
	for _, doc := range []string{
		`query Q { searchMyAssets { mainImage @include(if: $x) { icon75 } } }`,
		`query Q { searchMyAssets { icon: icon75 } }`,
		`query Q { searchMyAssets { ...rowFields } }`,
	} {
		if _, err := fixtures.ParseSelectionSets(doc); err == nil {
			t.Errorf("the parser accepted %q; every field under it would be kept whole", doc)
		}
	}
	// The document actually in use still parses, or the check above is vacuous.
	if _, err := fixtures.ParseSelectionSets(store.SearchDocument); err != nil {
		t.Errorf("the pinned query no longer parses: %v", err)
	}
}
