package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mozlz4Stored wraps payload in the container Gecko writes, as the single all-literals
// sequence LZ4 emits for incompressible input. It has to be one sequence: only the last
// one in a block may omit its match, so a chain of literal-only sequences is not a legal
// block and the decoder is right to reject it.
func mozlz4Stored(t *testing.T, payload []byte) []byte {
	t.Helper()
	out := []byte(mozlz4Magic)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))

	if n := len(payload); n < 15 {
		out = append(out, byte(n)<<4)
	} else {
		out = append(out, byte(15)<<4)
		for remainder := n - 15; ; {
			if remainder >= 255 {
				out = append(out, 255)
				remainder -= 255
				continue
			}
			out = append(out, byte(remainder))
			break
		}
	}
	return append(out, payload...)
}

// A match may point into bytes the same match is still writing, which is how the format
// encodes a repeating run. Copying in bulk would read the pre-overlap bytes instead of
// the ones just produced, so this is hand-built rather than generated: the encoder above
// emits literals only and would never exercise it.
func TestMozLZ4ExpandsAnOverlappingMatch(t *testing.T) {
	// literals "abc", then a match of length 9 at offset 3 — each copied byte is one the
	// match itself just wrote.
	block := []byte{0x35, 'a', 'b', 'c', 0x03, 0x00}
	raw := append([]byte(mozlz4Magic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(raw[len(mozlz4Magic):], 12)
	raw = append(raw, block...)

	got, err := decodeMozLZ4(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != "abcabcabcabc" {
		t.Errorf("decoded %q, want %q", got, "abcabcabcabc")
	}
}

// A match pointing before the start of the output is the shape a corrupt or hostile file
// takes; it must be refused rather than read out of bounds.
func TestMozLZ4RefusesAMatchBeforeTheStart(t *testing.T) {
	block := []byte{0x35, 'a', 'b', 'c', 0xFF, 0x00} // offset 255, only 3 bytes decoded
	raw := append([]byte(mozlz4Magic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(raw[len(mozlz4Magic):], 12)
	raw = append(raw, block...)

	if _, err := decodeMozLZ4(raw); err == nil {
		t.Error("decode followed a match offset past the start of the output")
	}
}

func storeJSON(t *testing.T, cookies []storeCookie) []byte {
	t.Helper()
	doc := map[string]any{
		"windows": []any{map[string]any{"cookies": cookies, "tabs": []any{}}},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// writeProfile lays out a Gecko root the way a real install does.
func writeProfile(t *testing.T, root, profile string, cookies []storeCookie) string {
	t.Helper()
	dir := filepath.Join(root, profile, "sessionstore-backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionStoreName)
	if err := os.WriteFile(path, mozlz4Stored(t, storeJSON(t, cookies)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMozLZ4RoundTripsAndRejectsRubbish(t *testing.T) {
	payload := []byte(strings.Repeat(`{"windows":[{"cookies":[]}]}`, 200))
	got, err := decodeMozLZ4(mozlz4Stored(t, payload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("round trip changed the payload")
	}

	if _, err := decodeMozLZ4([]byte("not compressed at all")); err != errNotMozLZ4 {
		t.Errorf("decode of a plain file = %v, want errNotMozLZ4", err)
	}

	// A header claiming more than the block delivers must be an error, not a short read
	// that later parses as truncated JSON.
	lying := mozlz4Stored(t, payload)
	binary.LittleEndian.PutUint32(lying[len(mozlz4Magic):], uint32(len(payload)+500))
	if _, err := decodeMozLZ4(lying); err == nil {
		t.Error("decode accepted a header that overstates the payload")
	}

	// The size comes off disk, so an absurd claim must be refused rather than allocated.
	huge := mozlz4Stored(t, payload)
	binary.LittleEndian.PutUint32(huge[len(mozlz4Magic):], 1<<30)
	if _, err := decodeMozLZ4(huge); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("decode of an absurd size = %v, want the ceiling to refuse it", err)
	}
}

// A truncated or scrambled block must fail rather than return whatever it managed, since
// the caller would otherwise treat partial JSON as a missing credential.
func TestMozLZ4RefusesACorruptBlock(t *testing.T) {
	full := mozlz4Stored(t, []byte(strings.Repeat("payload ", 64)))
	for _, cut := range []int{len(full) / 2, len(full) - 1} {
		if _, err := decodeMozLZ4(full[:cut]); err == nil {
			t.Errorf("decode of a block truncated to %d bytes returned no error", cut)
		}
	}
}

// The jar holds every host the browsing session touched. Only the store's own cookies may
// leave this package.
func TestOnlyUnityCookiesLeaveTheSessionStore(t *testing.T) {
	raw := mozlz4Stored(t, storeJSON(t, []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "credential"},
		{Host: "assetstore.unity.com", Name: "_csrf", Value: "token"},
		{Host: ".unity.com", Name: "PIM-SESSION-ID", Value: "pim"},
		{Host: "bank.example.com", Name: "session", Value: "SHOULD-NOT-LEAK"},
		{Host: "notunity.com", Name: "session", Value: "SHOULD-NOT-LEAK"},
		{Host: "evil-unity.com.attacker.test", Name: "session", Value: "SHOULD-NOT-LEAK"},
	}))
	pairs, err := fromSessionStore(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 3 {
		t.Errorf("kept %d cookies, want the 3 unity.com ones: %v", len(pairs), keys(pairs))
	}
	for name, value := range pairs {
		if strings.Contains(value, "SHOULD-NOT-LEAK") {
			t.Errorf("cookie %q came from another site", name)
		}
	}
	header := join(pairs)
	if strings.Contains(header, "SHOULD-NOT-LEAK") {
		t.Error("an unrelated site's cookie reached the Cookie header")
	}
	if !strings.Contains(header, "LS=credential") {
		t.Errorf("header %q is missing the credential", header)
	}
}

// profiles.ini can mark one profile Default=1 while the browser runs another, and only
// the running one has a live session. Preferring the flag finds an empty profile and
// reports no session on a machine where there plainly is one.
func TestTheRunningProfileWinsOverTheDefaultFlag(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "aaaa.Default Profile", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "stale-profile"},
	})
	writeProfile(t, root, "bbbb.Default (release)", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "running-profile"},
	})
	os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile1]\nName=Default Profile\nIsRelative=1\nPath=aaaa.Default Profile\nDefault=1\n\n"+
			"[Profile0]\nName=Default (release)\nIsRelative=1\nPath=bbbb.Default (release)\n"), 0o644)
	os.WriteFile(filepath.Join(root, "installs.ini"), []byte(
		"[15B76BAA26BA15E7]\nDefault=bbbb.Default (release)\nLocked=1\n"), 0o644)

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(got.Header, "running-profile") {
		t.Errorf("the header came from the Default=1 profile, not the one installs.ini names: %q", got.Path)
	}
}

// Gecko has written the jar both under each window and as a top level array, and the
// reader accepts either. Only the windowed shape appears in the fixtures, so the fallback
// that exists for version drift is the half that would rot unnoticed.
func TestCookiesAtTheTopLevelOfTheSessionStoreAreRead(t *testing.T) {
	doc, err := json.Marshal(map[string]any{
		"cookies": []storeCookie{
			{Host: "assetstore.unity.com", Name: "LS", Value: "top-level"},
			{Host: "unrelated.example", Name: "leaked", Value: "no"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := fromSessionStore(mozlz4Stored(t, doc))
	if err != nil {
		t.Fatalf("fromSessionStore: %v", err)
	}
	if pairs[credentialCookie] != "top-level" {
		t.Errorf("pairs = %v, want the credential from the top-level array", pairs)
	}
	if _, ok := pairs["leaked"]; ok {
		t.Error("a cookie from an unrelated host survived the unity.com filter")
	}
}

// Mozilla writes IsRelative=0 with an absolute Path for a profile kept outside the browser
// root, which is what the Profile Manager produces for "Choose Folder". Joining that under
// the root anyway names a directory that cannot exist, so the profile is dropped from the
// scan without a word and a signed-in browser is reported as having no session.
func TestAnAbsoluteProfilePathIsNotJoinedUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	writeProfile(t, elsewhere, "relocated", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "relocated-profile"},
	})
	os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile0]\nName=relocated\nIsRelative=0\nPath="+
			filepath.ToSlash(filepath.Join(elsewhere, "relocated"))+"\n"), 0o644)

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(got.Header, "relocated-profile") {
		t.Errorf("header %q did not come from the profile profiles.ini names", got.Header)
	}
}

