package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/curbol/unity-sync/internal/lockfile"
	"github.com/curbol/unity-sync/internal/manifest"
	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/syncer"
)

// Failure models the CLI must keep pinned: ways a command could do something other than
// what the user typed.

// flag.Parse stops at the first positional, so an unchecked one swallows the flags after
// it: `sync foo --dry-run` would download.
func TestStrayPositionalIsRejectedAndSuggestsOnly(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"sync", "status", "list", "select"} {
		code, err := run([]string{cmd, "some-asset", "--dry-run"})
		if code == 0 || err == nil {
			t.Errorf("%s with a positional = %d, %v; want a failure", cmd, code, err)
			continue
		}
		if !strings.Contains(err.Error(), "--only") {
			t.Errorf("%s: error %q does not point at --only", cmd, err)
		}
	}
}

// These return before flag parsing, so their positionals are checked in their own branch
// or not at all. `unity-sync version foo` succeeding quietly is how a typo turns into a
// command that did nothing and said nothing.
func TestTheCommandsThatTakeNoArgumentsRejectOne(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"version", "-v", "--version", "help", "-h", "--help"} {
		code, err := run([]string{cmd, "stray"})
		if code == 0 || err == nil {
			t.Errorf("%s with a positional = %d, %v; want a failure", cmd, code, err)
		}
	}
}

func TestSessionWithoutTheCredentialIsNamedBeforeAnyRequest(t *testing.T) {
	wd := isolate(t)
	if err := os.WriteFile(filepath.Join(wd, manifest.FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(t.TempDir(), "session.curl")
	body := "curl 'https://assetstore.unity.com/' -H 'Cookie: DS=abc; _csrf=zzz'"
	if err := os.WriteFile(sessionPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := run([]string{"status", "--session", sessionPath})
	if code == 0 || err == nil {
		t.Fatalf("status = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "LS") {
		t.Errorf("error %q does not name the missing credential cookie", err)
	}
}

// Without a session every networked command must fail with advice, not a stack trace or
// an opaque HTTP error.
func TestMissingSessionIsExplained(t *testing.T) {
	wd := isolate(t)
	if err := os.WriteFile(filepath.Join(wd, manifest.FileName), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, err := run([]string{"status"})
	if code == 0 || err == nil {
		t.Fatalf("status with no session = %d, %v; want a failure", code, err)
	}
	if !strings.Contains(err.Error(), "session") {
		t.Errorf("error %q does not mention the session", err)
	}
}

// The class tally counts a delisted asset but cannot say which one, and which one is the
// only part the user can act on.
func TestTheSummaryNamesEachDelistedAsset(t *testing.T) {
	buf := &bytes.Buffer{}
	printReport(buf, syncer.Report{
		Owned: 2,
		Results: []syncer.Result{
			{Asset: model.Asset{ID: "1", Name: "Still Fine", State: model.StatePublished}, Class: syncer.Unchanged},
			{Asset: model.Asset{ID: "193760", Name: "Fantasy Sounds Bundle", State: model.StateDisabled}, Class: syncer.Undownloadable},
		},
	}, false, "/lib")
	out := buf.String()
	for _, want := range []string{"Fantasy Sounds Bundle", "193760", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not name %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Still Fine") {
		t.Errorf("the summary named an asset that is not delisted:\n%s", out)
	}
}

// serveStore points run() at a stub Asset Store and reports every request it received.
// Everything below drives the real dispatch — config, session, store.New, Bootstrap and
// the writes that follow — which no other test in this repo reaches.
func serveStore(t *testing.T, h http.HandlerFunc) *[]*http.Request {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []*http.Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		h(w, r)
	}))
	prev := storeBaseURL
	storeBaseURL = srv.URL
	t.Cleanup(func() { storeBaseURL = prev; srv.Close() })
	return &seen
}

// project writes a manifest with one enabled asset and returns its path.
func project(t *testing.T, wd string) string {
	t.Helper()
	path := filepath.Join(wd, manifest.FileName)
	if err := os.WriteFile(path, []byte("[[asset]]\nid = \"115488\"\nname = \"Quick Outline\"\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// session writes a pasted-curl file carrying the credential cookie and points --session
// at it, so the whole session arm runs rather than being stubbed out.
func sessionFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.curl")
	body := "curl 'https://assetstore.unity.com/' -H 'Cookie: LS=the-credential; _csrf=zzz'"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// store.New takes the cookie header and the version as two bare strings in a fixed order,
// so a transposed call compiles and sends the version where the session belongs — and the
// store answers a missing LS with an opaque 500 that reads like a server fault. Nothing
// else in the suite watches the wiring between resolveSession and the client.
func TestTheResolvedSessionIsWhatReachesTheStore(t *testing.T) {
	wd := isolate(t)
	project(t, wd)
	capture(t)
	seen := serveStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "_csrf=issued")
		w.WriteHeader(http.StatusNotFound)
	})

	// Enumeration fails after the bootstrap, which is fine: the assertion is on what the
	// bootstrap request carried.
	run([]string{"status", "--session", sessionFile(t)})

	if len(*seen) == 0 {
		t.Fatal("no request reached the store")
	}
	got := (*seen)[0]
	if cookie := got.Header.Get("Cookie"); !strings.Contains(cookie, "LS=the-credential") {
		t.Errorf("the store was sent Cookie %q, which does not carry the resolved session", cookie)
	}
	if agent := got.Header.Get("User-Agent"); !strings.HasPrefix(agent, "unity-sync/") {
		t.Errorf("User-Agent = %q; the version and the cookie look transposed", agent)
	}
}

// A run against an expired session must leave the committed files exactly as it found
// them. Both writes sit after the store has answered, and this is the only test that
// proves it through the real dispatch rather than through a stub handed to the syncer.
func TestAnExpiredSessionLeavesTheCommittedFilesAlone(t *testing.T) {
	expired := func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "graphql") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `[{"data":null,"errors":[{"errorCode":"GraphqlError","message":""}]}]`)
			return
		}
		w.Header().Set("Set-Cookie", "_csrf=issued")
		w.WriteHeader(http.StatusNotFound)
	}

	for _, cmd := range []string{"sync", "select"} {
		t.Run(cmd, func(t *testing.T) {
			wd := isolate(t)
			capture(t)
			manifestPath := project(t, wd)
			lockPath := manifest.LockPath(manifestPath)
			lf := lockfile.New()
			lf.Assets["quick-outline-115488"] = lockfile.Entry{
				AssetID: "115488", Name: "Quick Outline", Tracked: true,
				ResolvedVersionID: "683375", CachePath: "chris-nolet/quick-outline-115488/quick-outline-115488.unitypackage",
			}
			if err := lockfile.Save(lockPath, lf); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, p := range []string{manifestPath, lockPath} {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				before[p] = b
			}
			serveStore(t, expired)

			code, err := run([]string{cmd, "--session", sessionFile(t)})
			if code == 0 || err == nil {
				t.Fatalf("%s against an expired session = %d, %v; want a failure", cmd, code, err)
			}
			for _, p := range []string{manifestPath, lockPath} {
				after, readErr := os.ReadFile(p)
				if readErr != nil {
					t.Fatalf("%s was removed by a failed run: %v", filepath.Base(p), readErr)
				}
				if !bytes.Equal(before[p], after) {
					t.Errorf("%s was rewritten by a run that never got past the store:\n%s", filepath.Base(p), after)
				}
			}
		})
	}
}
