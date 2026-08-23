// Package store is the Asset Store client: the batched GraphQL endpoint that lists what
// an account owns, and the endpoint that serves package bytes. It owns the
// response-level guards — refusing redirects, refusing a re-encoded body, checking the
// content type — and hands the body to the caller unbuffered, because a package here can
// be 23 GB. The checks that need the enumeration metadata belong to the syncer.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/curbol/unity-sync/internal/model"
	"github.com/curbol/unity-sync/internal/retry"
)

const (
	defaultBase = "https://assetstore.unity.com"

	// csrfRoute issues the _csrf cookie. Not every storefront path does: "/" and
	// "/publishers/{id}" answer 200 and set nothing, while this one answers 404 and
	// sets the token. The uncached 404 routes are the ones that issue it.
	csrfRoute = "/packages"

	graphQLPath  = "/api/graphql/batch"
	downloadPath = "/api/downloads/"

	pageSize = 100
)

var (
	// ErrExpiredSession is the one failure a user can act on. The store signals it two
	// different ways: an empty GraphqlError inside an HTTP 500 on the API, and a 302 to
	// the OAuth authorize URL on the download endpoint.
	ErrExpiredSession = errors.New("session expired or missing; re-copy it from a signed-in browser")

	// ErrCSRF means the double-submit token did not match, i.e. the bootstrap failed.
	ErrCSRF = errors.New("csrf token mismatch")

	// ErrNotDownloadable means the store has no bytes for this product any more. It is
	// permanent: no re-run changes it.
	ErrNotDownloadable = errors.New("asset is not downloadable")
)

// SearchDocument is pinned. Its field set is the tool's contract with the store: it asks
// for no account-identifying field, and currentVersion.id is mandatory, being the key
// every classification diffs on.
const SearchDocument = `query SearchMyAssets($page: Int, $pageSize: Int, $ids: [String!]) {
  searchMyAssets(page: $page, pageSize: $pageSize, ids: $ids) {
    total
    results {
      product {
        id
        name
        state
        downloadSize
        currentVersion { id name publishedDate }
        publisher { id name }
        mainImage { icon75 }
      }
    }
  }
}`

// Client talks to one Asset Store host.
type Client struct {
	http    *http.Client
	base    string
	agent   string
	retries retry.Policy

	// A CSRF mismatch re-bootstraps mid-flight, and the syncer's download pool shares one
	// client, so these two are written while other goroutines are reading them.
	mu     sync.RWMutex
	cookie string
	csrf   string
}

// credentials reads the pair that every request carries.
func (c *Client) credentials() (cookie, csrf string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cookie, c.csrf
}

// adoptCSRF folds a freshly issued token into both the header and the cookie jar.
func (c *Client) adoptCSRF(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.csrf = token
	c.cookie = withCSRF(c.cookie, token)
}

// Option adjusts a Client for tests.
type Option func(*Client)

// WithBaseURL points the client at a test server.
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimSuffix(u, "/") } }

// WithRetryPolicy replaces the backoff policy, so tests need not sleep.
func WithRetryPolicy(p retry.Policy) Option { return func(c *Client) { c.retries = p } }

// WithResponseHeaderTimeout shortens the header deadline so a test can prove the
// difference between bounding the headers and bounding the whole transfer.
func WithResponseHeaderTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.http.Transport.(*http.Transport).ResponseHeaderTimeout = d
	}
}

// New builds a client for the given session Cookie header.
//
// The transport sets a response-header timeout rather than a whole-request timeout: a
// 23 GB body legitimately takes a long time, and a request deadline would kill it, while
// a server that never answers still needs bounding.
func New(cookieHeader, version string, opts ...Option) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 60 * time.Second
	c := &Client{
		http: &http.Client{
			Transport: transport,
			// No store request may follow a redirect: an unauthenticated download 302s
			// to Unity's OAuth page, and following it would write a sign-in page into
			// the cache under a .unitypackage name.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base:    defaultBase,
		cookie:  cookieHeader,
		agent:   "unity-sync/" + version,
		retries: retry.DefaultPolicy(),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Bootstrap obtains the _csrf token the GraphQL endpoint requires and folds it into the
// cookie header. Its own route answers 404 by design, so a non-2xx here is normal and a
// 3xx is not the expired-session signal it is everywhere else — but a response that
// issues no token is a hard failure, since proceeding guarantees ErrCSRF.
func (c *Client) Bootstrap(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+csrfRoute, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "text/html,*/*")
	cookie, _ := c.credentials()
	req.Header.Set("Cookie", cookie)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("csrf bootstrap: %w", err)
	}
	defer drain(resp)

	for _, ck := range resp.Cookies() {
		if ck.Name == "_csrf" && ck.Value != "" {
			c.adoptCSRF(ck.Value)
			return nil
		}
	}
	return fmt.Errorf("csrf bootstrap: %s issued no _csrf cookie (status %d)", csrfRoute, resp.StatusCode)
}

