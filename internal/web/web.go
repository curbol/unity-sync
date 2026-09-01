// Package web serves the local page behind `unity-sync select`: every owned asset with
// its thumbnail and a checkbox, returning the chosen set so the caller can persist the
// manifest. It is the only command that writes the manifest, so the page is also where
// the guards against clobbering a curated file live.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/curbol/unity-sync/internal/humanize"
	"github.com/curbol/unity-sync/internal/model"
)

// The two refusals, as the page states them. A save that would clear every selection at
// once, and a POST from a page some earlier run served: a stale tab reopened after the
// library changed, or a mis-click on "none", should not silently wipe a curated allowlist.
const (
	msgWouldEmptySelection = "refusing a save that would deselect everything"
	msgStaleTab            = "this page was served by an earlier run; reload and choose again"
	msgForeignHost         = "this page is only served to a browser on this machine"
)

// shutdownGrace bounds how long Serve waits for the in-flight save to be written before
// it stops waiting. The response is one short line, so reaching this means the client
// stopped reading rather than that the write is slow.
const shutdownGrace = 5 * time.Second

type row struct {
	ID        string
	Name      string
	Publisher string
	Size      string
	State     string
	Thumb     string
	Enabled   bool
}

type pageData struct {
	Rows  []row
	Token string
	Count int
}

var page = template.Must(template.New("select").Parse(`<!doctype html>
<meta charset="utf-8"><title>unity-sync select</title>
<style>
 body{font:14px/1.5 system-ui,sans-serif;margin:0;background:#14161a;color:#e6e6e6}
 header{position:sticky;top:0;background:#14161a;border-bottom:1px solid #2a2f38;padding:12px 16px;display:flex;gap:12px;align-items:center}
 input[type=search]{flex:1;padding:6px 10px;background:#1c2027;border:1px solid #2a2f38;color:inherit;border-radius:6px}
 button{padding:6px 12px;background:#2b6cb0;color:#fff;border:0;border-radius:6px;cursor:pointer}
 button.ghost{background:#2a2f38}
 .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:10px;padding:16px}
 .card{display:flex;gap:10px;padding:8px;background:#1c2027;border:1px solid #2a2f38;border-radius:8px}
 .card img{width:56px;height:56px;object-fit:cover;border-radius:4px;background:#2a2f38}
 .meta{min-width:0}
 .name{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
 .sub{color:#9aa4b2;font-size:12px}
 .deprecated{color:#d69e2e}.disabled{color:#e53e3e}
</style>
<header>
  <strong>unity-sync</strong>
  <input type=search placeholder="filter by name or publisher" oninput="flt(this.value)">
  <span><b id=n>{{.Count}}</b> selected</span>
  <button class=ghost type=button onclick="all(true)">all</button>
  <button class=ghost type=button onclick="all(false)">none</button>
  <button form=f>Save</button>
</header>
<form id=f method=post>
<input type=hidden name=token value="{{.Token}}">
<div class=grid>
{{range .Rows}}
  <label class=card data-hay="{{.Name}} {{.Publisher}}">
    <input type=checkbox name=asset value="{{.ID}}" {{if .Enabled}}checked{{end}} onchange=tally()>
    <img loading=lazy src="{{.Thumb}}" alt="">
    <span class=meta>
      <span class=name>{{.Name}}</span><br>
      <span class=sub>{{.Publisher}} &middot; {{.Size}}{{if ne .State "published"}} &middot; <span class="{{.State}}">{{.State}}</span>{{end}}</span>
    </span>
  </label>
{{end}}
</div>
</form>
<script>
 function tally(){document.getElementById('n').textContent=document.querySelectorAll('input[name=asset]:checked').length}
 function all(v){document.querySelectorAll('.card').forEach(c=>{if(c.style.display!=='none')c.querySelector('input').checked=v});tally()}
 function flt(q){q=q.toLowerCase();document.querySelectorAll('.card').forEach(c=>{c.style.display=c.dataset.hay.toLowerCase().includes(q)?'':'none'})}
</script>
`))

// Selection is what the page returned.
type Selection map[string]bool

// Handler renders the page and accepts one save. It is separated from Serve so the
// behaviour can be tested without a socket.
//
// A refused save is answered and nothing more: the page stays up so the user can correct
// the mistake the refusal describes.
type Handler struct {
	assets  []model.Asset
	enabled map[string]bool
	token   string

	// bound is the address this page is served on, checked against every request's Host.
	bound net.Addr

	// One save, enforced rather than assumed. Two tabs on this page share the per-run
	// token, so without this the second POST is answered "Saved ..." and its selection
	// then sits in a channel Serve has already stopped reading.
	once sync.Once
	done chan Selection
}

// Selection delivers the one accepted save, so a caller without a socket can wait on the
// same channel Serve does.
func (h *Handler) Selection() <-chan Selection { return h.done }

