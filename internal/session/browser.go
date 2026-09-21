package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// sessionStoreName is the file Gecko rewrites periodically while it is running, with
// the live session and the cookies for every host visited during it.
const sessionStoreName = "recovery.jsonlz4"

// cleanShutdownStoreName is where Gecko puts the session when it exits cleanly, deleting
// the recovery pair as it goes. Looking only for recovery.jsonlz4 therefore reports "no
// session" for a user who signed in and then quit the browser, on a machine whose
// profile holds the jar — and LS is a server-side session, so closing the browser does
// not invalidate it.
const cleanShutdownStoreName = "sessionstore.jsonlz4"

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
	return geckoRootsFor(runtime.GOOS, home, os.Getenv("APPDATA"))
}

// geckoRootsFor takes the platform and the redirectable base explicitly so a test can
// check every branch. The list is a promise the README makes by name, and a branch that is
// short a browser is invisible to a run on any other platform.
//
// appData is passed rather than derived because %APPDATA% is a Windows known folder, not a
// fixed place under the profile: Folder Redirection, which is ordinary on a domain-joined
// machine, moves it elsewhere entirely. Reconstructing it as home/AppData/Roaming is why
// os.UserConfigDir reads the variable instead, and getting it wrong reports "no session
// store found" to a user with a signed-in browser while listing five directories that do
// not exist. The Linux paths stay home-relative because the browsers themselves hardcode
// them: Gecko reads $HOME/.mozilla and does not consult $XDG_CONFIG_HOME.
func geckoRootsFor(goos, home, appData string) []string {
	if appData == "" {
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	var roots []string
	add := func(base string, rel ...string) {
		for _, r := range rel {
			roots = append(roots, filepath.Join(base, filepath.FromSlash(r)))
		}
	}
	switch goos {
	case "darwin":
		add(home,
			"Library/Application Support/zen",
			"Library/Application Support/Firefox",
			"Library/Application Support/LibreWolf",
			"Library/Application Support/Waterfox",
			"Library/Application Support/Floorp",
		)
	case "windows":
		add(appData, "zen", "Mozilla/Firefox", "LibreWolf", "Waterfox", "Floorp")
	default:
		// Order is the contract, so these stay interleaved exactly as they were when
		// every entry hung off the home directory: the first root carrying the credential
		// wins, and reordering them changes which account a run reads.
		add(home, ".config/zen", ".zen", ".mozilla/firefox", ".librewolf", ".waterfox",
			".floorp", ".config/floorp")
		// Sandboxed packagings put the profile somewhere else entirely, and on Ubuntu
		// 22.04+ the snap is what `apt install firefox` gives you — so omitting these
		// answers "no session store found" on a machine with a signed-in Firefox, for
		// a browser the README promises by name.
		add(home,
			"snap/firefox/common/.mozilla/firefox",
			".var/app/org.mozilla.firefox/.mozilla/firefox",
			".var/app/app.zen_browser.zen/.zen",
			".var/app/io.gitlab.librewolf-community/.librewolf",
			".var/app/net.waterfox.waterfox/.waterfox",
			".var/app/one.ablaze.floorp/.floorp",
		)
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
//
// The flag still beats file order, which is the only thing left to rank by. installs.ini
// can be absent, or can name a profile carrying no session, and two profiles holding a
// credential each — two Unity accounts, the browser closed so both files are resting — are
// then separated by which section happens to come first. The flag is what the user chose.
func profileDirs(root string) []string {
	preferred := installDefaults(root)
	var flagged, rest []string
	for _, e := range iniEntries(filepath.Join(root, "profiles.ini"), "Path") {
		full := e.under(root)
		switch {
		case contains(preferred, full):
		case e.preferred:
			flagged = append(flagged, full)
		default:
			rest = append(rest, full)
		}
	}
	return append(append(preferred, flagged...), rest...)
}

// installDefaults reads installs.ini, which records the profile each installation of the
// browser last used.
func installDefaults(root string) []string {
	var out []string
	for _, e := range iniEntries(filepath.Join(root, "installs.ini"), "Default") {
		out = append(out, e.under(root))
	}
	return out
}

// iniEntry is one section's value for the key asked for, along with how that section says
// to read it.
type iniEntry struct {
	value string

	// relative mirrors the section's IsRelative flag, which Mozilla sets to 0 when Path
	// names an absolute directory — what the Profile Manager writes for a profile placed
	// outside the browser root. Joining such a path under the root yields a directory that
	// does not exist, so the profile is silently skipped and a signed-in browser reports
	// no session.
	relative bool

	// preferred mirrors profiles.ini's Default flag, which marks the profile the Profile
	// Manager opens. It ranks below installs.ini, which records what is actually running,
	// and above nothing but file order.
	preferred bool
}

// under resolves an entry against the browser root. An absolute value is honoured whatever
// the flag says, because installs.ini carries no IsRelative at all.
func (e iniEntry) under(root string) string {
	p := filepath.FromSlash(e.value)
	if !e.relative || filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(root, p)
}

// iniEntries reads one key from every section of a Mozilla ini, pairing it with that
// section's IsRelative flag. The format is plain enough that a full ini parser would be
// more code than it saves, and both files this reads are written by the browser rather
// than by a user.
func iniEntries(path, key string) []iniEntry {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []iniEntry
	// A section's two keys arrive in either order, so the value is held until the section
	// ends and the flag is known.
	pending := iniEntry{relative: true}
	flush := func() {
		if pending.value != "" {
			out = append(out, pending)
		}
		pending = iniEntry{relative: true}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case strings.EqualFold(k, key):
			pending.value = v
		case strings.EqualFold(k, "IsRelative"):
			pending.relative = v != "0"
		case strings.EqualFold(k, "Default"):
			// Only ever a flag here: the case above claims this key first when Default is
			// the value being read, which is how installs.ini names a path with it.
			pending.preferred = v == "1"
		}
	}
	flush()
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

	// OriginAttributes carries the jar a cookie belongs to. Multi-Account Containers gives
	// a container tab its own, so one file can hold two LS cookies for the same host under
	// two Unity accounts. Keyed on the name alone they collapse to whichever the document
	// happens to list last, which is a run against the other account — and Resolved.Path
	// cannot diagnose it, because both came from the same file.
	OriginAttributes struct {
		UserContextID int `json:"userContextId"`
	} `json:"originAttributes"`
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

// errNotSessionStoreJSON means the block decompressed but is not the document this
// parses. It carries nothing read out of the file.
var errNotSessionStoreJSON = errors.New("session store is not the JSON this expects")

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
		// The decoder's own error is dropped rather than wrapped. encoding/json builds a
		// syntax error by quoting the offending byte, and that byte came out of a file
		// holding credentials for every host the browsing session touched; this error
		// then reaches the user through errNoBrowserCredential. Nothing unwraps it.
		return nil, errNotSessionStoreJSON
	}

	pairs := map[string]string{}
	take := func(jar []storeCookie, wantDefault bool) {
		for _, c := range jar {
			if !hostMatches(c.Host) {
				continue
			}
			if (c.OriginAttributes.UserContextID == 0) != wantDefault {
				continue
			}
			// First wins, so the order is the document's rather than whichever entry the
			// walk happens to reach last.
			if _, have := pairs[c.Name]; !have {
				pairs[c.Name] = c.Value
			}
		}
	}
	// The default context first, containers only to supply names it did not. A user signed
	// in only inside a container still resolves, and a container tab left open against a
	// second Unity account never silently outranks the session the rest of the browser is
	// using.
	for _, wantDefault := range []bool{true, false} {
		for _, w := range store.Windows {
			take(w.Cookies, wantDefault)
		}
		take(store.Cookies, wantDefault)
	}

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

// searchedRoots is what storeCandidates swept, for a diagnostic when it found nothing.
func searchedRoots(source string) []string {
	if source == BrowserKeyword {
		return geckoRoots()
	}
	return []string{source}
}

// storeCandidates lists the session-store files worth trying for a source, in the order
// they should be tried.
//
// A source is one of: the browser keyword, which sweeps every known Gecko root; a browser
// root holding profiles.ini; or a single profile directory. Anything the caller names is
// tried first and alone, so pointing at a specific profile never silently falls through
// to a different browser. A path to a regular file never reaches here: ResolveFrom
// identifies a file by its contents instead.
func storeCandidates(source string) []string {
	if source == BrowserKeyword {
		var live, resting []string
		for _, root := range geckoRoots() {
			l, r := storesUnder(root)
			live, resting = append(live, l...), append(resting, r...)
		}
		// Every live jar first, across all roots, before any clean-shutdown one. A
		// profile signed in right now beats a profile whose last clean exit happened to
		// write a session, whichever browser they belong to: resolveBrowser takes the
		// first candidate carrying LS, and a resting file's credential can be old.
		return append(live, resting...)
	}
	live, resting := storesUnder(source)
	return append(live, resting...)
}

// storesUnder finds the session stores beneath a browser root or a single profile,
// keeping the ones a running browser maintains apart from the ones it leaves behind on
// a clean exit. A profile has one or the other, never both at once.
func storesUnder(dir string) (live, resting []string) {
	collect := func(profile string) {
		if c := filepath.Join(profile, "sessionstore-backups", sessionStoreName); exists(c) && !contains(live, c) {
			live = append(live, c)
		}
		if c := filepath.Join(profile, cleanShutdownStoreName); exists(c) && !contains(resting, c) {
			resting = append(resting, c)
		}
	}
	collect(dir)
	for _, p := range profileDirs(dir) {
		collect(p)
	}
	return live, resting
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// resolveBrowser returns the Cookie header from the first session store that carries the
// credential, along with the file it came from.
//
// A profile that parses but holds no LS is skipped rather than fatal: a second browser,
// or a second profile in the same browser, is where the signed-in tab usually is.
func resolveBrowser(source string) (Resolved, error) {
	candidates := storeCandidates(source)
	if len(candidates) == 0 {
		// Named, the way errNoBrowserCredential names what it read. This is the message a
		// user gets when their browser is not one of the roots swept, and without the list
		// it reads as "you have no session" rather than "look somewhere else".
		return Resolved{}, fmt.Errorf("no Firefox-family session store found for %q (looked under: %s)",
			source, strings.Join(searchedRoots(source), ", "))
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
		if _, ok := pairs[credentialCookie]; !ok {
			skipped = append(skipped, fmt.Sprintf("%s (no %s cookie)", path, credentialCookie))
			continue
		}
		return Resolved{Header: join(pairs), Path: path}, nil
	}
	return Resolved{}, &errNoBrowserCredential{Source: source, Skipped: skipped}
}

// errNoBrowserCredential means session stores were found and read but none held the
// credential. It lists what was tried, because the usual cause is that the browser has
// never signed in during this browsing session rather than anything being misconfigured.
type errNoBrowserCredential struct {
	Source  string
	Skipped []string
}

func (e *errNoBrowserCredential) Error() string {
	return fmt.Sprintf("no %s cookie in any Firefox-family session store for %q: the browser keeps it "+
		"for the life of a browsing session, so sign in to the Asset Store in that browser and "+
		"try again (looked at: %s)", credentialCookie, e.Source, strings.Join(e.Skipped, "; "))
}