// installs.ini carries no IsRelative key at all, so a pending entry defaults to relative
// and an absolute Default= would be joined under the browser root without the IsAbs arm
// in iniEntry.under. That names a directory which cannot exist, so the running profile
// drops out of the preferred set and ranking falls back to the Default=1 flag in
// profiles.ini — the exact ordering profileDirs exists to override, because the flagged
// profile can be the one with no session store.
//
// The sibling test above writes IsRelative=0, which takes the other arm; deleting
// `|| filepath.IsAbs(p)` left the whole session suite green until this was added.
func TestAnAbsolutePathInInstallsIniIsNotJoinedUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	// The profile installs.ini points at, kept outside the browser root and signed in.
	writeProfile(t, elsewhere, "running", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "the-running-profile"},
	})
	// The profile profiles.ini flags as default, under the root and signed in as someone
	// else. installs.ini has to win, or the run reads the wrong account.
	writeProfile(t, root, "flagged", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "the-flagged-profile"},
	})
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile0]\nName=flagged\nIsRelative=1\nPath=flagged\nDefault=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No IsRelative key, which is the whole point.
	if err := os.WriteFile(filepath.Join(root, "installs.ini"), []byte(
		"[3B722F5C50BF4C1D]\nDefault="+filepath.ToSlash(filepath.Join(elsewhere, "running"))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(got.Header, "the-running-profile") {
		t.Errorf("header came from %s, not the profile installs.ini names: %q", got.Path, got.Header)
	}
}

// A profile whose session has no Asset Store credential is skipped, not fatal: the
// signed-in one is usually another profile or another browser.
func TestAProfileWithoutTheCredentialIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "aaaa.empty", []storeCookie{
		{Host: "assetstore.unity.com", Name: "_csrf", Value: "token-but-no-credential"},
	})
	writeProfile(t, root, "bbbb.signed-in", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "credential"},
	})
	os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile0]\nIsRelative=1\nPath=aaaa.empty\nDefault=1\n\n"+
			"[Profile1]\nIsRelative=1\nPath=bbbb.signed-in\n"), 0o644)

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(got.Header, "LS=credential") {
		t.Errorf("header = %q, want the signed-in profile's credential", got.Header)
	}
}

