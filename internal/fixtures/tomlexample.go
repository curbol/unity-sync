package fixtures

import (
	"regexp"
	"strings"
)

// settingLine matches a whole commented-out setting and not the prose around it, which in
// these files often opens with a key name mid-sentence.
var settingLine = regexp.MustCompile(`^[a-z_]+ *= *(".*"|[0-9]+|true|false)$`)

// UncommentSettings strips the leading "#" from every line of an example TOML file that is
// a commented-out setting or table header, leaving explanatory comments in place.
//
// It lives here for the reason Package does. Both example files are checked by feeding
// them to the parser rather than by reading them, which only works on the lines a user
// would uncomment — and the two suites that do it are separate test packages, so each had
// its own copy of the pattern above. A change to one left the other silently matching
// less, and a pattern that matches nothing hands the parser a file with no settings in it,
// where every assertion still passes and the example has stopped being checked at all.
func UncommentSettings(raw []byte) []byte {
	out := strings.Split(string(raw), "\n")
	for i, line := range out {
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		if strings.HasPrefix(body, "[[") || settingLine.MatchString(body) {
			out[i] = body
		}
	}
	return []byte(strings.Join(out, "\n"))
}
