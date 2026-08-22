// Package session turns a saved browser session into the Cookie header the Asset Store
// requires. The credential is the LS cookie: measured against the live store, _csrf plus
// LS alone returns a full owned-asset list, and the NextAuth session token is not
// consulted by either endpoint this tool uses. LS is a session cookie, so no browser
// cookie database ever holds it — which is why the only supported sources are a pasted
// curl command or a cookies.txt export.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CredentialCookie is the one cookie the store actually checks.
const CredentialCookie = "LS"

// cookieDomain is the family whose cookies a browser would send to the storefront. The
// storefront mixes scopes — the same response sets some cookies host-only on
// assetstore.unity.com and others with Domain=.unity.com — and which scope LS uses was
// never established, so the whole family is accepted rather than one host guessed.
const cookieDomain = "unity.com"

// httpOnlyPrefix marks an HttpOnly record in a Netscape cookies.txt. Exporters write it
// on the line itself, so a parser that treats every '#' line as a comment drops exactly
// the credential cookies.
const httpOnlyPrefix = "#HttpOnly_"

// ErrNoCredential means the source parsed but carries no LS cookie. It is reported
// before any request goes out, because the store answers a missing LS with an opaque
// HTTP 500 that reads like a server fault.
type ErrNoCredential struct{ Source string }

func (e *ErrNoCredential) Error() string {
	return fmt.Sprintf("session %s has no %s cookie: Unity keeps it in memory only, so a browser "+
		"cookie database never has it — re-copy the session from DevTools (Network > any "+
		"assetstore.unity.com request > Copy as cURL) while signed in", e.Source, CredentialCookie)
}

// Resolve reads a session file and returns the Cookie header for the store. It also
// asserts the credential is present, whatever the source, so the diagnostic names the
// real problem instead of leaving it to a 500.
func Resolve(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	pairs, err := parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := pairs[CredentialCookie]; !ok {
		return "", &ErrNoCredential{Source: path}
	}
	return join(pairs), nil
}

// Discover looks for a session file in the user config dir, so a first run needs no
// flag once the file is in the obvious place.
func Discover(configDir string) (string, bool) {
	for _, name := range []string{"session.curl", "cookies.txt"} {
		p := filepath.Join(configDir, name)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

func parse(content string) (map[string]string, error) {
	if isCurlPaste(content) {
		return fromCurl(content)
	}
	return fromCookiesTxt(content)
}

// A cookie value can itself contain the opposite quote character, so each outer-quote
// style gets its own pattern; RE2 has no backreferences.
var (
	curlSingle = regexp.MustCompile(`(?i)(?:-H|--header)\s+'Cookie:\s*([^']*)'`)
	curlDouble = regexp.MustCompile(`(?i)(?:-H|--header)\s+"Cookie:\s*([^"]*)"`)
)

// isCurlPaste distinguishes a pasted command from a cookies.txt by structure, not by the
// word "curl" appearing somewhere: exported cookie files often carry a header comment
// mentioning curl, and treating that as a command sends it to a parser that can only
// fail.
func isCurlPaste(content string) bool {
	if curlSingle.MatchString(content) || curlDouble.MatchString(content) {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.HasPrefix(line, "curl ")
	}
	return false
}

func fromCurl(content string) (map[string]string, error) {
	var header string
	switch {
	case curlSingle.MatchString(content):
		header = curlSingle.FindStringSubmatch(content)[1]
	case curlDouble.MatchString(content):
		header = curlDouble.FindStringSubmatch(content)[1]
	default:
		return nil, fmt.Errorf("no Cookie header in the pasted curl command")
	}
	pairs := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		pairs[name] = value
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("the pasted curl command's Cookie header is empty")
	}
	return pairs, nil
}

func fromCookiesTxt(content string) (map[string]string, error) {
	pairs := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		// "#HttpOnly_<domain>" is a record, not a comment.
		line = strings.TrimPrefix(line, httpOnlyPrefix)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		if !hostMatches(f[0]) {
			continue
		}
		pairs[f[5]] = f[6]
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("no %s cookies found (is this a cookies.txt export for the right site?)", cookieDomain)
	}
	return pairs, nil
}

func hostMatches(host string) bool {
	host = strings.TrimPrefix(strings.TrimSpace(host), ".")
	return host == cookieDomain || strings.HasSuffix(host, "."+cookieDomain)
}

// join renders a deterministic header so two runs with the same session produce
// byte-identical requests.
func join(pairs map[string]string) string {
	names := make([]string, 0, len(pairs))
	for n := range pairs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(n)
		b.WriteByte('=')
		b.WriteString(pairs[n])
	}
	return b.String()
}

// WithCSRF returns header with its _csrf cookie replaced by token, adding it when
// absent. A pasted session usually carries a stale _csrf, and sending two would leave
// the store matching the header against whichever it picked.
func WithCSRF(header, token string) string {
	pairs := map[string]string{}
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		pairs[name] = value
	}
	pairs["_csrf"] = token
	return join(pairs)
}
