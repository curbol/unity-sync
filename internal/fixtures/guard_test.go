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
// account data reaches testdata. It walks every testdata directory in the repo, not just
// the one at the root: a package-local testdata/ is where Go puts fixtures by default, so
// it is where a session store or a raw capture would land, and one already exists at
// internal/store/testdata. Anything the walk misses is committed, public and permanent.
func TestCommittedFixturesCarryNoAccountData(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := 0
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
	// The walk is over the whole repo, so a mistake in the testdata filter would silently
	// narrow it to nothing much. Both known testdata directories have to be in the count.
	if seen < 5 {
		t.Errorf("walked only %d file(s); both testdata directories should be covered", seen)
	}
}
