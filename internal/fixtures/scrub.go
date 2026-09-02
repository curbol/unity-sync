// Package fixtures turns raw Asset Store captures into the PII-free JSON that the
// offline test suite runs against. Captures are never committed; the scrubbed output is.
//
// The scrub is an allowlist, not a list of known-bad fields. It projects every captured
// row onto the field set store.SearchDocument asks for and drops everything else, so a
// capture taken with the storefront's own wider query carries nothing extra into
// testdata. A denylist would have to name each account-identifying field in advance, and
// the only supported way to regenerate is to capture again from a signed-in session
// against whatever the store returns that day.
package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/curbol/unity-sync/internal/store"
)

// fieldTree is a GraphQL selection set: each field mapped to its own selection set, empty
// for a leaf. A leaf's value is kept whole, because that is what the query asked for.
type fieldTree map[string]fieldTree

// searchFields is the selection set under searchMyAssets, read out of the pinned document
// itself. Deriving it rather than restating it is the point: there is no second list to
// keep in step, so a field added to or removed from the query changes the scrub with it.
var searchFields = sync.OnceValues(func() (fieldTree, error) {
	root, err := parseSelectionSets(store.SearchDocument)
	if err != nil {
		return nil, fmt.Errorf("parsing the pinned query: %w", err)
	}
	sel, ok := root["searchMyAssets"]
	if !ok || len(sel) == 0 {
		return nil, fmt.Errorf("the pinned query has no searchMyAssets selection set")
	}
	// The whole operation is pruned, not just the rows: a capture can carry a second root
	// field beside searchMyAssets, or an errors/extensions array beside data, and those
	// sit above the row projection where nothing else would look at them.
	return fieldTree{"data": root}, nil
})

// Scrub rewrites one captured `searchMyAssets` batch response into fixture form,
// preserving key order-independent structure and stable indentation so the committed
// file diffs cleanly. It fails rather than guessing when the payload is not the batch
// shape the client actually parses.
func Scrub(raw []byte) ([]byte, error) {
	allowed, err := searchFields()
	if err != nil {
		return nil, err
	}
	var batch []map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil {
		return nil, fmt.Errorf("capture is not a GraphQL batch array: %w", err)
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("capture holds no operations")
	}
	for _, op := range batch {
		if _, err := searchObject(op); err != nil {
			return nil, err
		}
		prune(op, allowed)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(batch); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// prune deletes everything the selection set did not ask for, in place. An empty set is a
// leaf, whose value the query wanted whole.
func prune(v any, allowed fieldTree) {
	if len(allowed) == 0 {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			prune(e, allowed)
		}
	case map[string]any:
		for k := range x {
			sub, ok := allowed[k]
			if !ok {
				delete(x, k)
				continue
			}
			prune(x[k], sub)
		}
	}
}

// searchObject walks to searchMyAssets, tolerating the terminator page whose results list
// is empty but not a payload missing the path entirely.
func searchObject(op map[string]any) (map[string]any, error) {
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
	rows, ok := raw.([]any)
	if !ok && raw != nil {
		return nil, fmt.Errorf("searchMyAssets.results is %T, want array", raw)
	}
	// Checked rather than skipped: the projection would quietly leave a row it does not
	// recognise untouched, and an unrecognised row is exactly the one whose fields nobody
	// has looked at.
	for _, row := range rows {
		if _, ok := row.(map[string]any); !ok {
			return nil, fmt.Errorf("result row is %T, want object", row)
		}
	}
	return search, nil
}

// parseSelectionSets reads the field tree out of a GraphQL query. It handles exactly what
// the pinned document uses — named fields, nested selection sets, and argument lists that
// carry no field names — because that document is the only input it will ever see.
func parseSelectionSets(doc string) (fieldTree, error) {
	toks := tokenize(doc)
	i := 0
	for i < len(toks) && toks[i] != "{" {
		i++
	}
	if i == len(toks) {
		return nil, fmt.Errorf("no selection set")
	}
	set, _, err := parseSet(toks, i+1)
	return set, err
}

func parseSet(toks []string, i int) (fieldTree, int, error) {
	set := fieldTree{}
	for i < len(toks) {
		switch toks[i] {
		case "}":
			return set, i + 1, nil
		case "{":
			return nil, 0, fmt.Errorf("selection set with no field before it")
		case "@", ":", ".":
			// Refused, not skipped. Each of these changes which response key a field
			// answers to, and guessing wrong here widens what reaches a committed
			// fixture. Teach this parser the construct before using it in the query.
			return nil, 0, fmt.Errorf("the pinned query uses %q, which this parser does not "+
				"understand; a directive, alias or fragment must be handled explicitly "+
				"before it can appear in a document the scrub projects from", toks[i])
		}
		name := toks[i]
		i++
		if i < len(toks) && toks[i] == "{" {
			sub, next, err := parseSet(toks, i+1)
			if err != nil {
				return nil, 0, err
			}
			set[name], i = sub, next
			continue
		}
		set[name] = fieldTree{}
	}
	return nil, 0, fmt.Errorf("unterminated selection set")
}

// tokenize emits field names and braces, dropping argument lists whole: everything inside
// parentheses is variables and types, never a field the response carries.
func tokenize(doc string) []string {
	var out []string
	var word strings.Builder
	depth := 0
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, r := range doc {
		switch {
		case r == '(':
			flush()
			depth++
		case r == ')':
			depth--
		case depth > 0:
		case r == '{' || r == '}':
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			word.WriteRune(r)
		// Emitted rather than dropped as whitespace. A directive, an alias or a fragment
		// spread would otherwise leave the field before it looking like a leaf, and a
		// leaf's value is kept whole — so the scrub would silently widen, which is the
		// one direction a scrubber must never fail in.
		case r == '@' || r == ':' || r == '.':
			flush()
			out = append(out, string(r))
		default:
			flush()
		}
	}
	flush()
	return out
}
