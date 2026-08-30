package store_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/retry"
	"github.com/curbol/unity-sync/internal/store"
)

// Failure models this package must keep pinned: what the client sends, and how it reads a
// response that only looks successful.

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{"csrf mismatch", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "csrf token mismatch")
		}, store.ErrCSRF},
		{"expired session as an empty GraphqlError", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `[{"data":null,"errors":[{"errorCode":"GraphqlError","message":""}]}]`)
		}, store.ErrExpiredSession},
		{"expired session as a redirect", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/errors/unexpected")
			w.WriteHeader(http.StatusFound)
		}, store.ErrExpiredSession},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, csrfRouter(tc.handler))
			c.Bootstrap(context.Background())
			_, err := c.Enumerate(context.Background())
			if !errors.Is(err, tc.want) {
				t.Errorf("Enumerate = %v, want %v", err, tc.want)
			}
		})
	}
}

// A populated errors array with a 200 must never read as "you own nothing", which on a
// first run would look like a legitimate empty library.
func TestPopulatedErrorsArrayIsNotAnEmptyLibrary(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"data":null,"errors":[{"errorCode":"Throttled","message":"slow down"}]}]`)
	}))
	c.Bootstrap(context.Background())
	assets, err := c.Enumerate(context.Background())
	if err == nil {
		t.Fatalf("Enumerate = %d assets, nil error; want an error", len(assets))
	}
	if !strings.Contains(err.Error(), "Throttled") {
		t.Errorf("error %v does not report what the store said", err)
	}
}

// The short-walk check counts raw rows, not deduped assets, because an account can hold
// the same product through more than one entitlement. Comparing the deduped count instead
// reads as a harmless simplification and breaks both ways: a genuinely short page whose
// missing rows were duplicates passes silently, and a complete walk over a duplicated row
// is rejected as short.
func TestDuplicateRowsCountTowardTheStoresTotal(t *testing.T) {
	row := func(id string) string {
		return `{"product":{"id":"` + id + `","name":"Asset ` + id + `","state":"published",` +
			`"downloadSize":"100","currentVersion":{"id":"v1","name":"1.0"},` +
			`"publisher":{"id":"p","name":"Pub"},"mainImage":{"icon75":""}}}`
	}
	page := 0
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		defer func() { page++ }()
		if page == 0 {
			// Three rows, two of them the same product: the store reports 3 owned.
			io.WriteString(w, `[{"data":{"searchMyAssets":{"total":3,"results":[`+
				row("1")+`,`+row("1")+`,`+row("2")+`]}}}]`)
			return
		}
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":3,"results":[]}}}]`)
	}))
	c.Bootstrap(context.Background())

	assets, err := c.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate refused a complete walk that carried a duplicate row: %v", err)
	}
	if len(assets) != 2 {
		t.Errorf("Enumerate returned %d assets, want 2 deduped", len(assets))
	}
}

func TestNonJSONSuccessIsAnError(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>sign in</html>")
	}))
	c.Bootstrap(context.Background())
	if _, err := c.Enumerate(context.Background()); err == nil {
		t.Error("Enumerate parsed an HTML body as a result set")
	}
}

// Every one of these headers is invisible when missing: the request still succeeds
// against a permissive server, so only an assertion catches an omission.
func TestClientSendsTheHeadersTheStoreNeeds(t *testing.T) {
	var got http.Header
	var body string
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":0,"results":[]}}}]`)
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enumerate(context.Background()); err != nil {
		t.Fatal(err)
	}

	for header, want := range map[string]string{
		"X-Requested-With": "XMLHttpRequest",
		"Accept-Encoding":  "identity",
		"User-Agent":       "unity-sync/test",
		"X-Csrf-Token":     "issued-token",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
	if cookie := got.Get("Cookie"); !strings.Contains(cookie, "_csrf=issued-token") {
		t.Errorf("Cookie %q does not carry the token the header claims", cookie)
	}
	if strings.Count(got.Get("Cookie"), "_csrf=") != 1 {
		t.Errorf("Cookie %q has more than one _csrf", got.Get("Cookie"))
	}
	// Losing this field from the document would break every classification silently.
	if !strings.Contains(body, `currentVersion { id name publishedDate }`) {
		t.Error("the pinned query no longer requests currentVersion.id")
	}
}

func TestFetchGuardsTheResponseBeforeAnyBytesAreKept(t *testing.T) {
	pkg := "\x1f\x8b\x08\x00rest-of-a-package"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr error
		wantSub string
	}{
		{"re-encoded body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, pkg)
		}, nil, "re-encoded"},
		{"wrong content type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<html>")
		}, nil, "Content-Type"},
		{"redirect to sign-in", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://api.unity.com/v1/oauth2/authorize")
			w.WriteHeader(http.StatusFound)
		}, store.ErrExpiredSession, ""},
		{"pulled asset", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, store.ErrNotDownloadable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, tc.handler)
			_, err := c.Fetch(context.Background(), "115488")
			if err == nil {
				t.Fatal("Fetch accepted a response it should have refused")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Fetch = %v, want %v", err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Fetch = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// The whole point of a response-header timeout is that a slow *body* is legitimate — a
// 23 GB package takes a while — while a server that never answers is not. Against
// kilobyte fixtures the two policies are indistinguishable, so this pins them apart.
func TestSlowBodyIsAllowedButSlowHeadersAreNot(t *testing.T) {
	slowBody, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for range 4 {
			time.Sleep(30 * time.Millisecond)
			io.WriteString(w, "chunk")
			w.(http.Flusher).Flush()
		}
	}, store.WithResponseHeaderTimeout(50*time.Millisecond))
	dl, err := slowBody.Fetch(context.Background(), "1")
	if err != nil {
		t.Fatalf("Fetch with a slow body: %v — a whole-request timeout would kill real downloads", err)
	}
	body, err := io.ReadAll(dl.Body)
	dl.Body.Close()
	if err != nil {
		t.Fatalf("reading a slow body: %v", err)
	}
	if len(body) != 20 {
		t.Errorf("read %d bytes, want 20", len(body))
	}

	slowHeaders, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/octet-stream")
	}, store.WithResponseHeaderTimeout(50*time.Millisecond))
	if _, err := slowHeaders.Fetch(context.Background(), "1"); err == nil {
		t.Error("Fetch waited indefinitely for headers")
	}
}

