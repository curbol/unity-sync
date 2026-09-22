package session_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/session"
)

// Failure models this package must keep pinned. Each case exists because getting it wrong
// fails silently or misdiagnoses the user.

// The credential is HttpOnly. A parser that treats every '#' line as a comment drops
// exactly the cookie that authenticates and then reports "no cookies found".
func TestCookiesTxtKeepsHttpOnlyRecords(t *testing.T) {
	got, err := session.ResolveFrom(write(t, "cookies.txt", cookiesTxt))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(got.Header, "LS=the-credential") {
		t.Errorf("header %q dropped the #HttpOnly_ record", got.Header)
	}
}

// The storefront sets some cookies host-only and others on Domain=.unity.com, and which
// scope LS uses was never established, so both must be accepted.
func TestCookiesTxtAcceptsTheWholeUnityFamilyAndNothingElse(t *testing.T) {
	body := "#HttpOnly_assetstore.unity.com\tFALSE\t/\tTRUE\t0\tLS\thost-only\n" +
		".unity.com\tTRUE\t/\tFALSE\t0\twide\tyes\n" +
		"evil.com\tFALSE\t/\tFALSE\t0\tleaked\tno\n"
	got, err := session.ResolveFrom(write(t, "cookies.txt", body))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, want := range []string{"LS=host-only", "wide=yes"} {
		if !strings.Contains(got.Header, want) {
			t.Errorf("header %q is missing %q", got.Header, want)
		}
	}
	if strings.Contains(got.Header, "leaked") {
		t.Errorf("header %q carries a cookie from an unrelated site", got.Header)
	}
}

func TestMissingCredentialIsNamedBeforeAnyRequest(t *testing.T) {
	body := "assetstore.unity.com\tFALSE\t/\tTRUE\t0\tDS\tabc\n"
	_, err := session.ResolveFrom(write(t, "cookies.txt", body))
	var missing *session.ErrNoCredential
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve = %v, want ErrNoCredential", err)
	}
	if !strings.Contains(err.Error(), "LS") {
		t.Errorf("diagnostic %q does not name the missing cookie", err)
	}
}

// A cookies.txt exported from the wrong site parses perfectly and yields nothing. That is
// not the same failure as a unity.com export missing LS, and reporting it as one sends the
// user to re-copy a session they already have. The diagnostic has to name the site.
func TestACookiesTxtForAnotherSiteIsNamedAsSuch(t *testing.T) {
	body := "#HttpOnly_bank.example.com\tFALSE\t/\tTRUE\t0\tsession\tSHOULD-NOT-LEAK\n" +
		".notunity.com\tTRUE\t/\tFALSE\t0\tLS\tSHOULD-NOT-LEAK\n"
	got, err := session.ResolveFrom(write(t, "cookies.txt", body))
	if err == nil {
		t.Fatalf("ResolveFrom returned %q for a cookies.txt with no unity.com record", got.Header)
	}
	var missing *session.ErrNoCredential
	if errors.As(err, &missing) {
		t.Errorf("a file for another site was reported as a missing LS cookie: %v", err)
	}
	if !strings.Contains(err.Error(), "unity.com") {
		t.Errorf("diagnostic %q does not say the file belongs to another site", err)
	}
	if strings.Contains(err.Error(), "SHOULD-NOT-LEAK") {
		t.Errorf("the diagnostic quoted what it read: %q", err)
	}
}

// The cmd form does not only change the quoting, it rewrites the value: a caret goes in
// front of every byte cmd would otherwise act on, and one more inside %7B so the percent
// cannot start a variable expansion. A reader that takes the value as written hands the
// store a credential with carets in it, which comes back as the opaque 500 that naming
// the missing cookie early exists to avoid.
func TestTheCmdFormsEscapingIsUndoneExactly(t *testing.T) {
	body := "curl ^\"https://assetstore.unity.com/^\" ^\n" +
		"  -H ^\"Cookie: LS=a%^7Bb^|c^&d^^e; _csrf=z^\"\n"
	got, err := session.ResolveFrom(write(t, "session.curl", body))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := "LS=a%7Bb|c&d^e; _csrf=z"; got.Header != want {
		t.Errorf("header = %q, want %q", got.Header, want)
	}
}

// The other half of the same rule: inside POSIX single quotes nothing is escaped, so a
// value that genuinely contains a caret has to survive as itself. Unescaping everywhere
// would corrupt every such value on the platform the tool is mostly used on.
func TestAValueIsNotUnescapedWhereTheShellDoesNotEscape(t *testing.T) {
	body := `curl 'https://assetstore.unity.com/' -H 'Cookie: LS=a^b\c; _csrf=z'`
	got, err := session.ResolveFrom(write(t, "session.curl", body))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := `LS=a^b\c; _csrf=z`; got.Header != want {
		t.Errorf("header = %q, want %q", got.Header, want)
	}
}

