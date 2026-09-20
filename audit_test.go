package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/config"
	"github.com/curbol/unity-sync/internal/fixtures"
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

// captureStderr redirects os.Stderr for the duration of a test and returns a function
// yielding what was written to it. Progress and the session provenance line go there, and
// what must never go there is the credential.
//
// Restoration runs through Cleanup as well as through the returned function: a t.Fatal or
// a panic inside run would otherwise leave every later test in this binary writing into a
// closed pipe.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	prev := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	var once sync.Once
	restore := func() { once.Do(func() { os.Stderr = prev; w.Close() }) }
	t.Cleanup(func() { restore(); r.Close() })

	return func() string {
		restore()
		var buf bytes.Buffer
		io.Copy(&buf, r)
		return buf.String()
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
	out := capture(t)
	errOut := captureStderr(t)
	seen := serveStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "_csrf=issued")
		w.WriteHeader(http.StatusNotFound)
	})

	// Enumeration fails after the bootstrap, which is fine: the assertion is on what the
	// bootstrap request carried.
	run([]string{"status", "--session", sessionFile(t)})
	stderr := errOut()

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
	// The same value, checked in the other direction: the header is the user's live
	// session, so it belongs in the request and nowhere a terminal, a pipe or a CI log
	// would keep it. Which file the session came from is printed on purpose; what the
	// file contained is not.
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": stderr} {
		if strings.Contains(stream, "the-credential") {
			t.Errorf("the credential was written to %s:\n%s", name, stream)
		}
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
				AssetID: "115488", Name: "Quick Outline",
				Resolution: lockfile.Resolution{
					Tracked:           true,
					ResolvedVersionID: "683375",
					CachePath:         "chris-nolet/quick-outline-115488/quick-outline-115488.unitypackage",
				},
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

			// select binds the address for real, so this asks for an ephemeral port.
			// The default is a fixed one, and a machine already serving on it fails the
			// bind before the enumeration — the failure this test wants, for a reason
			// that has nothing to do with the session.
			code, err := run([]string{cmd, "--session", sessionFile(t), "--addr", "127.0.0.1:0"})
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

// repoDir is captured before any test chdirs away from it, so the committed fixtures
// stay reachable from a test running in a temporary working directory.
var repoDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}()

// serveFixtures answers the bootstrap and then the enumeration from the committed
// fixtures, so a command runs all the way through classification.
func serveFixtures(t *testing.T) {
	t.Helper()
	pages := [][]byte{}
	for _, name := range []string{"my_assets_p0.json", "my_assets_p1.json", "my_assets_p2.json"} {
		// Absolute: isolate() chdirs into a scratch directory before this runs.
		raw, err := os.ReadFile(filepath.Join(repoDir, "testdata", "store", name))
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, raw)
	}
	var mu sync.Mutex
	var served int
	serveStore(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "graphql") {
			w.Header().Set("Set-Cookie", "_csrf=issued")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		page := served
		served++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if page < len(pages) {
			w.Write(pages[page])
			return
		}
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":0,"results":[]}}}]`)
	})
}

// `status` is `sync` with DryRun, and that mapping is one expression in run(). Losing it
// makes `unity-sync status` download packages and rewrite the lockfile — the opposite of
// what the subcommand is for. Every other status test in this file stops at the session
// or the bootstrap, so none of them reaches the gate.
func TestStatusAndDryRunReachClassificationAndStillWriteNothing(t *testing.T) {
	for _, args := range [][]string{{"status"}, {"sync", "--dry-run"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			wd := isolate(t)
			capture(t)
			serveFixtures(t)
			lib := t.TempDir()
			manifestPath := project(t, wd)

			code, err := run(append(append([]string{}, args...),
				"--session", sessionFile(t), "--library", lib))
			if err != nil {
				t.Fatalf("run %v: %v (exit %d)", args, err, code)
			}

			if _, err := os.Stat(manifest.LockPath(manifestPath)); !os.IsNotExist(err) {
				t.Error("a read-only command wrote the lockfile")
			}
			entries, err := os.ReadDir(lib)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("a read-only command wrote %d entries into the library", len(entries))
			}
			before, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(before), "115488") {
				t.Error("a read-only command rewrote the manifest")
			}
		})
	}
}

