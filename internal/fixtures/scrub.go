// Package fixtures turns raw Asset Store captures into the PII-free JSON that the
// offline test suite runs against. Captures are never committed; the scrubbed output
// is. The account-identifying field the store returns is the per-row entitlement id,
// which nothing in unity-sync reads, so the scrub deletes the field outright rather
// than substituting a value — that also keeps a fixture shaped exactly like what the
// pinned query asks for.
package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// entitlementField is the per-row id the store returns alongside each product. It
// identifies the grant, not the asset, and no code path in this tool uses it.
const entitlementField = "id"

// unusedImageFields are image sizes the captures carry but the pinned query does not
// request. Dropping them keeps a fixture shaped exactly like a real response to the
// query the client actually sends.
var unusedImageFields = []string{"icon", "big", "small", "facebook"}

// Scrub rewrites one captured `searchMyAssets` batch response into fixture form,
// preserving key order-independent structure and stable indentation so the committed
// file diffs cleanly. It fails rather than guessing when the payload is not the batch
// shape the client actually parses.
func Scrub(raw []byte) ([]byte, error) {
	var batch []map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, fmt.Errorf("capture is not a GraphQL batch array: %w", err)
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("capture holds no operations")
	}
	for _, op := range batch {
		results, err := resultRows(op)
		if err != nil {
			return nil, err
		}
		for _, row := range results {
			m, ok := row.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("result row is %T, want object", row)
			}
			delete(m, entitlementField)
			if product, ok := m["product"].(map[string]any); ok {
				if img, ok := product["mainImage"].(map[string]any); ok {
					for _, f := range unusedImageFields {
						delete(img, f)
					}
				}
			}
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(batch); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// resultRows walks to searchMyAssets.results, tolerating the terminator page whose
// results list is empty but not a payload missing the path entirely.
func resultRows(op map[string]any) ([]any, error) {
	data, ok := op["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("operation has no data object")
	}
	search, ok := data["searchMyAssets"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("operation data has no searchMyAssets object")
	}
	raw, present := search["results"]
	if !present {
		return nil, fmt.Errorf("searchMyAssets has no results field")
	}
	if raw == nil {
		return nil, nil
	}
	rows, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("searchMyAssets.results is %T, want array", raw)
	}
	return rows, nil
}
