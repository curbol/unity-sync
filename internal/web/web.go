// Package web serves the local page behind `unity-sync select`: every owned asset with
// its thumbnail and a checkbox, returning the chosen set so the caller can persist the
// manifest. It is the only command that writes the manifest, so the page is also where
// the guards against clobbering a curated file live.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"github.com/curbol/unity-sync/internal/model"
)

// ErrWouldEmptySelection is returned when a save would clear every selection at once.
// A stale tab reopened after the library changed, or a mis-click on "none", should not
// silently wipe a curated allowlist.
var ErrWouldEmptySelection = errors.New("refusing a save that would deselect everything")

// ErrStaleTab is returned when a POST does not carry this run's token, which means it
// came from a page some earlier run served.
var ErrStaleTab = errors.New("this page was served by an earlier run; reload and choose again")

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

	done chan Selection
}

// Selection delivers the one accepted save, so a caller without a socket can wait on the
// same channel Serve does.
func (h *Handler) Selection() <-chan Selection { return h.done }

// NewHandler builds the page handler for one run.
func NewHandler(assets []model.Asset, enabled map[string]bool) *Handler {
	buf := make([]byte, 16)
	rand.Read(buf)
	return &Handler{
		assets:  assets,
		enabled: enabled,
		token:   hex.EncodeToString(buf),
		done:    make(chan Selection, 1),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			Size:      humanBytes(a.AdvertisedSize),
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
		http.Error(w, ErrStaleTab.Error(), http.StatusConflict)
		return
	}
	chosen := Selection{}
	for _, id := range r.PostForm["asset"] {
		chosen[id] = true
	}
	if len(chosen) == 0 && anyEnabled(h.enabled) {
		http.Error(w, ErrWouldEmptySelection.Error(), http.StatusConflict)
		return
	}
	fmt.Fprintf(w, "Saved %d selection(s). You can close this tab.", len(chosen))
	h.done <- chosen
}

// Serve runs the page until it is saved once or the context ends.
func Serve(ctx context.Context, addr string, assets []model.Asset, enabled map[string]bool) (Selection, error) {
	h := NewHandler(assets, enabled)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()

	url := "http://" + ln.Addr().String()
	fmt.Println("select assets at", url)
	openBrowser(url)

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

func openBrowser(url string) {
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

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGT"[exp])
}
