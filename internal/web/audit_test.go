package web_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/web"
)

// Failure models the select page must keep pinned. This page is the account's owned-asset
// list and the only surface that writes the manifest, so both what it shows and what it
// accepts are guarded.

// The per-run token stops a blind cross-origin POST, because a page on another origin
// cannot read the token out of this one. DNS rebinding gets around that: a page the user
// is already on re-resolves its own name to a loopback address, and the browser then
// treats http://attacker.example:8788/ as same-origin *by name*. Its script reads the
// rendered list and the token, then spends the one save this page accepts — so the user's
// own save is refused as a stale tab and their real selection is silently lost.
//
// The Host header is the only thing that separates that request from a real one, so it is
// checked before the render as well as before the save.
func TestAForeignHostIsRefusedBeforeAnythingIsRendered(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	token := tokenFrom(t, render(t, h))

	for _, host := range []string{
		"attacker.example:8788",   // the rebinding case: any name pointed at loopback
		"unity-sync.example:8788", // a plausible-looking one is no better
		"127.0.0.1:9999",          // right address, another run's port
		"10.0.0.5:8788",           // reachable from the network, not from this machine
		"",                        // HTTP/1.0 with no Host at all
	} {
		get := httptest.NewRequest(http.MethodGet, "/", nil)
		get.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, get)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("GET with Host %q = %d, want %d", host, rec.Code, http.StatusMisdirectedRequest)
		}
		for _, leaked := range []string{"Quick Outline", "Fantasy Sounds Bundle", token} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Errorf("the refusal to Host %q still disclosed %q", host, leaked)
			}
		}

		post := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
			url.Values{"token": {token}, "asset": {"193760"}}.Encode()))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		post.Host = host
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, post)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("POST with Host %q = %d, want %d", host, rec.Code, http.StatusMisdirectedRequest)
		}
	}

	// The save was never spent, so the real tab can still use it.
	rec := httptest.NewRecorder()
	go h.ServeHTTP(rec, newRequest(http.MethodPost,
		url.Values{"token": {token}, "asset": {"115488"}}.Encode()))
	if got := <-h.Selection(); !got["115488"] {
		t.Errorf("selection = %v; a refused foreign POST consumed the one accepted save", got)
	}
}

// The other half, and the one that matters more in practice: a guard that refuses the
// ways a browser really does address a page on this machine makes the page unreachable.
func TestTheWaysABrowserAddressesThisPageAreAccepted(t *testing.T) {
	for _, tc := range []struct {
		bound net.Addr
		hosts []string
	}{
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8788},
			[]string{"127.0.0.1:8788", "localhost:8788", "[::1]:8788"}},
		// A wildcard bind has no one address to match, so loopback is what is accepted.
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8788},
			[]string{"127.0.0.1:8788", "localhost:8788"}},
		// On the scheme's default port a browser sends no port at all.
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80},
			[]string{"127.0.0.1", "localhost"}},
		// Bound to a LAN address on purpose, so that address is legitimate too.
		{&net.TCPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 8788},
			[]string{"192.168.1.20:8788", "127.0.0.1:8788"}},
	} {
		for _, host := range tc.hosts {
			h := web.NewHandler(assets(), map[string]bool{"115488": true}, tc.bound)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("bound %s, Host %q = %d, want 200; the page is unreachable this way",
					tc.bound, host, rec.Code)
			}
		}
	}
}
