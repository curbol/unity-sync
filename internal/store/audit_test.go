package store_test

import (
	"context"
	"errors"
	"fmt"
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
	c, srv := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
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
		"Accept":           "application/json",
		"Content-Type":     "application/json;charset=UTF-8",
		"X-Source":         "storefront",
		"Operations":       "SearchMyAssets",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
	for header, want := range map[string]string{
		"Origin":  srv.URL,
		"Referer": srv.URL + "/",
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
	// The bootstrap rewrites the whole Cookie header to swap the token in, and this is
	// the only assertion in the tree made on the far side of that rewrite. LS is the
	// entire credential: dropping it turns every later call into a 500 with an empty
	// GraphqlError, which this client reports as an expired session — so the user is
	// told to re-copy a session that was never the problem.
	if cookie := got.Get("Cookie"); !strings.Contains(cookie, "LS=cred") {
		t.Errorf("Cookie %q lost the credential to the CSRF rewrite", cookie)
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
			// Relative, and so answered by this same server: an absolute Location makes a
			// regression in the redirect guard reach the network before it fails, which
			// turns a deterministic failure into whatever DNS returns.
			w.Header().Set("Location", "/v1/oauth2/authorize")
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

// "identity" asserts the body was not transformed, so it is the one Content-Encoding that
// must be accepted rather than refused. An intermediary that states the negotiated coding
// explicitly — a corporate proxy, a TLS-inspecting appliance, a CDN configured to name it
// — sends it back on a body that is the untouched package. Refusing on non-emptiness alone
// failed every asset in the library behind one of those, unmarked and so retryable, so
// each spent its full budget first and the message blamed the store for what the network
// in front of it did. The offline suite cannot reach the case on its own, since the store
// itself sends no Content-Encoding at all.
func TestAnIdentityContentEncodingIsNotAReEncode(t *testing.T) {
	const pkg = "\x1f\x8b\x08\x00rest-of-a-package"
	for _, enc := range []string{"identity", "Identity", " identity "} {
		t.Run(enc, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Encoding", enc)
				io.WriteString(w, pkg)
			})
			dl, err := c.Fetch(context.Background(), "115488")
			if err != nil {
				t.Fatalf("Fetch refused an untransformed body: %v", err)
			}
			defer dl.Body.Close()
			// Read through, because accepting the response and then handing back
			// something other than the bytes would be the same outcome by another route.
			got, err := io.ReadAll(dl.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != pkg {
				t.Errorf("body = %q, want the package bytes", got)
			}
		})
	}

	// Every other coding is still refused, which is the half that must not weaken: the
	// endpoint honours gzip by gzipping the already-gzipped package.
	for _, enc := range []string{"gzip", "br", "deflate", "identity, gzip"} {
		t.Run("refused/"+enc, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Encoding", enc)
				io.WriteString(w, pkg)
			})
			if _, err := c.Fetch(context.Background(), "115488"); err == nil {
				t.Fatalf("Fetch accepted Content-Encoding %q", enc)
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

// The per-call deadline the API requests carry must never reach a download. It is
// declared beside the stall timeout and applied to the other two request paths, so
// extending it to the shared http.Client or to Fetch's own context reads as a
// consistency fix — and kills every package that takes longer than it, mid-transfer, on
// exactly the slow links the tool exists to survive. The deadline here is far shorter
// than the transfer, so only a download that carries no whole-request bound completes.
func TestADownloadCarriesNoWholeRequestDeadline(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for range 6 {
			time.Sleep(30 * time.Millisecond)
			io.WriteString(w, "chunk")
			w.(http.Flusher).Flush()
		}
	}, store.WithRequestTimeout(50*time.Millisecond), store.WithStallTimeout(2*time.Second))

	dl, err := c.Fetch(context.Background(), "1")
	if err != nil {
		t.Fatalf("Fetch: %v — the API deadline reached the download path", err)
	}
	body, err := io.ReadAll(dl.Body)
	dl.Body.Close()
	if err != nil {
		t.Fatalf("a transfer longer than the API deadline was cut off: %v", err)
	}
	if len(body) != 30 {
		t.Errorf("read %d bytes, want 30", len(body))
	}
}

// The other half of the same rule, checked structurally because no test server can prove
// the absence of a timeout that is hours long. Client.Timeout bounds headers and body
// together and cannot be scoped to the API calls, so the only correct value is zero.
func TestTheSharedClientCarriesNoTimeout(t *testing.T) {
	if d := store.ClientTimeout(store.New("LS=x", "test")); d != 0 {
		t.Errorf("http.Client.Timeout = %v, want 0: a whole-request bound on the shared "+
			"client kills a multi-hour download that is transferring normally", d)
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

// The same rule for the body shapes that carry no message to classify by. A gateway in
// front of the store can answer a 503 with something that parses as a batch and says
// nothing — an empty array, or an operation with neither data nor errors — and deciding
// permanence from the shape alone ends the run on a fault the next attempt would clear.
// The outage would then be fatal or survivable depending on the content type of the error
// page, which is not a distinction the store makes.
func TestABodyShapeWithNoMessageIsStillJudgedByItsStatus(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty batch", `[]`},
		{"neither data nor errors", `[{"data":{}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					io.WriteString(w, tc.body)
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
				t.Errorf("made %d calls, want the 503 to be retried", calls)
			}
		})
	}
}

// The other half: the same shapes under a 200 are the store contradicting itself, and no
// number of attempts fixes that.
func TestABodyShapeWithNoMessageUnderA200IsNotRetried(t *testing.T) {
	var calls int
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enumerate(context.Background()); err == nil {
		t.Fatal("an empty batch under a 200 was accepted")
	}
	if calls != 1 {
		t.Errorf("the store was called %d times for a 200, want 1", calls)
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
	// Exact, not a floor. `issued < 2` passes just as happily on a client that
	// re-bootstraps on every attempt, so deleting the !csrfRetried guard — which is what
	// makes it "exactly one more go" — would leave this green while doubling the request
	// count against a store that persistently mismatches.
	if issued != 2 || posts != 2 {
		t.Errorf("bootstrap ran %d times and posted %d times, want exactly one re-bootstrap "+
			"and one retry (2, 2)", issued, posts)
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
					w.Header().Set("Location", "/oauth2/authorize")
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

// A body that goes quiet after its headers arrive is the failure the response-header
// timeout above cannot see, and the one with the worst blast radius: the read blocks
// forever, so download never returns, so the retry that would open a fresh connection
// never runs and the pool slot is never given up. At the default concurrency of two,
// two such transfers stop a 75 GB mirror with no error, no progress line, and no exit.
func TestAStalledBodyFailsRatherThanBlockingForever(t *testing.T) {
	release := make(chan struct{})
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "the first chunk")
		w.(http.Flusher).Flush()
		<-release // and then nothing, without ending the response
	}, store.WithStallTimeout(100*time.Millisecond))
	defer close(release)

	dl, err := c.Fetch(context.Background(), "1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer dl.Body.Close()

	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(dl.Body); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrStalled) {
			t.Errorf("err = %v, want ErrStalled so the failure names the stall rather than "+
				"reading as an interrupt the user caused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read never returned; a stalled body still hangs the run")
	}
}

// The stall guard is installed on the body Fetch hands back, so it covers the successful
// download and nothing else — and this request deliberately carries no deadline, because a
// 23 GB package legitimately takes hours. That leaves the rejection path: a response whose
// headers are refused still has an open body, and drain reads it so the connection can go
// back to the pool. A captive portal or a proxy that answers 200 text/html, flushes, and
// then goes quiet would otherwise block that read forever, with exactly the blast radius
// the stalled-download case has — Fetch never returns, the pool slot is never given up,
// and two of them stop a run at the default concurrency with no error and no exit.
//
// Neither timeout the client is configured with applies here unless the drain is bounded,
// so the short ones below prove the bound rather than the client.
func TestAStalledBodyOnARejectedResponseDoesNotBlockForever(t *testing.T) {
	release := make(chan struct{})
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// Refused by the content-type guard, so the body is drained rather than returned.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "<html>sign in")
		w.(http.Flusher).Flush()
		<-release // and then nothing, without ending the response
	}, store.WithRequestTimeout(100*time.Millisecond), store.WithStallTimeout(100*time.Millisecond))
	defer close(release)

	done := make(chan error, 1)
	go func() {
		_, err := c.Fetch(context.Background(), "1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Fetch accepted an HTML body as a package")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch never returned; a rejected response whose body stalls still hangs the run")
	}
}

// The other half, and the one that makes the guard safe to have: a transfer that is slow
// but alive must not be cut off. The window resets on every read that returns bytes, so a
// package trickling in over a poor link survives indefinitely — which is the case the
// absence of a whole-request deadline exists to protect.
func TestASlowButLiveBodyIsNotCutOff(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// Eight gaps, each a fifth of the stall window: far longer in total than the
		// window, but never silent for the whole of it.
		//
		// The window is a second rather than the 100ms this used to run at, and the gaps
		// scale with it. The property needs gap < window < total, so the ratio is
		// inherent, but at 30ms against 100ms one sleep overshooting — which -race and
		// parallel package runs both make ordinary — failed the test with "a slow but
		// live body was cut off". That reads as the guard being broken rather than as the
		// scheduling noise it is, which is the worst way for a timing test to fail.
		for range 8 {
			time.Sleep(200 * time.Millisecond)
			io.WriteString(w, "chunk")
			w.(http.Flusher).Flush()
		}
	}, store.WithStallTimeout(time.Second))

	dl, err := c.Fetch(context.Background(), "1")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	body, err := io.ReadAll(dl.Body)
	dl.Body.Close()
	if err != nil {
		t.Fatalf("a slow but live body was cut off: %v", err)
	}
	if len(body) != 40 {
		t.Errorf("read %d bytes, want 40", len(body))
	}
}

// The API calls are bounded end to end, unlike a download: they carry small JSON, so a
// body that stops arriving there is a server that will not finish. Without the deadline
// the enumeration blocks inside its own retry loop, which cannot time it out.
func TestAStalledApiResponseFailsTheCall(t *testing.T) {
	release := make(chan struct{})
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":1,`)
		w.(http.Flusher).Flush()
		<-release
	}), store.WithRequestTimeout(150*time.Millisecond),
		store.WithRetryPolicy(retry.Policy{Attempts: 1}))
	defer close(release)

	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Enumerate(context.Background()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Enumerate succeeded against a response that never finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Enumerate never returned; a stalled API body still hangs the run")
	}
}