// Enumerate walks every page of owned assets. It compares the raw row count against the
// store's own total *before* deduplicating, so a duplicate entitlement row cannot look
// like a short page, then dedups by product id.
func (c *Client) Enumerate(ctx context.Context) ([]model.Asset, error) {
	var (
		assets  []model.Asset
		seen    = map[string]bool{}
		rawRows int
		total   = -1
	)
	for page := 0; ; page++ {
		res, err := c.search(ctx, map[string]any{"page": page, "pageSize": pageSize})
		if err != nil {
			return nil, err
		}
		total = res.Total
		if len(res.Results) == 0 {
			break
		}
		rawRows += len(res.Results)
		for _, row := range res.Results {
			a, err := row.Product.asset()
			if err != nil {
				return nil, fmt.Errorf("page %d: %w", page, err)
			}
			if seen[a.ID] {
				continue
			}
			seen[a.ID] = true
			assets = append(assets, a)
		}
	}
	if total >= 0 && rawRows != total {
		return nil, fmt.Errorf("enumeration collected %d rows but the store reports %d owned; "+
			"refusing to treat a short walk as the truth", rawRows, total)
	}
	return assets, nil
}

// Lookup re-reads one product through the same pinned document, using the ids filter. It
// is the discriminator for a short download: if the advertised version or size has moved
// since enumeration, the publisher republished mid-transfer.
func (c *Client) Lookup(ctx context.Context, id string) (model.Asset, bool, error) {
	res, err := c.search(ctx, map[string]any{"page": 0, "pageSize": 1, "ids": []string{id}})
	if err != nil {
		return model.Asset{}, false, err
	}
	for _, row := range res.Results {
		if row.Product.ID == id {
			a, err := row.Product.asset()
			return a, err == nil, err
		}
	}
	return model.Asset{}, false, nil
}

type searchResult struct {
	Total   int `json:"total"`
	Results []struct {
		Product product `json:"product"`
	} `json:"results"`
}

type product struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	State          string `json:"state"`
	DownloadSize   string `json:"downloadSize"`
	CurrentVersion struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		PublishedDate string `json:"publishedDate"`
	} `json:"currentVersion"`
	Publisher struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"publisher"`
	MainImage struct {
		Icon75 string `json:"icon75"`
	} `json:"mainImage"`
}

// asset converts a decoded row, refusing anything the diff depends on being absent
// rather than defaulting it.
func (p product) asset() (model.Asset, error) {
	if p.ID == "" {
		return model.Asset{}, fmt.Errorf("product row has no id")
	}
	if p.CurrentVersion.ID == "" {
		return model.Asset{}, fmt.Errorf("product %s has no currentVersion.id, which is the diff key", p.ID)
	}
	size, err := strconv.ParseInt(p.DownloadSize, 10, 64)
	if err != nil && p.DownloadSize != "" {
		return model.Asset{}, fmt.Errorf("product %s has unparseable downloadSize %q: %w", p.ID, p.DownloadSize, err)
	}
	return model.Asset{
		ID:        p.ID,
		Name:      p.Name,
		State:     model.State(p.State),
		Publisher: model.Publisher{ID: p.Publisher.ID, Name: p.Publisher.Name},
		Version: model.Version{
			ID:            p.CurrentVersion.ID,
			Name:          p.CurrentVersion.Name,
			PublishedDate: p.CurrentVersion.PublishedDate,
		},
		AdvertisedSize: size,
		ThumbnailURL:   p.MainImage.Icon75,
	}, nil
}

type graphQLError struct {
	ErrorCode string `json:"errorCode"`
	Message   string `json:"message"`
}

func (c *Client) search(ctx context.Context, vars map[string]any) (searchResult, error) {
	var out searchResult
	var csrfRetried bool
	err := retry.Do(ctx, c.retries, func(int) error {
		res, err := c.searchOnce(ctx, vars)
		if err == nil {
			out = res
			return nil
		}
		// A stale token is worth exactly one more go: re-bootstrap and retry, since the
		// token can expire between the bootstrap and the call that uses it. A second
		// mismatch is a real problem and is reported.
		if errors.Is(err, ErrCSRF) && !csrfRetried {
			csrfRetried = true
			if boot := c.Bootstrap(ctx); boot != nil {
				return retry.Permanent(err)
			}
			res, err = c.searchOnce(ctx, vars)
			if err == nil {
				out = res
				return nil
			}
		}
		return err
	})
	return out, err
}

func (c *Client) searchOnce(ctx context.Context, vars map[string]any) (searchResult, error) {
	body, err := json.Marshal([]map[string]any{{
		"query":         SearchDocument,
		"variables":     vars,
		"operationName": "SearchMyAssets",
	}})
	if err != nil {
		return searchResult{}, retry.Permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+graphQLPath, strings.NewReader(string(body)))
	if err != nil {
		return searchResult{}, retry.Permanent(err)
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Origin", c.base)
	req.Header.Set("Referer", c.base+"/")
	// Without this header the store answers a failed call with a 302 to an HTML error
	// page instead of the JSON error that carries the diagnosis.
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Source", "storefront")
	req.Header.Set("Operations", "SearchMyAssets")
	req.Header.Set("Accept-Encoding", "identity")
	cookie, csrf := c.credentials()
	req.Header.Set("X-Csrf-Token", csrf)
	req.Header.Set("Cookie", cookie)

	resp, err := c.http.Do(req)
	if err != nil {
		return searchResult{}, err
	}
	defer drain(resp)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return searchResult{}, retry.Permanent(ErrExpiredSession)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return searchResult{}, err
	}
	if resp.StatusCode == http.StatusBadRequest && strings.Contains(string(payload), "csrf token mismatch") {
		return searchResult{}, retry.Permanent(ErrCSRF)
	}

	var batch []struct {
		Data struct {
			Search *searchResult `json:"searchMyAssets"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal(payload, &batch); err != nil {
		if retry.Retryable(resp.StatusCode) {
			return searchResult{}, fmt.Errorf("status %d with a non-JSON body", resp.StatusCode)
		}
		return searchResult{}, retry.Permanent(fmt.Errorf("status %d with a non-JSON body: %w", resp.StatusCode, err))
	}
	if len(batch) == 0 {
		return searchResult{}, retry.Permanent(fmt.Errorf("empty GraphQL batch response"))
	}
	op := batch[0]
	if len(op.Errors) > 0 {
		// A missing or invalid credential arrives here as a 500 whose single error has
		// an empty message. Every other error shape is reported as itself, and never as
		// "you own nothing".
		if resp.StatusCode == http.StatusInternalServerError && op.Errors[0].Message == "" {
			return searchResult{}, retry.Permanent(ErrExpiredSession)
		}
		err := fmt.Errorf("store returned %s: %q", op.Errors[0].ErrorCode, op.Errors[0].Message)
		// Only the empty-message shape is the session verdict. A server error that
		// bothered to say what went wrong is still a server error, and the ordinary
		// backoff applies.
		if retry.Retryable(resp.StatusCode) {
			return searchResult{}, err
		}
		return searchResult{}, retry.Permanent(err)
	}
	if op.Data.Search == nil {
		return searchResult{}, retry.Permanent(fmt.Errorf("response carries neither data nor errors"))
	}
	return *op.Data.Search, nil
}