// A de-owned asset's bytes stay on disk, so the summary naming them is the only signal
// the user gets that a package is now unreferenced. A manifest entry the account does not
// own is the other half: silently ignoring it hides a typo'd id forever.
func TestTheSummaryNamesDroppedAndUnknownAssets(t *testing.T) {
	buf := &bytes.Buffer{}
	printReport(buf, syncer.Report{
		Owned: 1,
		Removed: []lockfile.Entry{
			{Name: "Old Pack", Resolution: lockfile.Resolution{
				CachePath: "pub/old-pack-42/old-pack-42.unitypackage", SizeBytes: 4096}},
			{Name: "Never Mirrored"},
		},
		Unknown: []manifest.Entry{{ID: "404", Name: "Typo'd Entry"}},
	}, false, "/lib")

	out := buf.String()
	for _, want := range []string{
		"Old Pack", "pub/old-pack-42/old-pack-42.unitypackage", "4096", "left in place",
		"Never Mirrored",
		"404", "Typo'd Entry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not name %q:\n%s", want, out)
		}
	}
}

// The select page is the account's purchase history and it carries the token that spends
// the run's one save. Host is written by the client, so a wildcard bind cannot tell a
// browser on this machine from anything that can route to it — the address is the only
// real control, and it is checked before the listener opens.
func TestSelectRefusesAnAddressThatIsNotThisMachine(t *testing.T) {
	for _, addr := range []string{":8788", "0.0.0.0:8788", "[::]:8788"} {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("--addr %q was accepted; the owned-asset list would be served to the network", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8788", "localhost:8788", "[::1]:0", "192.168.1.20:8788"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("--addr %q was refused (%v); the page is unreachable this way", addr, err)
		}
	}
}

// checkLoopback is only a control if run actually calls it. Go does not complain about an
// unused function, so deleting the call — or moving it below the listener — leaves the
// helper here still passing its own test while `select --addr 0.0.0.0:8788` serves the
// owned-asset list and the save token to anything that can route to the machine.
func TestRunRefusesAWildcardSelectAddress(t *testing.T) {
	isolate(t)
	capture(t)
	seen := serveStore(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the store was called for a --addr that should never have been accepted: %s", r.URL)
	})
	for _, addr := range []string{":8788", "0.0.0.0:8788"} {
		code, err := run([]string{"select", "--addr", addr, "--session", sessionFile(t)})
		if err == nil {
			t.Errorf("--addr %q was accepted by run", addr)
		}
		if code == 0 {
			t.Errorf("--addr %q exited 0", addr)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("the refusal came after %d store request(s); it does not depend on the store", len(*seen))
	}
}

// The only manifest write in the program, end to end. The handler and Reconcile are each
// tested alone; what is untested between them is the order — EnabledIDs has to be read
// from the manifest Reconcile already rewrote, or a de-owned asset dropping out reads as
// the user clearing the list and the page refuses their save.
func TestSelectWritesTheSelectionTheBrowserPosted(t *testing.T) {
	wd := isolate(t)
	manifestPath := project(t, wd)
	capture(t)

	owned := []model.Asset{
		ownedAsset("115488", "Quick Outline", "v1", 500),
		ownedAsset("222222", "Another Asset", "v1", 500),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()

	done := make(chan error, 1)
	go func() { done <- selectAssets(context.Background(), &fakeStore{owned: owned}, manifestPath, ln) }()

	body := poll(t, url)
	token := tokenFrom(t, body)
	for _, a := range owned {
		if !strings.Contains(body, a.Name) {
			t.Errorf("the page does not list %q", a.Name)
		}
	}

	form := neturl.Values{"token": {token}, "asset": {"222222"}}
	resp, err := http.PostForm(url, form)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(saved), "Saved") {
		t.Fatalf("the save was refused: %s", saved)
	}
	if err := <-done; err != nil {
		t.Fatalf("selectAssets: %v", err)
	}

	m, err := manifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	enabled := m.EnabledIDs()
	if len(enabled) != 1 || !enabled["222222"] {
		t.Errorf("manifest enables %v, want only the asset the browser posted", enabled)
	}
	// The entry the user deselected stays in the file: it is still owned, and the manifest
	// records what there is to choose from as well as what is chosen.
	kept := false
	for _, e := range m.Assets {
		if e.ID == "115488" {
			kept = true
		}
	}
	if !kept {
		t.Error("the deselected asset was dropped from the manifest instead of disabled")
	}
}