// installs.ini is what a running browser is using, so it wins — but it can be absent, or
// can name a profile that never signed in to Unity. What is left to rank by then is the
// Default flag or nothing, and "nothing" means file order: two profiles carrying a
// credential each, two Unity accounts, and the one the user actually chose loses to
// whichever section the browser happened to write first.
func TestTheDefaultProfileWinsWhenInstallsIniDoesNotDecide(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "aaaa.other-account", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "the-other-account"},
	})
	writeProfile(t, root, "bbbb.chosen", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "the-chosen-account"},
	})
	// No installs.ini at all, and the flagged profile is written second, so file order and
	// the flag disagree.
	os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile0]\nIsRelative=1\nPath=aaaa.other-account\n\n"+
			"[Profile1]\nIsRelative=1\nPath=bbbb.chosen\nDefault=1\n"), 0o644)

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(got.Header, "LS=the-chosen-account") {
		t.Errorf("header = %q, want the Default=1 profile's credential", got.Header)
	}
}

// When nothing holds the credential the error has to say what was tried, because the
// cause is usually "that browser never signed in" rather than a broken setup.
func TestNoCredentialAnywhereNamesWhatWasTried(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "aaaa.empty", []storeCookie{
		{Host: "assetstore.unity.com", Name: "_csrf", Value: "token"},
	})
	os.WriteFile(filepath.Join(root, "profiles.ini"),
		[]byte("[Profile0]\nIsRelative=1\nPath=aaaa.empty\n"), 0o644)

	_, err := ResolveFrom(root)
	if err == nil {
		t.Fatal("ResolveFrom succeeded with no credential anywhere")
	}
	var missing *errNoBrowserCredential
	if !asErr(err, &missing) {
		t.Fatalf("error is %T, want *errNoBrowserCredential", err)
	}
	if !strings.Contains(err.Error(), "aaaa.empty") {
		t.Errorf("error does not name the profile it read: %v", err)
	}
	if !strings.Contains(err.Error(), "sign in") {
		t.Errorf("error does not say what to do about it: %v", err)
	}
}

// A session store is identified by its contents, so the same flag takes a paste, a
// cookies.txt or a recovery file without the user declaring which.
func TestResolveTellsASessionStoreFromAPasteByContent(t *testing.T) {
	dir := t.TempDir()

	store := filepath.Join(dir, "recovery.jsonlz4")
	os.WriteFile(store, mozlz4Stored(t, storeJSON(t, []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "from-store"},
	})), 0o600)

	paste := filepath.Join(dir, "session.curl")
	os.WriteFile(paste, []byte(`curl 'https://assetstore.unity.com/api/graphql/batch' -H 'Cookie: LS=from-paste; _csrf=t'`), 0o600)

	for path, want := range map[string]string{store: "from-store", paste: "from-paste"} {
		got, err := ResolveFrom(path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !strings.Contains(got.Header, "LS="+want) {
			t.Errorf("%s produced %q, want LS=%s", filepath.Base(path), got.Header, want)
		}
	}
}

