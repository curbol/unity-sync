package store_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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