// The pinned document is the tool's contract with the store, and every field in it is
// load-bearing: losing currentVersion.id would break every classification silently, and
// adding a field back would start requesting account data the tool has no use for. A
// substring check would miss both, so the whole document is golden.
func TestQueryDocumentMatchesItsGoldenCopy(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "search_query.graphql"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if store.SearchDocument != string(want) {
		t.Errorf("the pinned query document changed.\n--- got ---\n%s\n--- want ---\n%s\n"+
			"If this change is intended, update testdata/search_query.graphql and say why in the "+
			"commit: the field set is a privacy boundary as well as a correctness one.",
			store.SearchDocument, want)
	}
}

// The identity encoding matters most on the download endpoint — that is where asking for
// gzip makes the store gzip an already-gzipped package — so the download's headers get
// their own assertion rather than riding on the GraphQL one.
func TestFetchSendsTheHeadersTheDownloadEndpointNeeds(t *testing.T) {
	var got http.Header
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "\x1f\x8b\x08\x04payload")
	})
	dl, err := c.Fetch(context.Background(), "115488")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	dl.Body.Close()

	for header, want := range map[string]string{
		"Accept-Encoding":  "identity",
		"X-Requested-With": "XMLHttpRequest",
		"User-Agent":       "unity-sync/test",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
	if !strings.Contains(got.Get("Cookie"), "LS=cred") {
		t.Errorf("Cookie %q does not carry the credential", got.Get("Cookie"))
	}
}

// Only the empty-message shape is the session verdict. A 5xx that says what went wrong is
// an ordinary server error, and short-circuiting it would turn a transient outage into an
// immediate failure.
func TestAServerErrorThatExplainsItselfStillRetries(t *testing.T) {
	var calls int
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `[{"data":null,"errors":[{"errorCode":"Backend","message":"upstream timeout"}]}]`)
			return
		}
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":0,"results":[]}}}]`)
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enumerate(context.Background()); err != nil {
		t.Fatalf("Enumerate = %v, want the second attempt to succeed", err)
	}
	if calls < 2 {
		t.Errorf("made %d calls, want the explained 5xx to be retried", calls)
	}
}

// The token can expire between the bootstrap and the call that uses it, so one mismatch is
// worth a re-bootstrap and a second try. A server that always mismatches still reports
// ErrCSRF, which is what the other test pins; this one pins the recovery.
func TestATransientCSRFMismatchRecoversOnce(t *testing.T) {
	var issued, posts int
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packages" {
			issued++
			http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "token", Path: "/"})
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts++
		if posts == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "csrf token mismatch")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":0,"results":[]}}}]`)
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enumerate(context.Background()); err != nil {
		t.Fatalf("Enumerate = %v, want the retry after a re-bootstrap to succeed", err)
	}
	if issued < 2 {
		t.Errorf("bootstrap ran %d times, want a re-bootstrap after the mismatch", issued)
	}
}

// The syncer shares one client across its download pool, and a CSRF mismatch re-bootstraps
// from inside a request, so the credential pair is written while other goroutines read it.
// Without synchronisation this reports a data race under -race; a torn Cookie value on the
// wire would present as an unreproducible mid-sync "session expired".
func TestTheClientIsSafeToShareAcrossTheDownloadPool(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/packages":
			http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "token", Path: "/"})
			w.WriteHeader(http.StatusNotFound)
		case "/api/graphql/batch":
			// Always a mismatch, so every call re-bootstraps mid-flight.
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "csrf token mismatch")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, "\x1f\x8b\x08\x00payload")
		}
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				c.Lookup(context.Background(), "1")
				return
			}
			if dl, err := c.Fetch(context.Background(), "1"); err == nil {
				io.Copy(io.Discard, dl.Body)
				dl.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// A 403 says the same thing on the second attempt, and the download policy's backoff is
// measured in seconds per asset, so the status has to reach the caller as permanent. Run
// through retry.Do, which is the only public way to observe that.
func TestANonRetryableDownloadStatusIsNotRetried(t *testing.T) {
	var calls int
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	})
	policy := retry.Policy{Attempts: 3, Base: time.Millisecond, Sleep: func(time.Duration) {}}
	err := retry.Do(context.Background(), policy, func(int) error {
		_, err := c.Fetch(context.Background(), "1")
		return err
	})
	if err == nil {
		t.Fatal("Fetch on a 403 = nil, want an error")
	}
	if calls != 1 {
		t.Errorf("the store was called %d times for a 403, want 1", calls)
	}

	// A 503 is the opposite case, and proves the test can tell the difference.
	calls = 0
	busy, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	retry.Do(context.Background(), policy, func(int) error {
		_, err := busy.Fetch(context.Background(), "1")
		return err
	})
	if calls != 3 {
		t.Errorf("a 503 was attempted %d times, want all 3", calls)
	}
}