// The bootstrap route is the one request whose headers nothing else asserts, and it is
// sent before the token exists — so the session cookie is all it carries. Dropping it
// still yields a _csrf, because the route issues one to anybody, and the token adopted
// would then have been issued against an anonymous context.
func TestTheBootstrapCarriesTheSessionCookie(t *testing.T) {
	var got http.Header
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packages" {
			got = r.Header.Clone()
			http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cookie := got.Get("Cookie"); !strings.Contains(cookie, "LS=cred") {
		t.Errorf("bootstrap Cookie %q does not carry the credential", cookie)
	}
	if got.Get("User-Agent") != "unity-sync/test" {
		t.Errorf("bootstrap User-Agent = %q", got.Get("User-Agent"))
	}
}

// Fetch has five paths that reject a response, and each has to consume and close the body
// before returning. A return that skips it leaks one connection per rejected asset, which
// on a library holding many delisted ones is a steady climb in open sockets with nothing
// failing. Reuse is the only observable: the server sees one remote address rather than
// one per attempt.
//
// What this pins is that the body is dealt with at all, not which helper does it: the
// bodies here are small, and Go's own Close consumes a small body itself. The cap in
// drain is what keeps that from reading a large one.
func TestARejectedDownloadReturnsItsConnectionToThePool(t *testing.T) {
	var addrs []string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		addrs = append(addrs, r.RemoteAddr)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>sign in</html>")
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Fetch(context.Background(), "115488"); err == nil {
			t.Fatal("Fetch accepted an HTML body as a package")
		}
	}
	if len(addrs) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(addrs))
	}
	for _, a := range addrs[1:] {
		if a != addrs[0] {
			t.Errorf("connections were not reused (%v); a rejection left its body undrained", addrs)
			break
		}
	}
}

