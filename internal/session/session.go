// Package session turns a saved browser session into the Cookie header the Asset Store
// requires. The credential is the LS cookie: measured against the live store, _csrf plus
// LS alone returns a full owned-asset list, and the NextAuth session token is not
// consulted by either endpoint this tool uses.
//
// LS is a session cookie, so cookies.sqlite never holds it. A Firefox-family session
// store does, because Gecko records the cookies of the hosts its open tabs are using, and
// that file is a supported source alongside a pasted curl command and a cookies.txt
// export. Which one a path is gets decided by reading it, not by configuration.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// credentialCookie is the one cookie the store actually checks.
const credentialCookie = "LS"

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
	return fmt.Sprintf("session %s has no %s cookie: Unity keeps it in memory only, so cookies.sqlite "+
		"never has it — re-copy the session from DevTools (Network > any assetstore.unity.com "+
		"request > Copy as cURL) while signed in, or set session_source = %q to read it from a "+
		"signed-in Firefox-family tab", e.Source, credentialCookie, BrowserKeyword)
}

// Resolved is a usable session.
//
// The two are a named pair rather than two return values because exactly one of them is
// printable and they are the same underlying type. main prints Path on every run that
// searched, three lines below where it takes the result; when these were
// `(header, from string, err error)`, transposing the two names at the destructure
// compiled, passed vet, and wrote the live credential to stderr. Nothing in the package
// would have caught it either, because the tests that guard this assert on error strings
// and on join's output, never on what main prints.
type Resolved struct {
	// Header is the Cookie header for the store. It is the user's live session: it goes
	// into a request and nowhere else — no log line, no error string, no committed file.
	Header string

	// Path is the file the credential came from, and is safe to print. Which profile a
	// search settled on is not obvious, and a run against the wrong signed-in account is
	// otherwise silent.
	Path string
}

