package session

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

	header, from, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(header, "running-profile") {
		t.Errorf("header came from the Default=1 profile, not the one installs.ini names: %q", from)
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

	header, _, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(header, "relocated-profile") {
		t.Errorf("header %q did not come from the profile profiles.ini names", header)
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

	header, _, err := ResolveFrom(root)
	if err != nil {
		t.Fatalf("ResolveFrom: %v", err)
	}
	if !strings.Contains(header, "LS=credential") {
		t.Errorf("header = %q, want the signed-in profile's credential", header)
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

	_, _, err := ResolveFrom(root)
	if err == nil {
		t.Fatal("ResolveFrom succeeded with no credential anywhere")
	}
	var missing *ErrNoBrowserCredential
	if !asErr(err, &missing) {
		t.Fatalf("error is %T, want *ErrNoBrowserCredential", err)
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
		header, _, err := ResolveFrom(path)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !strings.Contains(header, "LS="+want) {
			t.Errorf("%s produced %q, want LS=%s", filepath.Base(path), header, want)
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
	t.Logf("decoded %d compressed bytes into %d bytes of valid JSON", len(raw), len(decoded))
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

	header, from, err := ResolveFrom(BrowserKeyword)
	if err != nil {
		t.Fatalf("ResolveFrom(%q): %v", BrowserKeyword, err)
	}
	if from != planted {
		t.Errorf("read from %q, want %q", from, planted)
	}
	if !strings.Contains(header, credentialCookie+"=cred") {
		t.Errorf("header %q does not carry the credential", header)
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
	if _, from, err := ResolveFrom(BrowserKeyword); err != nil || from != decoy {
		t.Fatalf("the decoy profile is not discoverable: from=%q err=%v", from, err)
	}

	named := t.TempDir() // an empty profile directory, named explicitly
	_, _, err := ResolveFrom(named)
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
		roots := geckoRootsFor(goos, filepath.Join("home", "someone"))
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

	header, _, err := ResolveFrom(path)
	if err == nil {
		t.Fatalf("ResolveFrom returned %q for a store with no unity.com cookie", header)
	}
	if !strings.Contains(err.Error(), cookieDomain) {
		t.Errorf("diagnostic %q does not say the file belongs to another site", err)
	}
	if strings.Contains(err.Error(), "SHOULD-NOT-LEAK") {
		t.Errorf("the diagnostic quoted what it read: %q", err)
	}
}
