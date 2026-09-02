package web_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/web"
)

func assets() []model.Asset {
	return []model.Asset{
		{ID: "115488", Name: "Quick Outline", State: model.StatePublished,
			Publisher: model.Publisher{Name: "Chris Nolet"}, AdvertisedSize: 33824,
			ThumbnailURL: "//assetstorev1-prd-cdn.unity3d.com/key-image/abc.png"},
		{ID: "193760", Name: "Fantasy Sounds Bundle", State: model.StateDisabled,
			Publisher: model.Publisher{Name: "Cafofo"}, AdvertisedSize: 1000},
	}
}

// bound is the address every handler in this suite believes it is serving on, and
// newRequest addresses it that way — as a browser on this machine would. A request built
// any other way is refused, which is what TestTheHostHeaderIsChecked pins.
var bound = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8788}

func newHandler(assets []model.Asset, enabled map[string]bool) *web.Handler {
	return web.NewHandler(assets, enabled, bound)
}

func newRequest(method, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/", nil)
	} else {
		r = httptest.NewRequest(method, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Host = bound.String()
	return r
}

func render(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	return rec.Body.String()
}

var tokenRe = regexp.MustCompile(`name=token value="([0-9a-f]+)"`)

func tokenFrom(t *testing.T, body string) string {
	t.Helper()
	m := tokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("page carries no token")
	}
	return m[1]
}

// listen binds an ephemeral loopback port, so no test here carries a fixed port number
// for another test — or another run — to collide with.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestPageRendersEveryAssetWithItsState(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	body := render(t, h)

	for _, want := range []string{"Quick Outline", "Chris Nolet", "Fantasy Sounds Bundle", "disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if !strings.Contains(body, `value="115488" checked`) {
		t.Error("the already-enabled asset is not checked")
	}
}

// The store returns protocol-relative image URLs; on a page served over http://localhost
// they would resolve to http:// and be blocked or broken.
func TestThumbnailsAreMadeAbsolute(t *testing.T) {
	body := render(t, newHandler(assets(), nil))
	if !strings.Contains(body, `src="https://assetstorev1-prd-cdn.unity3d.com/key-image/abc.png"`) {
		t.Error("thumbnail URL was not normalised to https")
	}
	if strings.Contains(body, `src="//assetstore`) {
		t.Error("page still carries a protocol-relative image URL")
	}
}

// Two tabs on this page carry the same per-run token, so the token alone does not stop a
// second save. Accepting one tells that tab "Saved ..." for a selection Serve has already
// stopped reading — the user is told their choice was kept while the manifest holds the
// other tab's.
func TestOnlyTheFirstSaveIsAccepted(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	token := tokenFrom(t, render(t, h))

	post := func(id string) *httptest.ResponseRecorder {
		form := url.Values{"token": {token}, "asset": {id}}
		req := newRequest(http.MethodPost, form.Encode())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := post("115488")
	if first.Code != http.StatusOK {
		t.Fatalf("first POST = %d: %s", first.Code, first.Body)
	}
	if got := <-h.Selection(); !got["115488"] {
		t.Fatalf("Serve received %v, want the first tab's selection", got)
	}

	second := post("193760")
	if second.Code != http.StatusConflict {
		t.Errorf("second POST = %d, want %d: it must not report a save nobody reads",
			second.Code, http.StatusConflict)
	}
	select {
	case stranded := <-h.Selection():
		t.Errorf("a second selection %v was accepted and stranded in the channel", stranded)
	default:
	}
}

// A page served by an earlier run still has a Save button. Honouring it would apply a
// selection made against a different library.
func TestStaleTabIsRefused(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	render(t, h)

	form := url.Values{"token": {"from-an-older-run"}, "asset": {"115488"}}
	rec := httptest.NewRecorder()
	req := newRequest(http.MethodPost, form.Encode())
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("POST from a stale tab = %d, want %d", rec.Code, http.StatusConflict)
	}
	if !strings.Contains(rec.Body.String(), "earlier run") {
		t.Errorf("response %q does not explain the refusal", rec.Body)
	}
}

// select is the only command that writes the manifest, so clearing every selection at
// once has to be deliberate rather than a mis-click or a reloaded old tab.
func TestSaveThatWouldDeselectEverythingIsRefused(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	body := render(t, h)

	form := url.Values{"token": {tokenFrom(t, body)}}
	rec := httptest.NewRecorder()
	req := newRequest(http.MethodPost, form.Encode())
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("empty save = %d, want %d", rec.Code, http.StatusConflict)
	}
	if !strings.Contains(rec.Body.String(), "deselect everything") {
		t.Errorf("response %q does not explain the refusal", rec.Body)
	}
}