// ResolveFrom turns a session source into the Cookie header for the store, and reports
// which file the credential came from. It asserts the credential is present, whatever the
// source, so the diagnostic names the real problem instead of leaving it to a 500. The
// browser keyword can search several profiles, and a run that picked one of them should be
// able to say so rather than leaving the user to guess which tab it read.
//
// A source is the browser keyword, a directory (a Gecko profile or the root holding
// several), or a file. A file is identified by its contents: a compressed session store, a
// pasted curl command, or a cookies.txt.
func ResolveFrom(source string) (Resolved, error) {
	if source == BrowserKeyword {
		return resolveBrowser(source)
	}
	fi, statErr := os.Stat(source)
	if statErr != nil {
		return Resolved{}, statErr
	}
	if fi.IsDir() {
		return resolveBrowser(source)
	}

	raw, err := os.ReadFile(source)
	if err != nil {
		return Resolved{}, err
	}
	var pairs map[string]string
	if isMozLZ4(raw) {
		pairs, err = fromSessionStore(raw)
	} else {
		pairs, err = parse(string(raw))
	}
	if err != nil {
		return Resolved{}, fmt.Errorf("%s: %w", source, err)
	}
	if _, ok := pairs[credentialCookie]; !ok {
		return Resolved{}, &ErrNoCredential{Source: source}
	}
	return Resolved{Header: join(pairs), Path: source}, nil
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

// quoting states for curlArguments, named for the shell construct each one is inside.
const (
	bare = iota
	singleQuoted
	doubleQuoted
	ansiCQuoted
)

// curlArguments splits a pasted curl command into its arguments, undoing the quoting
// the shell it was copied for applies.
//
// DevTools writes the command for that shell, so the Cookie argument arrives in one of
// four spellings: POSIX single quotes, ANSI-C $'…' when a value holds a quote, plain
// double quotes, and on Windows the cmd form, which wraps every argument in ^" and
// prefixes ^ to each character cmd would otherwise eat — including the one inside %^7B
// that stops variable expansion. Matching one quote style with a pattern drops the other
// three, and dropping the Cookie argument drops exactly the credential.
//
// A value is never unescaped in a context that does not escape: inside POSIX single
// quotes every byte is literal, so a cookie value containing ^ survives as itself.
func curlArguments(content string) []string {
	var (
		args []string
		cur  strings.Builder
		// caret records that the open double-quoted run began as ^", which escapes the
		// quote itself: cmd therefore never counts itself as inside quotes and keeps
		// stripping carets through the value, and only the program's own argument parser
		// sees the quote as a delimiter.
		started, caret bool
		state          = bare
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}
	literal := func(b byte) {
		cur.WriteByte(b)
		started = true
	}
	for i := 0; i < len(content); i++ {
		c := content[i]
		next := byte(0)
		if i+1 < len(content) {
			next = content[i+1]
		}
		switch state {
		case singleQuoted:
			if c == '\'' {
				state = bare
				continue
			}
			literal(c)
		case ansiCQuoted, doubleQuoted:
			closer := byte('\'')
			if state == doubleQuoted {
				closer = '"'
			}
			switch {
			case c == closer:
				state, caret = bare, false
			case c == '\\' && next != 0:
				i++
				literal(next)
			case caret && c == '^' && next == '^':
				i++
				literal('^')
			case caret && c == '^':
				// A prefix on the byte after it, which keeps its normal meaning — so the
				// closing ^" ends the run rather than contributing a caret.
			default:
				literal(c)
			}
		default:
			switch {
			case c == ' ' || c == '\t' || c == '\n' || c == '\r':
				flush()
			case c == '\'':
				state, started = singleQuoted, true
			case c == '"':
				state, started = doubleQuoted, true
			case c == '$' && next == '\'':
				state, started = ansiCQuoted, true
				i++
			case c == '^' && next == '"':
				state, started, caret = doubleQuoted, true, true
				i++
			case c == '^' && next == '^':
				// cmd escapes a literal caret by doubling it.
				i++
				literal('^')
			case c == '^' || c == '`':
				// A line continuation, or a prefix on the byte after it, which keeps its
				// normal meaning. Dropping the marker covers both.
			case c == '\\':
				if next == '\n' || next == '\r' {
					continue
				}
				if next != 0 {
					i++
					literal(next)
				}
			default:
				literal(c)
			}
		}
	}
	flush()
	return args
}

// cookieArgument returns the value of a Cookie header argument in a pasted curl command.
func cookieArgument(content string) (string, bool) {
	args := curlArguments(content)
	for i, a := range args {
		if !strings.EqualFold(a, "-H") && !strings.EqualFold(a, "--header") {
			continue
		}
		if i+1 >= len(args) {
			continue
		}
		if v, ok := cutHeader(args[i+1], "Cookie"); ok {
			return v, true
		}
	}
	return "", false
}

func cutHeader(arg, name string) (string, bool) {
	if len(arg) <= len(name) || !strings.EqualFold(arg[:len(name)], name) || arg[len(name)] != ':' {
		return "", false
	}
	return strings.TrimSpace(arg[len(name)+1:]), true
}

// isCurlPaste distinguishes a pasted command from a cookies.txt by structure, not by the
// word "curl" appearing somewhere: exported cookie files often carry a header comment
// mentioning curl, and treating that as a command sends it to a parser that can only
// fail.
func isCurlPaste(content string) bool {
	if _, ok := cookieArgument(content); ok {
		return true
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A Windows paste names curl.exe, and the cmd form puts a caret against the
		// first argument's quote, so neither the program name nor the separator can be
		// matched as a literal prefix.
		first := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '^' || r == '"' || r == '\''
		})
		if len(first) == 0 {
			return false
		}
		base := strings.ToLower(filepath.Base(first[0]))
		return base == "curl" || base == "curl.exe"
	}
	return false
}

func fromCurl(content string) (map[string]string, error) {
	header, ok := cookieArgument(content)
	if !ok {
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
