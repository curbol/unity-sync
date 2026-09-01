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

// Every status Fetch judges as permanent has to reach the caller marked, not just the
// generic ones. A pulled asset is the case that actually happens, and an unmarked 404 is
// three requests and a full backoff for bytes the store will never have again; an unmarked
// 302 retries the one error that is supposed to stop the run at once. Run through retry.Do,
// which is the only public way to observe permanence.
func TestPermanentDownloadStatusesAreNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   error
	}{
		{"forbidden", http.StatusForbidden, nil},
		{"pulled asset", http.StatusNotFound, store.ErrNotDownloadable},
		{"expired session", http.StatusFound, store.ErrExpiredSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "https://id.unity.com/oauth2/authorize")
				}
				w.WriteHeader(tc.status)
			})
			policy := retry.Policy{Attempts: 3, Base: time.Millisecond, Sleep: func(time.Duration) {}}
			err := retry.Do(context.Background(), policy, func(int) error {
				_, err := c.Fetch(context.Background(), "1")
				return err
			})
			if err == nil {
				t.Fatalf("Fetch on a %d = nil, want an error", tc.status)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want it to unwrap to %v", err, tc.want)
			}
			if calls != 1 {
				t.Errorf("the store was called %d times for a %d, want 1", calls, tc.status)
			}
		})
	}

	// A 503 is the opposite case, and proves the test can tell the difference.
	var calls int
	busy, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	policy := retry.Policy{Attempts: 3, Base: time.Millisecond, Sleep: func(time.Duration) {}}
	retry.Do(context.Background(), policy, func(int) error {
		_, err := busy.Fetch(context.Background(), "1")
		return err
	})
	if calls != 3 {
		t.Errorf("a 503 was attempted %d times, want all 3", calls)
	}
}

// Lookup is the discriminator that decides whether a short body was a republish (asset
// skipped, run stays green) or a truncation (asset fails). If the ids filter stopped
// reaching the wire, or the row were taken positionally instead of matched on product id,
// Lookup would answer with the first row of the whole owned list, every truncated download
// would be reported as "republished mid-download; nothing stored", and the run would exit
// 0 forever.
func TestLookupFiltersByIdAndMatchesTheRowItGetsBack(t *testing.T) {
	const wanted = "115488"
	var sent string
	page := func(rows string) http.HandlerFunc {
		return csrfRouter(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			sent = string(body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `[{"data":{"searchMyAssets":{"total":1,"results":[`+rows+`]}}}]`)
		})
	}
	const match = `{"product":{"id":"115488","name":"Quick Outline","state":"published",
		"downloadSize":"4096","currentVersion":{"id":"905463","name":"3.5"},
		"publisher":{"id":"7","name":"Chris Nolet"}}}`

	c, _ := serve(t, page(match))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, ok, err := c.Lookup(context.Background(), wanted)
	if err != nil || !ok {
		t.Fatalf("Lookup = %v, %v, %v; want the product", a, ok, err)
	}
	if !strings.Contains(sent, `"ids":["115488"]`) {
		t.Errorf("request did not carry the ids filter, so the store answered with the whole "+
			"owned list: %s", sent)
	}
	if !strings.Contains(sent, `"pageSize":1`) {
		t.Errorf("request did not ask for a single row: %s", sent)
	}
	if a.ID != wanted || a.Version.ID != "905463" || a.AdvertisedSize != 4096 {
		t.Errorf("asset = %+v, want id %s at version 905463 sized 4096", a, wanted)
	}

	// A response carrying somebody else's product is a miss, not that product. Reported as
	// not-found rather than as an error, because republished() reads an error and a miss
	// the same way: keep the size floor on.
	other, _ := serve(t, page(strings.Replace(match, `"id":"115488"`, `"id":"999999"`, 1)))
	if err := other.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := other.Lookup(context.Background(), wanted)
	if ok || err != nil {
		t.Errorf("Lookup on a foreign row = %v, %v, %v; want a clean miss", got, ok, err)
	}
}

// The walk ends on an empty page and nothing else bounds it, so a store that clamped an
// over-range page to the last valid one would loop here forever: flat memory because of
// the dedup, no output, no error, no timeout. Overshooting the reported total is the same
// broken-pagination symptom a short walk is, and has to be as loud.
func TestEnumerateRefusesAWalkLongerThanTheStoresOwnTotal(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		// Every page answers with the same full page, as a clamping store would.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":2,"results":[
			{"product":{"id":"1","currentVersion":{"id":"v1"},"downloadSize":"10"}},
			{"product":{"id":"2","currentVersion":{"id":"v1"},"downloadSize":"10"}}
		]}}}]`)
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Enumerate(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Enumerate accepted a walk that never ends")
		}
		if !strings.Contains(err.Error(), "paginating") {
			t.Errorf("error %q does not name the pagination problem", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Enumerate never returned: the walk has no upper bound")
	}
}

// The total anchors to the first page, not to whichever page happens to end the walk. The
// page that ends it carries no rows, so its own total is the least load-bearing number in
// the response and the worst one to hold the guard to.
func TestEnumerateAnchorsTheTotalToTheFirstPage(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"page":0`) {
			io.WriteString(w, `[{"data":{"searchMyAssets":{"total":1,"results":[
				{"product":{"id":"1","currentVersion":{"id":"v1"},"downloadSize":"10"}}
			]}}}]`)
			return
		}
		// The overflow page reports the total for its own empty result set.
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":0,"results":[]}}}]`)
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	assets, err := c.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v; a complete walk was refused because the empty page reported "+
			"its own row count as the total", err)
	}
	if len(assets) != 1 {
		t.Errorf("got %d assets, want 1", len(assets))
	}
}