// Bootstrap is the tool's first network call, made before any output, and it carries a
// per-call deadline for the same reason searchOnce does. Without it a server that sets the
// cookie, flushes its headers and then goes quiet without closing leaves Bootstrap
// succeeding — resp.Cookies() has what it needs — and then blocking forever inside the
// deferred drain. The process hangs with nothing on stdout and nothing to interrupt.
//
// The response-header timeout does not cover this: the headers did arrive.
func TestAStalledBootstrapFailsTheCall(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
		w.WriteHeader(http.StatusNotFound)
		w.(http.Flusher).Flush()
		<-release
	}, store.WithRequestTimeout(150*time.Millisecond))

	done := make(chan error, 1)
	go func() { done <- c.Bootstrap(context.Background()) }()
	select {
	case <-done:
		// Either verdict is fine: the cookie did arrive, so returning nil is correct.
		// What matters is that it returned at all.
	case <-time.After(5 * time.Second):
		t.Fatal("Bootstrap never returned against a response that never finished")
	}
}

// The re-bootstrap can itself fail, and that arm is what turns "csrf token mismatch" —
// which sends the user looking at their session — into a message naming the route that
// stopped issuing tokens. Two regressions hide here: returning the bootstrap error
// instead of the wrapped one silently breaks errors.Is(err, ErrCSRF) for every caller,
// and dropping the retry.Permanent turns one bootstrap outage into a full backoff
// schedule against a route that just proved it is not issuing.
func TestAFailedReBootstrapStillReportsACSRFMismatch(t *testing.T) {
	var issued, posts int
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packages" {
			issued++
			// The first bootstrap works; the re-bootstrap gets nothing.
			if issued == 1 {
				http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts++
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "csrf token mismatch")
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	_, err := c.Enumerate(context.Background())
	if err == nil {
		t.Fatal("Enumerate succeeded against a store that always mismatches")
	}
	if !errors.Is(err, store.ErrCSRF) {
		t.Errorf("err = %v, want it to still unwrap to ErrCSRF", err)
	}
	if !strings.Contains(err.Error(), "re-bootstrap") {
		t.Errorf("err = %q, does not say the re-bootstrap is what failed", err)
	}
	// Permanent, so the backoff schedule does not run against a route that just answered
	// without a token: one post, one re-bootstrap attempt, and no second pass.
	if issued != 2 || posts != 1 {
		t.Errorf("bootstrap ran %d times and posted %d times, want (2, 1): one attempt, "+
			"one failed re-bootstrap, and no retry after it", issued, posts)
	}
}