// Download is an open package body plus what the store called it.
type Download struct {
	Body io.ReadCloser

	// Filename is parsed from Content-Disposition, which the store does not always
	// send. It is recorded in the lockfile for reference and never determines a path.
	Filename string
}

// Fetch opens the package stream for one product, applying the response-level guards.
// The caller owns Body and must close it.
func (c *Client) Fetch(ctx context.Context, id string) (*Download, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+downloadPath+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", c.base+"/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	cookie, _ := c.credentials()
	req.Header.Set("Cookie", cookie)
	// Asking for identity is not hygiene: the endpoint honours Accept-Encoding: gzip by
	// gzipping the already-gzipped package, and Go does not transparently decode an
	// encoding the caller requested, so the cache would receive a double-gzipped blob
	// with no readable metadata.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		drain(resp)
		return nil, ErrExpiredSession
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return nil, ErrNotDownloadable
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		err := fmt.Errorf("download %s: status %d", id, resp.StatusCode)
		// A 403 or a 400 will say the same thing on the second attempt, and the download
		// policy's backoff is measured in seconds per asset.
		if !retry.Retryable(resp.StatusCode) {
			return nil, retry.Permanent(err)
		}
		return nil, err
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		drain(resp)
		return nil, fmt.Errorf("download %s: server re-encoded the body as %q despite identity", id, enc)
	}
	if err := checkOctetStream(resp.Header.Get("Content-Type")); err != nil {
		drain(resp)
		return nil, fmt.Errorf("download %s: %w", id, err)
	}
	return &Download{Body: resp.Body, Filename: dispositionFilename(resp.Header.Get("Content-Disposition"))}, nil
}

func checkOctetStream(header string) error {
	if header == "" {
		return fmt.Errorf("response has no Content-Type")
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("unparseable Content-Type %q: %w", header, err)
	}
	if mt != "application/octet-stream" {
		return fmt.Errorf("Content-Type is %q, not a package body", mt)
	}
	return nil
}

func dispositionFilename(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return params["filename"]
}

func withCSRF(header, token string) string {
	var kept []string
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "_csrf=") {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(append(kept, "_csrf="+token), "; ")
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
