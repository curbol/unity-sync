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
	got, err := session.Resolve(write(t, "cookies.txt", cookiesTxt))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(got, "LS=the-credential") {
		t.Errorf("header %q dropped the #HttpOnly_ record", got)
	}
}

// The storefront sets some cookies host-only and others on Domain=.unity.com, and which
// scope LS uses was never established, so both must be accepted.
func TestCookiesTxtAcceptsTheWholeUnityFamilyAndNothingElse(t *testing.T) {
	body := "#HttpOnly_assetstore.unity.com\tFALSE\t/\tTRUE\t0\tLS\thost-only\n" +
		".unity.com\tTRUE\t/\tFALSE\t0\twide\tyes\n" +
		"evil.com\tFALSE\t/\tFALSE\t0\tleaked\tno\n"
	got, err := session.Resolve(write(t, "cookies.txt", body))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, want := range []string{"LS=host-only", "wide=yes"} {
		if !strings.Contains(got, want) {
			t.Errorf("header %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "leaked") {
		t.Errorf("header %q carries a cookie from an unrelated site", got)
	}
}

func TestMissingCredentialIsNamedBeforeAnyRequest(t *testing.T) {
	body := "assetstore.unity.com\tFALSE\t/\tTRUE\t0\tDS\tabc\n"
	_, err := session.Resolve(write(t, "cookies.txt", body))
	var missing *session.ErrNoCredential
	if !errors.As(err, &missing) {
		t.Fatalf("Resolve = %v, want ErrNoCredential", err)
	}
	if !strings.Contains(err.Error(), "LS") {
		t.Errorf("diagnostic %q does not name the missing cookie", err)
	}
}
