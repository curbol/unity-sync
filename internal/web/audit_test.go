package web_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/model"
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
		// A wildcard bind never reaches here: main refuses it, because Host alone cannot
		// tell a loopback claim made on this machine from the same claim off the network.
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

// The save is delivered on a buffered channel and the handler writes "Saved …" after it,
// so an interrupt arriving in that window leaves Serve with both select cases ready — and
// Go picks between ready cases at random. Dropping the save there is the same outcome the
// one-save rule exists to prevent: the browser is told the selection was kept while the
// manifest holds nothing.
func TestASaveAlreadyAcceptedSurvivesAnInterrupt(t *testing.T) {
	assets := []model.Asset{{ID: "1", Name: "One"}, {ID: "2", Name: "Two"}}
	for i := 0; i < 50; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		h := web.NewHandler(assets, map[string]bool{"1": true}, ln.Addr())
		ctx, cancel := context.WithCancel(context.Background())

		// Deliver the save and cancel together, which is the window the two cases race in.
		web.Deliver(h, web.Selection{"2": true})
		cancel()

		sel, err := web.ServeWith(ctx, ln, h)
		ln.Close()
		if err != nil {
			t.Fatalf("round %d: an accepted save was discarded: %v", i, err)
		}
		if !sel["2"] {
			t.Fatalf("round %d: Serve returned %v, want the accepted selection", i, sel)
		}
	}
}

// acceptSignal closes seen on the first accepted connection, which is the closest a test
// can get to "the handler has the request in hand".
type acceptSignal struct {
	net.Listener
	once sync.Once
	seen chan struct{}
}

func (l *acceptSignal) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.once.Do(func() { close(l.seen) })
	}
	return c, err
}

// The sibling test covers a save already on the channel when the context ends. This is the
// window one step earlier: the interrupt lands while the handler is still reading the POST
// body off the socket. Serve takes the ctx.Done branch and finds nothing queued, then its
// deferred Shutdown waits for that handler — which goes on to pass the token check, send
// the selection, and answer "Saved …" to the browser.
//
// Returning the interrupt there tells the user their selection was kept while the manifest
// holds the old one, which is the outcome the whole one-save rule exists to prevent.
func TestASaveAcceptedWhileTheInterruptLandsIsStillReturned(t *testing.T) {
	for round := range 5 {
		base, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ln := &acceptSignal{Listener: base, seen: make(chan struct{})}
		h := web.NewHandler(assets(), map[string]bool{"115488": true}, base.Addr())

		// The token comes from the rendered page, the way a browser's would.
		rec := httptest.NewRecorder()
		get := httptest.NewRequest(http.MethodGet, "/", nil)
		get.Host = base.Addr().String()
		h.ServeHTTP(rec, get)
		form := url.Values{"token": {tokenFrom(t, rec.Body.String())}, "asset": {"115488"}}.Encode()

		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			sel web.Selection
			err error
		}
		served := make(chan result, 1)
		go func() {
			sel, err := web.ServeWith(ctx, ln, h)
			served <- result{sel, err}
		}()

		// A body delivered in two parts with a declared length, so ParseForm blocks for
		// the rest and the handler is demonstrably mid-request when the interrupt lands.
		pr, pw := io.Pipe()
		req, err := http.NewRequest(http.MethodPost, "http://"+base.Addr().String()+"/", pr)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.ContentLength = int64(len(form))
		postBody := make(chan string, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				postBody <- "ERR: " + err.Error()
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			postBody <- resp.Status + " | " + string(b)
		}()

		<-ln.seen
		cut := len(form) - 3
		pw.Write([]byte(form[:cut]))
		// Long enough for the server to have read the request line and headers and called
		// the handler, which then blocks in ParseForm waiting for the declared remainder.
		// Interrupting before that leaves the connection idle, and Shutdown closes an idle
		// connection rather than waiting for it — a different case from the one under test.
		time.Sleep(50 * time.Millisecond)
		cancel()
		// Long enough for Serve to have taken the ctx.Done branch and found nothing.
		time.Sleep(25 * time.Millisecond)
		pw.Write([]byte(form[cut:]))
		pw.Close()

		select {
		case got := <-served:
			if got.err != nil {
				t.Fatalf("round %d: Serve returned %v while the page was answered %q",
					round, got.err, <-postBody)
			}
			if !got.sel["115488"] {
				t.Fatalf("round %d: selection = %v, want the accepted save", round, got.sel)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Serve never returned", round)
		}
		base.Close()
	}
}
