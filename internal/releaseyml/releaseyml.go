// Package releaseyml reads the platform matrix out of the release workflow, which is
// the one place the published asset labels are decided. The installer derives those
// labels from uname and the updater derives them from GOOS/GOARCH; each is bound to
// this list by a guard in its own package, and those two packages cannot share a test
// helper, so the parser lives here rather than as a copy beside each of them.
package releaseyml

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Platform is one entry of the workflow's platforms list: the pair it builds for and
// the label the asset carries.
type Platform struct {
	GOOS, GOARCH, Label string
}

var entryRe = regexp.MustCompile(`"([a-z0-9]+)/([a-z0-9]+)/([a-z0-9-]+)"`)

// Platforms parses the platforms=( ... ) array out of the workflow at path. A shape it
// cannot read is an error rather than an empty slice: every caller is a guard, and a
// guard that quietly checks nothing is worse than one that fails.
func Platforms(path string) ([]Platform, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	_, rest, ok := strings.Cut(string(raw), "platforms=(")
	if !ok {
		return nil, fmt.Errorf("%s has no platforms=( ... ) list; this guard no longer reads what the workflow builds", path)
	}
	block, _, ok := strings.Cut(rest, ")")
	if !ok {
		return nil, fmt.Errorf("%s: the platforms list is unterminated", path)
	}
	var out []Platform
	for _, m := range entryRe.FindAllStringSubmatch(block, -1) {
		out = append(out, Platform{GOOS: m[1], GOARCH: m[2], Label: m[3]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no platforms parsed", path)
	}
	// A shape this reads only part of is the same failure as one it cannot read at all,
	// just quieter: an entry the regex misses (an underscore or a capital in a label,
	// say) drops out of the installer's guard, the updater's guard and the workflow's
	// own cross-compile step, while the release keeps publishing that asset. Counting
	// the quoted entries in the block is what tells "parsed everything" from "parsed
	// what it recognized".
	if quoted := strings.Count(block, `"`) / 2; quoted != len(out) {
		return nil, fmt.Errorf("%s: the platforms list holds %d entries but only %d parse; "+
			"this guard would silently stop covering the rest", path, quoted, len(out))
	}
	return out, nil
}
