package fixtures_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/store"
)

// The store will return several account-identifying fields if a query asks for them,
// and a capture taken with a wider query would carry them into testdata. The pinned
// query asks for none of them, so their appearance in a committed fixture means either
// the query grew or a fixture was hand-edited from another source.
var forbiddenFields = []string{
	"assignFrom",
	"grantTime",
	"orderId",
	"organizations",
	"userOverview",
}

var forbiddenPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"an email address", regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.]+`)},
	// Two spellings, because a cookie reaches this repo two ways. A curl paste or a
	// cookies.txt writes name=value; a Gecko session store writes {"name":"LS", ...},
	// which the first pattern cannot match. Without the second, the only term that fired
	// on a committed recovery.jsonlz4 was __Secure-next-auth — the one cookie docs/design
	// records as not being the credential and frequently absent, so a jar carrying LS
	// alone passed the guard with a live session in it.
	{"a session cookie", regexp.MustCompile(`(?i)(__Secure-next-auth|_csrf\s*=|\bLS\s*=)`)},
	{"a session-store cookie record", regexp.MustCompile(`(?i)"name"\s*:\s*"(LS|_csrf|__Secure-next-auth[\w.-]*)"`)},
	// Entitlement ids are 14-digit runs. Product ids are 5-6 digits and version ids 6-7,
	// so this cannot collide with the catalogue data the fixtures are for.
	{"a 14-digit entitlement id", regexp.MustCompile(`\b\d{14}\b`)},
}

// TestCommittedFixturesCarryNoAccountData fails the build rather than the review when
// account data reaches testdata. It walks every testdata directory in the repo, not just
// the one at the root: a package-local testdata/ is where Go puts fixtures by default, so
// it is where a session store or a raw capture would land, and one already exists at
// internal/store/testdata. Anything the walk misses is committed, public and permanent.
func TestCommittedFixturesCarryNoAccountData(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := 0
	dirs := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(filepath.ToSlash(path), "/testdata/") {
			return nil
		}
		seen++
		dirs[filepath.ToSlash(filepath.Dir(path))] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A session store is refused on sight rather than searched. It is compressed, so
		// what the patterns below can find in one is down to which literals LZ4 happened
		// to leave intact — and the suite builds every store it needs in memory through
		// fixtures.MozLZ4, so a committed one is never legitimate whatever it holds.
		//
		// testdata/fuzz is the exception: `go test -fuzz` writes a crasher there, and a
		// crasher for FuzzDecodeMozLZ4 carries that magic by construction. Those bytes come
		// from the fuzzer mutating a seed corpus defined in Go source, so there is no
		// account data in them — but they are still scanned for it below.
		inFuzzCorpus := strings.Contains(filepath.ToSlash(path), "/testdata/fuzz/")
		if bytes.HasPrefix(raw, []byte("mozLz40\x00")) && !inFuzzCorpus {
			t.Errorf("%s: is a Firefox session store, which carries the credentials of "+
				"every host the browsing session touched", path)
			return nil
		}
		body := string(raw)
		for _, field := range forbiddenFields {
			if strings.Contains(body, `"`+field+`"`) {
				t.Errorf("%s: contains account-identifying field %q", path, field)
			}
		}
		for _, p := range forbiddenPatterns {
			if m := p.re.FindString(body); m != "" {
				t.Errorf("%s: contains %s", path, p.name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk testdata: %v", err)
	}
	if seen == 0 {
		t.Fatal("no fixtures found; the guard would pass vacuously")
	}
	// The walk is over the whole repo, so a mistake in the testdata filter would silently
	// narrow it to the top-level directory. Counted by shape rather than by number: there
	// happen to be two today, and inlining internal/store's golden query into its own test
	// file is a reasonable change that would take one away — which must not read as the
	// filter having broken. What has to hold is that a testdata directory nested inside a
	// package is reached at all when one exists.
	var nested, top bool
	for d := range dirs {
		if strings.Contains(filepath.ToSlash(d), "/internal/") {
			nested = true
		} else {
			top = true
		}
	}
	if !top {
		t.Errorf("walked %d file(s) in %v; the top-level testdata directory went unchecked",
			seen, sortedKeys(dirs))
	}
	if !nested && packageLocalTestdataExists(t) {
		t.Errorf("walked %d file(s) in %v; a package-local testdata directory exists but the "+
			"filter no longer reaches it", seen, sortedKeys(dirs))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// packageLocalTestdataExists reports whether any package under internal/ keeps its own
// testdata directory. It is what keeps the nested-directory assertion honest in both
// directions: required while one exists, silent once none does.
func packageLocalTestdataExists(t *testing.T) bool {
	t.Helper()
	var found bool
	// Compared against the walk root itself, not against a directory name. The root is the
	// relative "../..", so filepath.Dir of the top-level testdata is "../.." and its Base
	// is ".." — never the repository's name, whatever the checkout is called. Matching on
	// the name counted the top-level directory as package-local, and only the lexical walk
	// order (internal sorts before testdata) hid it.
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Name() == "testdata" && filepath.Dir(p) != root {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for package-local testdata: %v", err)
	}
	return found
}

// The scrub is an allowlist projected from store.SearchDocument, so a field the query
// never asks for cannot reach a fixture — and a field it does ask for reaches every
// fixture the next regeneration writes. That makes the pinned document, not the committed
// JSON, the place where an account-identifying field actually gets in.
//
// The walk above cannot catch it. It matches `"field"`, a JSON key, while the document is
// GraphQL where the same field is the bare token `orderId`. Adding one under product and
// updating the golden copy would widen searchFields with nothing refusing it; the golden
// test makes the change visible in review, which is not the same as stopping it.
func TestThePinnedQueryAsksForNoAccountField(t *testing.T) {
	for _, field := range forbiddenFields {
		word := regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\b`)
		if word.MatchString(store.SearchDocument) {
			t.Errorf("store.SearchDocument asks for the account-identifying field %q; "+
				"the scrub projects its allowlist from this document, so every fixture "+
				"regenerated after this change would carry it", field)
		}
	}
	// The document has to be the real one, or the loop above passes on an empty string.
	if !strings.Contains(store.SearchDocument, "searchMyAssets") {
		t.Fatal("store.SearchDocument is not the pinned query; this test is reading the wrong thing")
	}
}
