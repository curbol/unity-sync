package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// sessionStoreName is the file Gecko rewrites periodically with the live session,
// including the cookies for every host visited during it.
const sessionStoreName = "recovery.jsonlz4"

// BrowserKeyword asks Resolve to find a signed-in Gecko profile instead of reading a file.
const BrowserKeyword = "browser"

// geckoRoots lists where Gecko browsers keep their profile directories, relative to the
// home directory. Only Zen's layout was verified against a real install; the others are
// the standard Gecko layout, which every fork inherits along with profiles.ini.
//
// The list is a convenience, not the mechanism: Resolve also accepts a path to a profile
// directory or straight to a recovery.jsonlz4, so a browser missing from here still works
// by pointing at it.
func geckoRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var rel []string
	switch runtime.GOOS {
	case "darwin":
		rel = []string{
			"Library/Application Support/zen",
			"Library/Application Support/Firefox",
			"Library/Application Support/LibreWolf",
			"Library/Application Support/Waterfox",
			"Library/Application Support/Floorp",
		}
	case "windows":
		rel = []string{"AppData/Roaming/zen", "AppData/Roaming/Mozilla/Firefox"}
	default:
		rel = []string{
			".config/zen", ".zen",
			".mozilla/firefox",
			".librewolf",
			".waterfox",
			".floorp", ".config/floorp",
		}
	}
	roots := make([]string, 0, len(rel))
	for _, r := range rel {
		roots = append(roots, filepath.Join(home, filepath.FromSlash(r)))
	}
	return roots
}

// profileDirs returns every profile under a Gecko root, the one the browser is actually
// running first.
//
// The ordering is the point. profiles.ini can mark one profile `Default=1` while the
// install is running a different one, and the install's choice is the profile with a live
// session: on the machine this was built against, the `Default=1` profile has no
// sessionstore-backups directory at all. Reading only the flag finds an empty profile and
// reports no session on a machine where there plainly is one.
func profileDirs(root string) []string {
	preferred := installDefaults(root)
	var rest []string
	for _, p := range iniPaths(filepath.Join(root, "profiles.ini")) {
		full := filepath.Join(root, filepath.FromSlash(p))
		if !contains(preferred, full) {
			rest = append(rest, full)
		}
	}
	return append(preferred, rest...)
}

// installDefaults reads installs.ini, which records the profile each installation of the
// browser last used.
func installDefaults(root string) []string {
	var out []string
	for _, p := range iniValues(filepath.Join(root, "installs.ini"), "Default") {
		out = append(out, filepath.Join(root, filepath.FromSlash(p)))
	}
	return out
}

// iniPaths pulls the Path= entries out of a profiles.ini.
func iniPaths(path string) []string { return iniValues(path, "Path") }

// iniValues reads one key from every section of a Mozilla ini. The format is plain enough
// that a full ini parser would be more code than it saves, and both files this reads are
// written by the browser rather than by a user.
func iniValues(path, key string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// storeCookie is the shape Gecko records a cookie in. The value is read and used, never
// logged: this file holds the credentials for every host the browsing session touched.
type storeCookie struct {
	Host  string `json:"host"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// sessionStore is the slice of recovery.jsonlz4 this needs. Cookies sit under each window;
// the top-level array is accepted too, since it costs one field to not depend on which of
// the two a given version writes.
type sessionStore struct {
	Windows []struct {
		Cookies []storeCookie `json:"cookies"`
	} `json:"windows"`
	Cookies []storeCookie `json:"cookies"`
}

// fromSessionStore reads a Gecko session store and keeps only the store's own cookies.
//
// Everything else in the file is discarded before it can reach a caller. The jar spans
// every host visited in the browsing session, not just the ones with a tab still open, so
// narrowing to the unity.com family here — rather than anywhere later — keeps unrelated
// credentials out of the rest of the program.
func fromSessionStore(raw []byte) (map[string]string, error) {
	decoded, err := decodeMozLZ4(raw)
	if err != nil {
		return nil, err
	}
	var store sessionStore
	if err := json.Unmarshal(decoded, &store); err != nil {
		return nil, fmt.Errorf("session store is not the JSON this expects: %w", err)
	}

	pairs := map[string]string{}
	take := func(jar []storeCookie) {
		for _, c := range jar {
			if hostMatches(c.Host) {
				pairs[c.Name] = c.Value
			}
		}
	}
	for _, w := range store.Windows {
		take(w.Cookies)
	}
	take(store.Cookies)

	if len(pairs) == 0 {
		return nil, fmt.Errorf("no %s cookies in the session store", cookieDomain)
	}
	return pairs, nil
}

// isMozLZ4 reports whether a file starts with Mozilla's compressed-blob magic, which is
// how a session store is told apart from a pasted curl or a cookies.txt without asking
// the user to declare which one they saved.
func isMozLZ4(raw []byte) bool {
	return len(raw) >= len(mozlz4Magic) && string(raw[:len(mozlz4Magic)]) == mozlz4Magic
}

// storeCandidates lists the session-store files worth trying for a source, in the order
// they should be tried.
//
// A source is one of: the browser keyword, which sweeps every known Gecko root; a browser
// root holding profiles.ini; a single profile directory; or a path straight to a session
// store. Anything the caller names is tried first and alone, so pointing at a specific
// profile never silently falls through to a different browser.
func storeCandidates(source string) []string {
	if source == BrowserKeyword {
		var out []string
		for _, root := range geckoRoots() {
			out = append(out, storesUnder(root)...)
		}
		return out
	}
	if fi, err := os.Stat(source); err == nil && !fi.IsDir() {
		return []string{source}
	}
	return storesUnder(source)
}

// storesUnder finds the session stores beneath a browser root or a single profile.
func storesUnder(dir string) []string {
	var out []string
	direct := filepath.Join(dir, "sessionstore-backups", sessionStoreName)
	if _, err := os.Stat(direct); err == nil {
		out = append(out, direct)
	}
	for _, p := range profileDirs(dir) {
		candidate := filepath.Join(p, "sessionstore-backups", sessionStoreName)
		if _, err := os.Stat(candidate); err == nil && !contains(out, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

// resolveBrowser returns the Cookie header from the first session store that carries the
// credential, along with the file it came from.
//
// A profile that parses but holds no LS is skipped rather than fatal: a second browser,
// or a second profile in the same browser, is where the signed-in tab usually is.
func resolveBrowser(source string) (header, from string, err error) {
	candidates := storeCandidates(source)
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no Firefox-family session store found for %q", source)
	}
	var skipped []string
	for _, path := range candidates {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", path, readErr))
			continue
		}
		pairs, parseErr := fromSessionStore(raw)
		if parseErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", path, parseErr))
			continue
		}
		if _, ok := pairs[CredentialCookie]; !ok {
			skipped = append(skipped, fmt.Sprintf("%s (no %s cookie)", path, CredentialCookie))
			continue
		}
		return join(pairs), path, nil
	}
	return "", "", &ErrNoBrowserCredential{Source: source, Skipped: skipped}
}

// ErrNoBrowserCredential means session stores were found and read but none held the
// credential. It lists what was tried, because the usual cause is that the browser has
// never signed in during this browsing session rather than anything being misconfigured.
type ErrNoBrowserCredential struct {
	Source  string
	Skipped []string
}

func (e *ErrNoBrowserCredential) Error() string {
	return fmt.Sprintf("no %s cookie in any Firefox-family session store for %q: the browser keeps it "+
		"for the life of a browsing session, so sign in to the Asset Store in that browser and "+
		"try again (looked at: %s)", CredentialCookie, e.Source, strings.Join(e.Skipped, "; "))
}
