package fixtures_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/fixtures"
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