// NewHandler builds the page handler for one run. bound is the address the page is
// served on, which every request is checked against.
func NewHandler(assets []model.Asset, enabled map[string]bool, bound net.Addr) *Handler {
	buf := make([]byte, 16)
	rand.Read(buf)
	return &Handler{
		assets:  assets,
		enabled: enabled,
		token:   hex.EncodeToString(buf),
		bound:   bound,
		done:    make(chan Selection, 1),
	}
}

// localRequest reports whether a request addressed this page the way a browser on this
// machine would. The per-run token stops a blind cross-origin POST, but not a page that
// points its own name at a loopback address: to the browser that is same-origin by name,
// so its script may read the rendered page — the whole owned-asset list, and the token —
// and then spend the one save this page accepts, leaving the user's own save refused as
// a stale tab and their real selection lost.
func localRequest(r *http.Request, bound net.Addr) bool {
	if bound == nil {
		return false
	}
	// A browser omits the port when it is the scheme's default, so a page bound to :80
	// arrives with a bare name rather than a host:port pair.
	host, port := r.Host, "80"
	if h, p, err := net.SplitHostPort(r.Host); err == nil {
		host, port = h, p
	}
	host = strings.Trim(host, "[]")

	boundHost, boundPort, err := net.SplitHostPort(bound.String())
	if err != nil || port != boundPort {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// A wildcard bind has no one address to match, so only loopback is accepted there.
	if ip.IsLoopback() {
		return true
	}
	boundIP := net.ParseIP(boundHost)
	return boundIP != nil && !boundIP.IsUnspecified() && boundIP.Equal(ip)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Before the render as well as before the save: the page itself is the account's
	// owned-asset list, and it carries the token that makes a save possible.
	if !localRequest(r, h.bound) {
		http.Error(w, msgForeignHost, http.StatusMisdirectedRequest)
		return
	}
	if r.Method == http.MethodPost {
		h.save(w, r)
		return
	}
	sorted := append([]model.Asset(nil), h.assets...)
	sort.Slice(sorted, func(i, j int) bool { return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name) })

	data := pageData{Token: h.token}
	for _, a := range sorted {
		if h.enabled[a.ID] {
			data.Count++
		}
		data.Rows = append(data.Rows, row{
			ID:        a.ID,
			Name:      a.Name,
			Publisher: a.Publisher.Name,
			Size:      humanize.Bytes(a.AdvertisedSize),
			State:     string(a.State),
			Thumb:     absoluteURL(a.ThumbnailURL),
			Enabled:   h.enabled[a.ID],
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page.Execute(w, data)
}

func (h *Handler) save(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("token") != h.token {
		http.Error(w, msgStaleTab, http.StatusConflict)
		return
	}
	chosen := Selection{}
	for _, id := range r.PostForm["asset"] {
		chosen[id] = true
	}
	if len(chosen) == 0 && anyEnabled(h.enabled) {
		http.Error(w, msgWouldEmptySelection, http.StatusConflict)
		return
	}
	accepted := false
	h.once.Do(func() {
		accepted = true
		h.done <- chosen
	})
	if !accepted {
		http.Error(w, msgStaleTab, http.StatusConflict)
		return
	}
	fmt.Fprintf(w, "Saved %d selection(s). You can close this tab.", len(chosen))
}

// Serve runs the page on ln until it is saved once or the context ends.
//
// It takes a bound listener rather than an address so the caller decides where the page
// lives and when the port is claimed — failing on a port already in use before a run
// spends an enumeration on it — and so a test can hand it an ephemeral port instead of
// carrying a fixed number that another test can collide with.
//
// It opens a browser at the address ln is bound to, so a test driving it must stub
// OpenBrowser first.
func Serve(ctx context.Context, ln net.Listener, assets []model.Asset, enabled map[string]bool) (Selection, error) {
	h := NewHandler(assets, enabled, ln.Addr())
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	// Shutdown, not Close. The accepted save is delivered on a buffered channel, so this
	// function is runnable the instant save() sends and before the handler has written a
	// byte; Close severs the active connection and the browser shows a connection reset
	// for a selection that was in fact kept. Shutdown waits for the handler to finish.
	defer func() {
		// Background, not the caller's ctx: the usual reason Serve is returning is that
		// ctx ended, and shutting down under an already-cancelled context severs the
		// connection exactly as Close would.
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			srv.Close()
		}
	}()

	url := "http://" + ln.Addr().String()
	fmt.Fprintln(os.Stderr, "select assets at", url)
	OpenBrowser(url)

	select {
	case sel := <-h.done:
		return sel, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// absoluteURL fixes the store's protocol-relative image URLs, which would otherwise
// resolve to http:// on a page served from localhost.
func absoluteURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

func anyEnabled(m map[string]bool) bool {
	for _, v := range m {
		if v {
			return true
		}
	}
	return false
}

// OpenBrowser launches the page. It is an exported variable so that tests in any package
// can stop it: Serve's whole job is to open a tab, so a test that drives Serve opens a
// real one per call on whoever is running the suite, at a URL that dies with the test.
var OpenBrowser = func(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	default:
		cmd = "xdg-open"
	}
	exec.Command(cmd, url).Start()
}
