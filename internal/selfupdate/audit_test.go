package selfupdate_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/curbol/unity-sync/internal/selfupdate"
)

// Failure models this package must keep pinned.

// The private-repo asset API answers with a 302 to a signed CDN URL, so the test server
// really redirects. A client that inherited the Asset Store's redirect ban fails here.
func TestDownloadFollowsTheAssetRedirect(t *testing.T) {
	archive := zipWithBinary(t, "#!/bin/true\n")
	var cdn *httptest.Server
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(archive)
	}))
	defer cdn.Close()

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			name, err := selfupdate.PlatformAsset("9.9.9")
			if err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v9.9.9",
				"assets": []map[string]string{
					{"name": "unrelated.txt", "url": api.URL + "/assets/1"},
					{"name": name, "url": api.URL + "/assets/2"},
				},
			})
		case r.URL.Path == "/assets/2":
			if got := r.Header.Get("Accept"); got != "application/octet-stream" {
				t.Errorf("asset request Accept = %q", got)
			}
			http.Redirect(w, r, cdn.URL+"/signed", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	c := selfupdate.New(api.URL, "token")
	rel, err := c.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	binary, err := c.DownloadBinary(rel)
	if err != nil {
		t.Fatalf("DownloadBinary: %v — a client with the store's redirect ban fails exactly here", err)
	}
	if string(binary) != "#!/bin/true\n" {
		t.Errorf("binary = %q", binary)
	}
}