func asErr(err error, target any) bool { return errors.As(err, target) }

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A match longer than 18 bytes encodes its length the same way a long literal run does,
// through the 15-plus-extension-bytes escape. Nothing above reaches that path.
func TestMozLZ4ExpandsAnExtendedMatchLength(t *testing.T) {
	// literals "ab", then a 30-byte match at offset 2: low nibble 15 plus one extension
	// byte of 11, since the encoder stores length minus the four-byte minimum.
	block := []byte{0x2F, 'a', 'b', 0x02, 0x00, 11}
	raw := append([]byte(mozlz4Magic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(raw[len(mozlz4Magic):], 32)
	raw = append(raw, block...)

	got, err := decodeMozLZ4(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := strings.Repeat("ab", 16); string(got) != want {
		t.Errorf("decoded %q, want %q", got, want)
	}
}

// Synthetic blocks only prove the decoder against blocks this file wrote. Point this at a
// real profile to check it against bytes Gecko produced:
//
//	UNITY_SYNC_REAL_SESSIONSTORE=~/.config/zen/<profile>/sessionstore-backups/recovery.jsonlz4 go test ./internal/session/
//
// Skipped by default: the file is a live credential store and belongs to no CI run.
func TestDecodesARealSessionStore(t *testing.T) {
	path := os.Getenv("UNITY_SYNC_REAL_SESSIONSTORE")
	if path == "" {
		t.Skip("set UNITY_SYNC_REAL_SESSIONSTORE to a recovery.jsonlz4")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMozLZ4(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var doc any
	if err := json.Unmarshal(decoded, &doc); err != nil {
		t.Fatalf("decoded %d bytes that are not JSON: %v", len(decoded), err)
	}
	// Through fromSessionStore, not just through json.Unmarshal. Every other
	// session-store test builds its payload by marshalling this package's own structs, so
	// the json tags and the windows[].cookies path are only ever checked against JSON
	// this package wrote. If Gecko moves or renames that array, every profile yields "no
	// unity.com cookies in the session store" and the whole suite stays green.
	pairs, err := fromSessionStore(raw)
	if err != nil {
		t.Fatalf("fromSessionStore on a real profile: %v; the schema this parses has moved", err)
	}
	// Names only. The values are live credentials and this is the one test that holds any.
	t.Logf("decoded %d compressed bytes into %d bytes of JSON carrying %d unity.com cookie(s): %v",
		len(raw), len(decoded), len(pairs), keys(pairs))
	if _, ok := pairs[credentialCookie]; !ok {
		t.Logf("no %s in this profile; sign in to the Asset Store in that browser to exercise the whole path",
			credentialCookie)
	}
}

// offset 0 is a different failure from an offset past the start, and the guard that
// catches it is the only thing between a corrupt block and a panic: with that clause gone,
// offset > len(dst) is false, start lands on len(dst), and the first copied byte indexes
// one past the slice. Go bounds-checks against len, not cap, so a malformed
// recovery.jsonlz4 in the user's own profile would take the CLI down instead of being
// skipped as an unreadable profile — and the sibling test would stay green.
func TestMozLZ4RefusesAZeroMatchOffset(t *testing.T) {
	block := []byte{0x35, 'a', 'b', 'c', 0x00, 0x00} // literals "abc", then offset 0
	if _, err := lz4Decompress(block, 12); err == nil {
		t.Error("decode accepted a match offset of 0")
	}
}

// The browser keyword is the documented workflow and the only source that sweeps the
// geckoRoots table, and no test reached it: every other browser test enters through a
// temp directory, which takes the named-source branch instead. That left the root list
// itself unexercised, including whether a platform's branch names paths that can exist.
func TestTheBrowserKeywordFindsAProfileUnderAKnownRoot(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads a different variable per platform, so both are set.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	roots := geckoRoots()
	if len(roots) == 0 {
		t.Fatal("geckoRoots is empty on this platform, so the browser keyword can never work")
	}
	for _, r := range roots {
		if !strings.HasPrefix(r, home) {
			t.Errorf("root %q is not under the home directory", r)
		}
	}
	// Planted under the last root, so finding it also proves the sweep does not stop at
	// the first root that happens to exist.
	last := roots[len(roots)-1]
	planted := writeProfile(t, last, "p1", []storeCookie{
		{Host: "assetstore.unity.com", Name: credentialCookie, Value: "cred"},
	})
	os.WriteFile(filepath.Join(last, "profiles.ini"),
		[]byte("[Profile0]\nName=default\nIsRelative=1\nPath=p1\n"), 0o644)

	got, err := ResolveFrom(BrowserKeyword)
	if err != nil {
		t.Fatalf("ResolveFrom(%q): %v", BrowserKeyword, err)
	}
	if got.Path != planted {
		t.Errorf("read from %q, want %q", got.Path, planted)
	}
	if !strings.Contains(got.Header, credentialCookie+"=cred") {
		t.Errorf("header %q does not carry the credential", got.Header)
	}
}

// A source the caller named is tried alone. Falling through to the root sweep would run
// against whichever browser happened to be signed in rather than the one asked for, and
// the run would report a profile the user never pointed at.
func TestANamedSourceNeverFallsThroughToAnotherBrowser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// A perfectly good profile under a known root, which must not be reached.
	roots := geckoRoots()
	if len(roots) == 0 {
		t.Skip("no gecko roots on this platform")
	}
	decoy := writeProfile(t, roots[0], "signed-in", []storeCookie{
		{Host: "assetstore.unity.com", Name: credentialCookie, Value: "decoy"},
	})
	os.WriteFile(filepath.Join(roots[0], "profiles.ini"),
		[]byte("[Profile0]\nName=default\nIsRelative=1\nPath=signed-in\n"), 0o644)
	// The decoy has to be reachable, or the test proves nothing about not reaching it.
	if got, err := ResolveFrom(BrowserKeyword); err != nil || got.Path != decoy {
		t.Fatalf("the decoy profile is not discoverable: from=%q err=%v", got.Path, err)
	}

	named := t.TempDir() // an empty profile directory, named explicitly
	_, err := ResolveFrom(named)
	if err == nil {
		t.Fatal("a named empty directory resolved a session from somewhere else")
	}
	if strings.Contains(err.Error(), decoy) {
		t.Errorf("the error names a profile under a browser root: %v", err)
	}
	// The diagnostic has to say where it looked, or "no session store found" reads as
	// "you have no session" rather than "not in the place you named".
	if !strings.Contains(err.Error(), named) {
		t.Errorf("error %q does not name the directory it was given", err)
	}
}

// The README names five browsers unconditionally and the release ships a Windows binary,
// so a platform branch that is short one is a user on that platform being told they have
// no session when they plainly do. Only the branch this test runs on is otherwise
// exercised, and it is never the Windows one.
func TestEveryPlatformKnowsTheSameBrowsers(t *testing.T) {
	browsers := []string{"zen", "firefox", "librewolf", "waterfox", "floorp"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		roots := geckoRootsFor(goos, filepath.Join("home", "someone"), "")
		if len(roots) == 0 {
			t.Errorf("%s has no gecko roots, so the browser keyword can never work there", goos)
			continue
		}
		joined := strings.ToLower(strings.Join(roots, "\n"))
		for _, b := range browsers {
			if !strings.Contains(joined, b) {
				t.Errorf("%s has no root for %s, which README.md promises by name", goos, b)
			}
		}
		for _, r := range roots {
			// Written with forward slashes and joined through FromSlash, so a Windows run
			// gets a path Windows can open rather than a literal "AppData/Roaming/zen".
			if strings.Contains(r, "/") && filepath.Separator != '/' {
				t.Errorf("%s root %q kept a forward slash", goos, r)
			}
		}
	}

	// On Ubuntu 22.04+ `apt install firefox` installs the snap, whose profiles live
	// nowhere near ~/.mozilla. A list that names only the unsandboxed path answers "no
	// session store found" on the most common Linux desktop there is.
	linux := strings.Join(geckoRootsFor("linux", filepath.Join("home", "someone"), ""), "\n")
	for _, sandboxed := range []string{
		filepath.FromSlash("snap/firefox/common/.mozilla/firefox"),
		filepath.FromSlash(".var/app/org.mozilla.firefox/.mozilla/firefox"),
	} {
		if !strings.Contains(linux, sandboxed) {
			t.Errorf("linux has no root under %s, so a sandboxed install reports no session", sandboxed)
		}
	}
}

// README.md offers a profile directory as the escape hatch for a browser the root list
// does not know, which makes it the recovery route for exactly the case above. Every
// other directory-valued case in this file passes a browser *root* holding profiles.ini
// and reaches the store through profileDirs, so nothing else makes this branch fire.
func TestANamedProfileDirectoryIsReadDirectly(t *testing.T) {
	root := t.TempDir()
	store := writeProfile(t, root, "p1", []storeCookie{
		{Host: "assetstore.unity.com", Name: credentialCookie, Value: "from-the-named-profile"},
	})

	got, err := ResolveFrom(filepath.Join(root, "p1"))
	if err != nil {
		t.Fatalf("a profile directory named directly did not resolve: %v", err)
	}
	if got.Path != store {
		t.Errorf("resolved path = %q, want the named profile's own store %q", got.Path, store)
	}
	if !strings.Contains(got.Header, credentialCookie+"=from-the-named-profile") {
		t.Errorf("header %q did not come from the profile that was named", got.Header)
	}
}

// A real recovery.jsonlz4 is highly repetitive JSON, so its block is a chain of
// literal-plus-match sequences. Every other block in this file is one sequence — the
// encoder above emits literals only, and the hand-built cases each stop after a single
// match — so nothing drives the loop across a sequence boundary, where the next token is
// read from wherever the previous match left the cursor. The one test that would,
// TestDecodesARealSessionStore, is opt-in and does not run in CI.
func TestMozLZ4WalksAChainOfSequences(t *testing.T) {
	block := []byte{
		0x40, 'a', 'b', 'c', 'd', 0x04, 0x00, // 4 literals, then 4 bytes from 4 back
		0x21, 'x', 'y', 0x02, 0x00, // 2 literals, then an overlapping 5 from 2 back
		0x30, 'E', 'N', 'D', // the last sequence: literals and no offset
	}
	const want = "abcdabcdxyxyxyxEND"

	raw := append([]byte(mozlz4Magic), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(raw[len(mozlz4Magic):], uint32(len(want)))
	raw = append(raw, block...)

	got, err := decodeMozLZ4(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != want {
		t.Errorf("decoded %q, want %q", got, want)
	}
}

// A profile whose jar holds no unity.com cookie at all is a different failure from one
// that holds them without LS: the user pointed at the wrong browser, or at a profile that
// never visited the store. Saying "no LS cookie" there sends them to re-copy a session
// from a tab that was never open.
func TestASessionStoreForAnotherSiteIsNamedAsSuch(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStoreName)
	raw := mozlz4Stored(t, storeJSON(t, []storeCookie{
		{Host: "bank.example.com", Name: "session", Value: "SHOULD-NOT-LEAK"},
		{Host: "notunity.com", Name: "LS", Value: "SHOULD-NOT-LEAK"},
	}))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveFrom(path)
	if err == nil {
		t.Fatalf("ResolveFrom returned %q for a store with no unity.com cookie", got.Header)
	}
	if !strings.Contains(err.Error(), cookieDomain) {
		t.Errorf("diagnostic %q does not say the file belongs to another site", err)
	}
	if strings.Contains(err.Error(), "SHOULD-NOT-LEAK") {
		t.Errorf("the diagnostic quoted what it read: %q", err)
	}
}

// The extension loop adds 255 per input byte and is bounded only by the end of the block,
// so a token of 0xF0 followed by a long run of 0xFF accumulates a length no output could
// reach. Where int is 32 bits that sum wraps negative, and a negative literal run skips
// the bounds check guarding it — the one path where a corrupt file is not refused but
// quietly decoded as something else.
//
// The wrap itself needs a block of megabytes, so what is pinned here is that the
// accumulation stops as soon as the length is impossible. Every other extended length in
// this file is on a success path, and without this check the refusal comes from the
// literal-run bound instead, which is the same verdict reached after the sum is complete.
func TestMozLZ4RefusesALengthThatOutrunsTheDeclaredOutput(t *testing.T) {
	block := append([]byte{0xF0}, []byte(strings.Repeat("\xff", 64))...)
	block = append(block, 0x00)
	_, err := lz4Decompress(block, 8)
	if err == nil {
		t.Fatal("decode accepted a literal length past the declared output size")
	}
	if !strings.Contains(err.Error(), "declared output size") {
		t.Errorf("refused with %q; the length was not stopped where it became impossible", err)
	}
}

// Every other block in this file encodes its offset as one significant byte followed by a
// zero, so a decoder that read a single byte and advanced two would pass all of them. A
// real recovery.jsonlz4 matches back into a window of hundreds of kilobytes, where the
// high byte is the usual case rather than the exception.
func TestMozLZ4ReadsBothBytesOfAMatchOffset(t *testing.T) {
	literals := make([]byte, 300)
	for i := range literals {
		literals[i] = byte('a' + i%26)
	}
	// Literal nibble 15 plus 0xFF and 30 makes 300; match nibble 4 plus the minimum
	// match makes 8, taken at offset 300 — the whole literal run back.
	block := append([]byte{0xF4, 0xFF, 0x1E}, literals...)
	block = append(block, 0x2C, 0x01)

	want := append(append([]byte{}, literals...), literals[:8]...)
	got, err := lz4Decompress(block, len(want))
	if err != nil {
		t.Fatalf("lz4Decompress: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("decoded %q, want %q", got, want)
	}
}

// writeRestingProfile lays out a profile the way Gecko leaves one after a clean exit: the
// session in sessionstore.jsonlz4 at the profile root, and no sessionstore-backups at all.
func writeRestingProfile(t *testing.T, root, profile string, cookies []storeCookie) string {
	t.Helper()
	dir := filepath.Join(root, profile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cleanShutdownStoreName)
	if err := os.WriteFile(path, mozlz4Stored(t, storeJSON(t, cookies)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Gecko keeps recovery.jsonlz4 only while it is running; on a clean exit it deletes the
// recovery pair and writes the session to sessionstore.jsonlz4 instead. A scan that looks
// for the first alone answers "no Firefox-family session store found" for a user who
// signed in and then quit the browser — LS is a server-side session and is still good —
// which is the exact shape of failure this package exists not to report.
func TestAProfileThatExitedCleanlyIsStillFound(t *testing.T) {
	root := t.TempDir()
	want := writeRestingProfile(t, root, "solo.default", []storeCookie{
		{Host: "assetstore.unity.com", Name: "LS", Value: "cred"},
	})
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"),
		[]byte("[Profile0]\nName=default\nIsRelative=1\nPath=solo.default\nDefault=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if got.Path != want {
		t.Errorf("read from %q, want the clean-shutdown store at %q", got.Path, want)
	}
	if !strings.Contains(got.Header, "LS=cred") {
		t.Errorf("header %q does not carry the credential", got.Header)
	}
}

// The ordering is across roots, not within one: a profile signed in right now beats a
// profile whose last clean exit happened to write a session, whichever browser they
// belong to. resolveBrowser takes the first candidate carrying LS, so a resting file
// offered ahead of a live one hands the store a credential that may have expired while
// the browser that wrote it was closed.
func TestALiveSessionStoreBeatsACleanShutdownOne(t *testing.T) {
	root := t.TempDir()
	writeRestingProfile(t, root, "aaa.resting", []storeCookie{
		{Host: "unity.com", Name: "LS", Value: "stale"},
	})
	live := writeProfile(t, root, "zzz.live", []storeCookie{
		{Host: "unity.com", Name: "LS", Value: "current"},
	})
	// The resting profile is named first, so file order alone would pick it.
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(
		"[Profile0]\nName=resting\nIsRelative=1\nPath=aaa.resting\n"+
			"[Profile1]\nName=live\nIsRelative=1\nPath=zzz.live\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if got.Path != live {
		t.Errorf("read from %q, want the live store at %q", got.Path, live)
	}
	if !strings.Contains(got.Header, "LS=current") {
		t.Errorf("header %q carries the stale credential", got.Header)
	}
}

// encoding/json builds a syntax error by quoting the byte it choked on, and that byte came
// out of a file holding credentials for every host the browsing session touched. The error
// reaches the user through errNoBrowserCredential, which lists what it tried. Wrapping the
// decoder's error with %w is the one path here that would put decoded bytes in a
// diagnostic.
func TestADecodeFailureDoesNotQuoteWhatItRead(t *testing.T) {
	// encoding/json quotes the byte it choked on, which is the first one here. A byte
	// that appears nowhere in the fixed message is the only way to tell a leak from the
	// message itself.
	const marker = 'Z'
	raw := mozlz4Stored(t, []byte{marker, 'n', 'o', 't', ' ', 'j', 's', 'o', 'n'})

	_, err := fromSessionStore(raw)
	if err == nil {
		t.Fatal("a payload that is not JSON was accepted")
	}
	if strings.ContainsRune(err.Error(), marker) {
		t.Errorf("the diagnostic quotes a byte it read out of the store: %q", err)
	}
	// Nothing but the fixed sentence: any wrapping at all is how a decoded byte gets out.
	if err.Error() != errNotSessionStoreJSON.Error() {
		t.Errorf("error = %q, want exactly %q", err, errNotSessionStoreJSON)
	}
}

// The truncated-offset guard is the second of the two standing between a corrupt block and
// a panic: without it binary.LittleEndian.Uint16 reads two bytes out of a one-byte
// remainder. Truncating a literals-only block trips the literal-run bound instead and
// never reaches here, which is why the corrupt-block test does not cover it.
func TestMozLZ4RefusesATruncatedMatchOffset(t *testing.T) {
	// token 0x10: one literal, then a match whose two offset bytes are one byte short.
	if _, err := lz4Decompress([]byte{0x10, 'a', 0x05}, 12); err == nil {
		t.Error("decode accepted a match offset with only one of its two bytes present")
	}
}

// A length extension that runs off the end of the block is its own bound: readLength goes
// on consuming 255s, and without the input check it walks past the slice.
func TestMozLZ4RefusesALengthExtensionThatRunsOffTheBlock(t *testing.T) {
	if _, err := lz4Decompress([]byte{0xF0, 0xFF, 0xFF}, 4096); err == nil {
		t.Error("decode accepted a literal length that runs past the end of the block")
	}
}

// A match is the one step that grows the output without consuming input in proportion, so
// it is where a block can overrun the size its header declared. The declared size is what
// bounds the allocation, so it is checked after every match rather than only at the end.
func TestMozLZ4RefusesOutputThatOverrunsTheDeclaredSize(t *testing.T) {
	// "ab", then a four-byte match at offset 2: eight bytes against a declared four.
	if _, err := lz4Decompress([]byte{0x20, 'a', 'b', 0x02, 0x00, 0x00, 0x00}, 4); err == nil {
		t.Error("decode accepted output that grew past the declared size")
	}
}

// The declared size is a ceiling on what gets allocated, not merely on what gets returned.
// readLength stops its 255-chain once the running total passes want, but the byte that
// ends the chain is added first, so one match can legitimately claim a length near want —
// and appending it before checking grows the output to roughly twice the declared size.
// A block of a few tens of kilobytes reaches it, which is the shape decodeMozLZ4's ceiling
// exists to refuse; a header claiming the full 256 MB would allocate three quarters of a
// gigabyte to decode a 1 MB file.
func TestMozLZ4DoesNotAllocatePastTheDeclaredSizeBeforeRefusing(t *testing.T) {
	const want = 8 << 20

	// The longest match a well-formed length chain can claim: extend while the running
	// total stays inside want, then end the chain with the largest non-255 byte.
	chain := (want - 15) / 255
	block := []byte{0x1F, 'a', 0x01, 0x00} // one literal, extended match, offset 1
	block = append(block, bytes.Repeat([]byte{0xFF}, chain)...)
	block = append(block, 0xFE)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, err := lz4Decompress(block, want)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("decode accepted %d bytes of output against a declared %d", len(out), want)
	}
	// The output buffer is preallocated at want, so anything approaching a second copy of
	// it means the match was appended and only then measured.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > want*3/2 {
		t.Errorf("decoding allocated %d bytes against a declared size of %d; the match was "+
			"copied before the ceiling was applied", grew, want)
	}
}

// Gecko's own document shape, written out by hand instead of marshalled from this
// package's types.
//
// Every other session-store payload in this suite comes from storeJSON, which encodes
// storeCookie — so the json tags and the windows[].cookies path are only ever checked
// against a document this package itself produced. Rename a tag or move the array and the
// whole suite stays green while `session_source = "browser"`, the workflow the README
// leads with, stops finding a credential on every machine there is.
//
// The fields Gecko carries alongside the three that are read are kept, and the keys inside
// each cookie are deliberately out of order, so the reader is held to ignoring both.
const geckoRecoveryDocument = `{
  "version": ["sessionrestore", 1],
  "windows": [
    {
      "tabs": [{"entries": [{"url": "https://unity.com/", "title": "Unity"}], "index": 1}],
      "selected": 1,
      "cookies": [
        {"path": "/", "value": "the-credential", "host": "assetstore.unity.com", "httponly": true, "name": "LS", "originAttributes": {}},
        {"host": ".unity.com", "name": "_csrf", "value": "the-token", "path": "/"},
        {"host": "accounts.google.com", "name": "SID", "value": "an unrelated credential", "path": "/"}
      ],
      "width": 1280
    }
  ],
  "session": {"lastUpdate": 1700000000000},
  "global": {}
}`

func TestARealGeckoDocumentShapeStillYieldsTheCredential(t *testing.T) {
	pairs, err := fromSessionStore(mozlz4Stored(t, []byte(geckoRecoveryDocument)))
	if err != nil {
		t.Fatalf("a document in Gecko's own shape was not read: %v", err)
	}
	for name, want := range map[string]string{"LS": "the-credential", "_csrf": "the-token"} {
		if pairs[name] != want {
			t.Errorf("%s = %q, want %q", name, pairs[name], want)
		}
	}
	// The same file holds every host the browsing session touched, so the filter is what
	// stands between it and the rest of the program.
	if _, ok := pairs["SID"]; ok {
		t.Error("a cookie from outside the unity.com family left the package")
	}
}

// %APPDATA% is a Windows known folder, not a fixed place under the profile: Folder
// Redirection, which is ordinary on a domain-joined machine, moves it off the profile
// entirely. Reconstructing it as <home>/AppData/Roaming is why os.UserConfigDir reads the
// variable instead, and getting it wrong reports "no Firefox-family session store found"
// to a user with a signed-in browser, listing five directories that do not exist.
func TestARedirectedAppDataIsHonoured(t *testing.T) {
	home := filepath.Join("home", "someone")
	redirected := filepath.Join("srv", "profiles", "someone", "AppData", "Roaming")

	roots := geckoRootsFor("windows", home, redirected)
	if len(roots) == 0 {
		t.Fatal("no windows roots")
	}
	for _, r := range roots {
		if !strings.HasPrefix(r, redirected) {
			t.Errorf("windows root %q ignores %%APPDATA%%", r)
		}
	}
	// The fallback still applies when the variable is unset, which is every non-Windows
	// run of this test and a Windows one with a stripped environment.
	plain := geckoRootsFor("windows", home, "")
	for _, r := range plain {
		if !strings.HasPrefix(r, filepath.Join(home, "AppData", "Roaming")) {
			t.Errorf("windows root %q does not fall back under the home directory", r)
		}
	}
	// Order is what decides which account a run reads when two profiles carry LS, so
	// redirection must not reshuffle the list.
	if len(plain) != len(roots) {
		t.Fatalf("redirecting %%APPDATA%% changed the root count: %d vs %d", len(plain), len(roots))
	}
	for i := range plain {
		if filepath.Base(plain[i]) != filepath.Base(roots[i]) {
			t.Errorf("root %d moved: %q became %q", i, plain[i], roots[i])
		}
	}
}