// An empty save is legitimate when nothing was selected to begin with: that is a first
// run where the user decided not to pick anything yet.
func TestEmptySaveIsFineWhenNothingWasSelected(t *testing.T) {
	h := newHandler(assets(), map[string]bool{})
	body := render(t, h)

	form := url.Values{"token": {tokenFrom(t, body)}}
	rec := httptest.NewRecorder()
	req := newRequest(http.MethodPost, form.Encode())

	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	<-done

	if rec.Code != http.StatusOK {
		t.Errorf("empty save on a fresh manifest = %d, want 200", rec.Code)
	}
}

// The 409 body tells the user to reload and choose again, so the page has to still be
// there when they do. The same property is what stops any page the user has open from
// ending a selection with one cross-origin POST.
func TestARefusedSaveLeavesThePageServing(t *testing.T) {
	h := newHandler(assets(), map[string]bool{"115488": true})
	body := render(t, h)

	stale := httptest.NewRecorder()
	h.ServeHTTP(stale, newRequest(http.MethodPost,
		url.Values{"token": {"from-an-older-run"}}.Encode()))
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale POST = %d, want %d", stale.Code, http.StatusConflict)
	}

	// The page is still up, with the same token, and a correct save still lands.
	if got := render(t, h); tokenFrom(t, got) != tokenFrom(t, body) {
		t.Error("the refusal changed the page token")
	}
	saved := httptest.NewRecorder()
	ok := newRequest(http.MethodPost,
		url.Values{"token": {tokenFrom(t, body)}, "asset": {"115488"}}.Encode())
	go h.ServeHTTP(saved, ok)

	select {
	case sel := <-h.Selection():
		if !sel["115488"] {
			t.Errorf("selection = %v, want asset 115488", sel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the save after a refusal never arrived")
	}
}

// The accepted save is delivered on a buffered channel, so Serve becomes runnable the
// instant save() sends and before the handler has written a byte. Closing the server there
// severs the active connection: the browser shows a connection reset for a selection that
// was in fact kept, and a reload-and-retry is then refused as a stale tab, which is
// doubly wrong.
//
// Repeated because the window is a scheduling race rather than a data race, so the race
// detector does not report it and a single pass usually wins it. Under `go test -race`,
// which is what CI runs, the detector's slowdown widens the window enough that this
// catches a regression every time; without it, roughly one run in ten.
func TestTheSaveConfirmationReachesTheBrowser(t *testing.T) {
	// Serve opens a tab every time it is called, and this calls it once per round. The
	// launcher is stubbed for the whole binary in TestMain; the count is what proves the
	// stub is still on the path Serve really takes.
	launchedBefore := web.BrowserLaunches()
	assets := []model.Asset{{ID: "1", Name: "A"}}
	const rounds = 40
	for i := range rounds {
		ln := listen(t)
		base := "http://" + ln.Addr().String()
		type reply struct {
			body string
			err  error
		}
		done := make(chan reply, 1)
		go func() {
			// No retry loop: the listener was bound before Serve was called, so the page
			// answers on the first request.
			resp, err := http.Get(base)
			if err != nil {
				done <- reply{err: err}
				return
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var token string
			const marker = `name=token value="`
			if k := strings.Index(string(page), marker); k >= 0 {
				rest := string(page)[k+len(marker):]
				token = rest[:strings.Index(rest, `"`)]
			}
			resp, err = http.PostForm(base, url.Values{"token": {token}, "asset": {"1"}})
			if err != nil {
				done <- reply{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			done <- reply{body: string(body), err: err}
		}()

		sel, err := web.Serve(context.Background(), ln, assets, map[string]bool{"1": true})
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
		if !sel["1"] {
			t.Fatal("the selection did not come back")
		}
		got := <-done
		if got.err != nil {
			t.Fatalf("round %d: the save that was accepted answered with %v; the server closed "+
				"the connection before the handler finished writing", i, got.err)
		}
		if !strings.Contains(got.body, "Saved") {
			t.Fatalf("round %d: response body = %q, want the save confirmation", i, got.body)
		}
	}
	// Without the stub this loop opens `rounds` real browser tabs on whoever ran the suite.
	if got := web.BrowserLaunches() - launchedBefore; got != rounds {
		t.Errorf("Serve asked to open %d tabs over %d rounds; the stub is no longer on the "+
			"path that would really launch", got, rounds)
	}
}
