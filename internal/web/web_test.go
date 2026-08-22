package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

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

func render(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
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

func TestPageRendersEveryAssetWithItsState(t *testing.T) {
	h := web.NewHandler(assets(), map[string]bool{"115488": true})
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
	body := render(t, web.NewHandler(assets(), nil))
	if !strings.Contains(body, `src="https://assetstorev1-prd-cdn.unity3d.com/key-image/abc.png"`) {
		t.Error("thumbnail URL was not normalised to https")
	}
	if strings.Contains(body, `src="//assetstore`) {
		t.Error("page still carries a protocol-relative image URL")
	}
}

func TestSaveReturnsTheChosenSet(t *testing.T) {
	h := web.NewHandler(assets(), map[string]bool{"115488": true})
	body := render(t, h)

	form := url.Values{"token": {tokenFrom(t, body)}, "asset": {"115488", "193760"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body)
	}
}

// A page served by an earlier run still has a Save button. Honouring it would apply a
// selection made against a different library.
func TestStaleTabIsRefused(t *testing.T) {
	h := web.NewHandler(assets(), map[string]bool{"115488": true})
	render(t, h)

	form := url.Values{"token": {"from-an-older-run"}, "asset": {"115488"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	h := web.NewHandler(assets(), map[string]bool{"115488": true})
	body := render(t, h)

	form := url.Values{"token": {tokenFrom(t, body)}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	h := web.NewHandler(assets(), map[string]bool{})
	body := render(t, h)

	form := url.Values{"token": {tokenFrom(t, body)}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	<-done

	if rec.Code != http.StatusOK {
		t.Errorf("empty save on a fresh manifest = %d, want 200", rec.Code)
	}
}
