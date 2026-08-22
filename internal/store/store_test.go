package store_test

import (
	"context"
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
