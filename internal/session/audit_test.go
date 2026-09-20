package session_test

import (
	"errors"
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