// A curl paste that the Cookie reader cannot find must still be reported as a curl paste.
// Falling through to the cookies.txt parser answers "no unity.com cookies found (is this a
// cookies.txt export for the right site?)" for a file that is plainly neither.
func TestAWindowsPasteIsNeverMisreportedAsACookiesTxt(t *testing.T) {
	body := "curl.exe ^\"https://assetstore.unity.com/^\" ^\n  -H ^\"Accept: application/json^\"\n"
	_, err := session.ResolveFrom(write(t, "session.curl", body))
	if err == nil {
		t.Fatal("a curl paste with no Cookie header resolved")
	}
	if strings.Contains(err.Error(), "cookies.txt") {
		t.Errorf("a curl paste was diagnosed as a cookies.txt: %v", err)
	}
}

// -b takes either a cookie string or the name of a jar file to read, and curl tells them
// apart by whether the value holds an "=". Reading a filename as the cookie string sends
// the store a Cookie header whose whole content is a path, which comes back as the same
// opaque 500 a missing LS does — so the one diagnostic the user can act on is replaced by
// one that points at the server.
func TestACookieJarFilenameIsNotReadAsTheCredential(t *testing.T) {
	body := `curl 'https://assetstore.unity.com/' -b cookies.txt`
	_, err := session.ResolveFrom(write(t, "session.curl", body))
	if err == nil {
		t.Fatal("a -b naming a jar file resolved as though it were a cookie string")
	}
	// The discriminator, not a leak check. The filename never reaches a diagnostic on
	// either path, so the condition this replaced ("names cookies.txt but not Cookie")
	// could not fire in either implementation: with the guard deleted the error is
	// "…Cookie header is empty", which contains Cookie and not the filename, and the test
	// passed while guarding nothing. Which of the two messages comes back is the only
	// observable there is — a jar filename holds no "=" by definition, so it can never
	// produce a successful resolve to assert against.
	if !strings.Contains(err.Error(), "no Cookie header") {
		t.Errorf("a -b naming a jar file was read as the cookie string: %v", err)
	}
}

// Firefox switches an argument to $'…' whenever it holds a byte outside printable ASCII,
// a "!" or a "'", and then writes "!" as \041, a byte under 256 as \xNN and anything
// above as \uNNNN. A reader that only drops the backslash and keeps the next byte — which
// is right inside double quotes and wrong here — turns \041 into the three characters
// 041. The cookie is still present under its own name, so the LS assertion passes, and
// the store answers a malformed credential with the same opaque 500 a missing one gets:
// the user is told their session expired and sent to re-copy one that was fine.
//
// Only \' and \\ came out right under the old rule, which is why the form looked like it
// worked and why the spelling table's ansi-c row — which carries no escape at all in its
// payload — did not notice.
func TestEveryAnsiCEscapeFirefoxEmitsIsDecoded(t *testing.T) {
	for _, tc := range []struct{ name, escaped, want string }{
		// The one that actually bites: "!" is %x21, the first octet RFC 6265 admits in a
		// cookie value, and Firefox never writes it literally.
		{"octal exclamation", `LS=a\041b`, "LS=a!b"},
		// Chrome spells the same byte in hex, so both arms earn their keep.
		{"hex exclamation", `LS=a\x21b`, "LS=a!b"},
		{"single quote", `LS=a\'b`, "LS=a'b"},
		{"backslash", `LS=a\\b`, `LS=a\b`},
		{"short octal", `LS=a\41b`, "LS=a!b"},
		// Bash takes one hex digit when the next byte is not one, so the run length has
		// to be driven by what is there rather than fixed at two.
		{"hex one digit", `LS=a\x9zb`, "LS=a\x09zb"},
		{"unicode escape", `LS=aéb`, "LS=aéb"},
		// An escape bash does not recognise keeps the byte and drops the backslash, which
		// is what this did for every byte before. Nothing that read correctly may change.
		{"unrecognised escape", `LS=a\qb`, "LS=aqb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `curl 'https://assetstore.unity.com/' -H $'Cookie: ` + tc.escaped + `'`
			got, err := session.ResolveFrom(write(t, "session.curl", body))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Header != tc.want {
				t.Errorf("header = %q, want %q", got.Header, tc.want)
			}
		})
	}
}

// Resolved carries the live session in an exported field, and String is what keeps the
// default way of printing one safe: it makes %v, %s and %+v — on the value, and on any
// error that wraps it — render the path instead of the credential. The type's own doc
// records that this bug shipped once already, in the two-bare-strings form, and nothing
// asserted the method that replaced it: removing String, switching it to a pointer
// receiver, or adding a second printable field leaves the whole suite green while
// `fmt.Errorf("session: %v", r)` starts writing LS=… to stderr.
func TestPrintingAResolvedSessionDoesNotPrintTheCredential(t *testing.T) {
	r := session.Resolved{Header: "_csrf=stale; LS=the-credential", Path: "/home/u/.config/zen/x/recovery.jsonlz4"}
	for _, format := range []string{"%v", "%s", "%+v"} {
		got := fmt.Sprintf(format, r)
		if got != r.Path {
			t.Errorf("fmt.Sprintf(%q, resolved) = %q, want the path %q", format, got, r.Path)
		}
		if strings.Contains(got, "the-credential") {
			t.Errorf("fmt.Sprintf(%q, resolved) carried the live session: %q", format, got)
		}
	}
	// Wrapped in an error is the shape the bug actually took.
	if err := fmt.Errorf("session: %v", r); strings.Contains(err.Error(), "the-credential") {
		t.Errorf("a wrapped error carried the live session: %v", err)
	}
}