// The half of the timeout rule that says what *is* bounded. A download carries no
// whole-request deadline by design, so before the first byte arrives this is the only
// thing standing between the run and a server that accepts the connection and never
// answers: the request context has no deadline and the stall guard is not installed until
// the headers are back. Two such connections at the default concurrency stop a run with
// no error and no exit.
//
// Pinned because every test that exercises the header deadline passes
// WithResponseHeaderTimeout to its own client, so all of them prove the option and none
// of them proves the default. Deleting the line in New left the suite green.
func TestTheSharedClientBoundsTheResponseHeaders(t *testing.T) {
	if d := store.ResponseHeaderTimeout(store.New("LS=x", "test")); d <= 0 {
		t.Errorf("transport.ResponseHeaderTimeout = %v, want a positive bound: without one a "+
			"server that never sends headers blocks the download forever, before the stall "+
			"guard exists to catch it", d)
	}
}

// The bootstrap is the first request a run makes, and it used to be the one store call
// with no retry: a 502 from the CDN ended the run before any work was done, while the
// same fault one call later got a full backoff schedule. The failure model has no clause
// for "except the first request".
func TestATransientBootstrapFailureIsRetried(t *testing.T) {
	var attempts int
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		// One bad gateway, then the token. A run that gives up on the first is a run
		// that never reaches enumeration.
		if attempts == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap gave up on a transient failure: %v", err)
	}
	if attempts != 2 {
		t.Errorf("bootstrap made %d attempts, want 2", attempts)
	}
}

// The other half: a route that answers but issues no token is not a transient fault, and
// retrying it spends a full backoff schedule to arrive at the same answer. Its own 404 is
// the normal case, which is exactly why the status cannot be what decides here.
func TestABootstrapRouteThatIssuesNoTokenIsNotRetried(t *testing.T) {
	var attempts int
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.Bootstrap(context.Background()); err == nil {
		t.Fatal("a bootstrap that issued no token was reported as success")
	}
	if attempts != 1 {
		t.Errorf("bootstrap made %d attempts against a route that is not issuing, want 1", attempts)
	}
}

// Credential holds the user's live session. session.Resolved deliberately renders its path
// rather than its header so a stray %v cannot leak one, and that protection was dropped at
// the conversion in main: past it the value was a bare string again. Nothing prints one
// today, which is the point — the route is closed before there is something to find.
func TestPrintingACredentialDoesNotPrintTheSession(t *testing.T) {
	c := store.Credential("LS=the-credential; _csrf=zzz")
	for _, got := range []string{
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%s", c),
		fmt.Sprint(c),
		fmt.Sprintf("%v", fmt.Errorf("building a client: %v", c)),
	} {
		if strings.Contains(got, "the-credential") {
			t.Errorf("printing a Credential rendered the live session: %q", got)
		}
	}
	// Still usable as the header it is, or the redaction would have broken every request.
	if string(c) != "LS=the-credential; _csrf=zzz" {
		t.Error("the underlying header changed")
	}
}
