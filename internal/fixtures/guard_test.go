package fixtures_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	{"a session cookie", regexp.MustCompile(`(?i)(__Secure-next-auth|_csrf\s*=|\bLS\s*=)`)},
	// Entitlement ids are 14-digit runs. Product ids are 5-6 digits and version ids 6-7,
	// so this cannot collide with the catalogue data the fixtures are for.
	{"a 14-digit entitlement id", regexp.MustCompile(`\b\d{14}\b`)},
}

// TestCommittedFixturesCarryNoAccountData fails the build rather than the review when
// account data reaches testdata. It walks whatever is committed, so it also covers
// fixtures added later by someone who never read the scrubber.
func TestCommittedFixturesCarryNoAccountData(t *testing.T) {
	root := filepath.Join("..", "..", "testdata")
	seen := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		seen++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
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
}
