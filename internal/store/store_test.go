package store_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/retry"
	"github.com/curbol/unity-sync/internal/store"
)

const testCookie = "LS=cred; DS=abc"

func fastRetries() store.Option {
	return store.WithRetryPolicy(retry.Policy{Attempts: 2, Base: time.Millisecond, Sleep: func(time.Duration) {}})
}

// serve wires a handler and returns a bootstrapped client pointed at it.
func serve(t *testing.T, h http.HandlerFunc, opts ...store.Option) (*store.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts = append([]store.Option{store.WithBaseURL(srv.URL), fastRetries()}, opts...)
	return store.New(testCookie, "test", opts...), srv
}

// csrfRouter answers the bootstrap route the way the real store does — 404 with a
// Set-Cookie — and delegates everything else.
func csrfRouter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/packages" {
			http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next(w, r)
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func TestBootstrapAcceptsA404AndRequiresAToken(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap on a 404-that-sets-the-cookie: %v", err)
	}

	// A response that issues no token must fail here rather than guaranteeing ErrCSRF later.
	silent, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if err := silent.Bootstrap(context.Background()); err == nil {
		t.Error("Bootstrap accepted a response that issued no _csrf cookie")
	}
}

// Everywhere else a 3xx means the session died. The bootstrap route is exempt: it is a
// storefront page, and a locale redirect there says nothing about the session.
func TestBootstrapIsExemptFromTheRedirectRule(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_csrf", Value: "issued-token", Path: "/"})
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(http.StatusFound)
	})
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Errorf("Bootstrap on a 3xx that still issued a token: %v", err)
	}
}

func TestEnumerateWalksTheRealFixturesAndDedups(t *testing.T) {
	pages := []string{
		fixture(t, "my_assets_p0.json"),
		fixture(t, "my_assets_p1.json"),
		fixture(t, "my_assets_p2.json"),
	}
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		page := 0
		switch {
		case strings.Contains(string(body), `"page":1`):
			page = 1
		case strings.Contains(string(body), `"page":2`):
			page = 2
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, pages[page])
	}))
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	assets, err := c.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(assets) != 176 {
		t.Fatalf("got %d assets, want 176", len(assets))
	}
	seen := map[string]bool{}
	for _, a := range assets {
		if seen[a.ID] {
			t.Errorf("duplicate product id %s", a.ID)
		}
		seen[a.ID] = true
		if a.Version.ID == "" {
			t.Errorf("asset %s decoded without a version id", a.ID)
		}
	}
	if !seen["115488"] {
		t.Error("known product 115488 missing from the enumeration")
	}
}

func TestEnumerateRefusesAWalkShorterThanTheStoresOwnTotal(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), `"page":0`) {
			io.WriteString(w, `[{"data":{"searchMyAssets":{"total":50,"results":[
				{"product":{"id":"1","name":"A","state":"published","downloadSize":"10",
				 "currentVersion":{"id":"9","name":"1.0"},"publisher":{"id":"p","name":"P"}}}]}}}]`)
			return
		}
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":50,"results":[]}}}]`)
	}))
	c.Bootstrap(context.Background())
	if _, err := c.Enumerate(context.Background()); err == nil {
		t.Error("Enumerate accepted 1 row against a reported total of 50")
	}
}

func TestStrictParsingRejectsAMissingVersionId(t *testing.T) {
	c, _ := serve(t, csrfRouter(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"data":{"searchMyAssets":{"total":1,"results":[
			{"product":{"id":"1","name":"A","state":"published","downloadSize":"10",
			 "currentVersion":{"name":"1.0"},"publisher":{"id":"p","name":"P"}}}]}}}]`)
	}))
	c.Bootstrap(context.Background())
	_, err := c.Enumerate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "currentVersion.id") {
		t.Errorf("Enumerate = %v, want a complaint about the missing diff key", err)
	}
}

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

func TestFetchReturnsTheBodyAndTheStoresFilename(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if enc := r.Header.Get("Accept-Encoding"); enc != "identity" {
			t.Errorf("download sent Accept-Encoding %q; gzip makes the store double-gzip the package", enc)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="Quick Outline.unitypackage"`)
		io.WriteString(w, "\x1f\x8b\x08\x04payload")
	})
	dl, err := c.Fetch(context.Background(), "115488")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer dl.Body.Close()
	if dl.Filename != "Quick Outline.unitypackage" {
		t.Errorf("Filename = %q", dl.Filename)
	}
	got, _ := io.ReadAll(dl.Body)
	if !strings.HasPrefix(string(got), "\x1f\x8b") {
		t.Errorf("body = %q, want the package bytes untouched", got)
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