// poll fetches the page until the goroutine running selectAssets has it listening.
func poll(t *testing.T, url string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		resp, err := http.Get(url)
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return string(raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the select page never came up")
	return ""
}

func tokenFrom(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`name=token value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token in the page: %s", body)
	}
	return m[1]
}

// An expired session cancels the pool with hundreds of assets still queued. Naming each
// as its own failure buries the one line the user can act on, so they are summarised —
// and the counter being kept apart from Retryable is only half of that rule: the other
// half is what printReport does with it, which is what the user actually sees.
func TestTheSummaryGivesEveryUnreachedAssetOneLine(t *testing.T) {
	rep := syncer.Report{Owned: 300, NotAttempted: 295}
	for i := range 5 {
		rep.Results = append(rep.Results, syncer.Result{
			Asset: model.Asset{ID: fmt.Sprint(i), Name: "Asset " + fmt.Sprint(i)},
			Class: syncer.New,
		})
	}
	rep.Results[0].Err = errors.New("session expired or missing")

	buf := &bytes.Buffer{}
	printReport(buf, rep, false, "/lib")
	got := buf.String()

	if n := strings.Count(got, "not attempted:"); n != 1 {
		t.Errorf("the summary says \"not attempted\" %d times, want exactly one line for all 295", n)
	}
	if !strings.Contains(got, "295 asset(s)") {
		t.Errorf("the summary does not say how many were never reached:\n%s", got)
	}
	if !strings.Contains(got, "session expired") {
		t.Errorf("the one diagnostic the user can act on is missing:\n%s", got)
	}
}

// A run that could not do what it was asked has to say so in its exit status, and the
// report is the only thing that knows: Run returns a nil error for a failure that fails
// one asset rather than the run, so main's own err != nil check never fires and this
// branch is the whole defence. Delete it and `unity-sync sync && deploy` proceeds on a
// mirror that is missing whatever failed.
//
// The two halves are opposite on purpose. A corrupt body is actionable, so it exits
// non-zero; an asset the store has pulled is permanent, and failing on it would fail every
// future run forever.
func TestTheExitStatusSeparatesAnActionableFailureFromAPulledAsset(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     []byte
		wantCode int
	}{
		{"a corrupt body", []byte("this is not a gzip stream at all"), 1},
		{"an asset the store has pulled", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wd := isolate(t)
			capture(t)
			a := ownedAsset("115488", "Quick Outline", "683375", 500)
			manifestPath := filepath.Join(wd, manifest.FileName)
			if err := manifest.Save(manifestPath, manifest.Manifest{
				Assets: []manifest.Entry{{ID: a.ID, Name: a.Name, Enabled: true}},
			}); err != nil {
				t.Fatal(err)
			}
			fake := &fakeStore{owned: []model.Asset{a}}
			if tc.body != nil {
				fake.bodies = map[string][]byte{a.ID: tc.body}
			}
			cfg := config.Config{LibraryPath: filepath.Join(wd, "library"), Concurrency: 1}

			code, err := syncOrStatus(context.Background(), fake, cfg, manifestPath,
				manifest.LockPath(manifestPath), "", false, false)
			if err != nil {
				t.Fatalf("sync returned an error rather than a report and a code: %v", err)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

// Which profile a browser scan settled on is printed because a run against the wrong
// signed-in account is otherwise completely silent: the enumeration succeeds, the lockfile
// is rewritten against somebody else's owned set, and nothing says why. The existing
// session test points --session at a file, where the source and what was read are the same
// string and this branch never runs.
//
// The credential assertion is repeated here rather than assumed: this is the one path that
// prints anything about the session at all, so it is the one that could print the wrong
// part of it.
func TestABrowserScanSaysWhichProfileItRead(t *testing.T) {
	wd := isolate(t)
	project(t, wd)
	capture(t)

	// A profile directory holding a session store, which is what --session accepts
	// besides the "browser" keyword and a pasted file.
	profile := filepath.Join(t.TempDir(), "abcd.default-release")
	backups := filepath.Join(profile, "sessionstore-backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	jar := `{"windows":[{"cookies":[` +
		`{"host":"assetstore.unity.com","name":"LS","value":"the-credential"},` +
		`{"host":"assetstore.unity.com","name":"_csrf","value":"t"}` +
		`]}]}`
	recovery := filepath.Join(backups, "recovery.jsonlz4")
	if err := os.WriteFile(recovery, fixtures.MozLZ4([]byte(jar)), 0o600); err != nil {
		t.Fatal(err)
	}

	errOut := captureStderr(t)
	serveStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "_csrf=issued")
		w.WriteHeader(http.StatusNotFound)
	})
	// Enumeration fails after the bootstrap, which is fine: the assertion is on what the
	// session resolution said on its way there.
	run([]string{"status", "--session", profile})

	stderr := errOut()
	if !strings.Contains(stderr, "session: read from") {
		t.Errorf("a browser scan did not say which profile it read:\n%s", stderr)
	}
	if !strings.Contains(stderr, recovery) {
		t.Errorf("the provenance line does not name the file it read:\n%s", stderr)
	}
	if strings.Contains(stderr, "the-credential") {
		t.Errorf("the credential was written to stderr:\n%s", stderr)
	}
}
