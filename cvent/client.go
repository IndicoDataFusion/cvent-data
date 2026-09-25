// Package cvent is a small, dependency-free Go client for the Cvent
// Events API: OAuth2 client_credentials auth with a cached token, cursor
// pagination for the list/filter endpoints, and the per-event "bundle"
// fetch + cache the PWA server and the cvent-dump CLI are built on.
//
// Credentials are resolved from the environment or a .env file (see
// FromEnvironment) and are never written into errors or logs: errors name
// the variable, not its value.
package cvent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the Cvent Events API base when CVENT_API_BASE is unset.
const DefaultBaseURL = "https://api-platform.cvent.com/ea"

// tokenTTL is the effective life of a cached token. Tokens are 60-min JWTs;
// we expire ours at 55 min so a token is never presented at the very edge of
// its lifetime.
const tokenTTL = 55 * time.Minute

// pageSize is the maximum page size in the Cvent OpenAPI spec. The /events
// Active list fits in one request; bigger resources page through.
const pageSize = 200

// Client is a Cvent API client. It owns the OAuth token cache (mutex-
// guarded, single-flight on a cold cache) and the HTTP client used for all
// data requests. Construct with New / NewWithCredentials (or FromEnvironment
// via the server / CLI).
type Client struct {
	base string // API base (see DefaultBaseURL)
	cid  string
	sec  string
	ttl  time.Duration // effective token lifetime (tests may shrink it)
	now  func() time.Time
	mu   sync.Mutex
	tok  string
	exp  time.Time
	http *http.Client // Timeout 20s
}

// newCventClient is the internal constructor: it takes the base URL as-is
// (no defaulting) so tests can point it at an httptest upstream.
func newCventClient(base, cid, sec string) *Client {
	return &Client{
		base: base,
		cid:  cid,
		sec:  sec,
		ttl:  tokenTTL,
		now:  time.Now,
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient replaces the client's HTTP client (timeout, transport,
// proxies).
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.http = c }
}

// WithTokenTTL overrides the effective token lifetime (tests shrink it).
func WithTokenTTL(d time.Duration) Option {
	return func(cl *Client) { cl.ttl = d }
}

// New builds a Client for the given API base and client credentials. An
// empty base falls back to DefaultBaseURL.
func New(base, clientID, clientSecret string, opts ...Option) *Client {
	cl := newCventClient(base, clientID, clientSecret)
	if cl.base == "" {
		cl.base = DefaultBaseURL
	}
	for _, o := range opts {
		o(cl)
	}
	return cl
}

// NewWithCredentials builds a Client from resolved Credentials.
func NewWithCredentials(c Credentials, opts ...Option) *Client {
	return New(c.BaseURL, c.ClientID, c.ClientSecret, opts...)
}

// token returns a valid Cvent access token, fetching one from the OAuth2
// endpoint if the cached token is missing or past its TTL. The mutex is held
// across the fetch so concurrent callers on a cold cache produce exactly one
// upstream request. The token is held in memory only: it is never persisted
// or logged.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.tok != "" && now.Before(c.exp) {
		return c.tok, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.cid)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.base, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("cvent oauth: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Cvent gotcha: the Basic payload is base64(cid:sec) with NO trailing
	// newline. Go's base64.StdEncoding never adds one, but this is the exact
	// shape that works against the real endpoint.
	req.Header.Set("Authorization",
		"Basic "+base64.StdEncoding.EncodeToString([]byte(c.cid+":"+c.sec)))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cvent oauth: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// Names the variables, never the values.
		return "", fmt.Errorf("cvent oauth: 401 — invalid client credentials (check CVENT_CLIENT_ID / CVENT_CLIENT_SECRET)")
	case http.StatusForbidden:
		return "", fmt.Errorf("cvent oauth: 403 — client lacks the required scope")
	case http.StatusOK:
		// fall through
	default:
		return "", fmt.Errorf("cvent oauth: unexpected status %d from %s", resp.StatusCode, req.URL)
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("cvent oauth: decoding token response: %v", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("cvent oauth: token response has no access_token")
	}
	c.tok = out.AccessToken
	c.exp = now.Add(c.ttl)
	return c.tok, nil
}

// Checkin POSTs the spec's bulk-checkin array to /events/{uuid}/check-in and
// returns the upstream status code and raw body. The caller interprets the
// status (the spec answers 207 on success).
func (c *Client) Checkin(ctx context.Context, uuid string, payload []map[string]string) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.base, "/")+"/events/"+uuid+"/check-in", strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	tok, err := c.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if rerr != nil {
		return 0, nil, fmt.Errorf("cvent check-in: reading body: %v", rerr)
	}
	return resp.StatusCode, raw, nil
}

// cventPage is one decoded page of a Cvent list/filter response. Cvent
// returns the items under "items" (most endpoints) or "data" (some); paging
// carries {limit, totalCount, currentToken, _links}.
type cventPage struct {
	Items      []json.RawMessage
	TotalCount int
	HasTotal   bool
	Token      string // "" when paging.currentToken is absent
}

// decodeCventPage parses a page body, accepting the items array under either
// "items" or "data".
func decodeCventPage(body []byte) (*cventPage, error) {
	var envelope struct {
		Items  *json.RawMessage `json:"items"`
		Data   *json.RawMessage `json:"data"`
		Paging struct {
			TotalCount   int    `json:"totalCount"`
			CurrentToken string `json:"currentToken"`
		} `json:"paging"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding page: %v", err)
	}
	var items []json.RawMessage
	switch {
	case envelope.Items != nil:
		if err := json.Unmarshal(*envelope.Items, &items); err != nil {
			return nil, fmt.Errorf("decoding items: %v", err)
		}
	case envelope.Data != nil:
		if err := json.Unmarshal(*envelope.Data, &items); err != nil {
			return nil, fmt.Errorf("decoding data: %v", err)
		}
	default:
		items = nil
	}
	return &cventPage{
		Items:      items,
		TotalCount: envelope.Paging.TotalCount,
		HasTotal:   envelope.Paging.TotalCount > 0,
		Token:      envelope.Paging.CurrentToken,
	}, nil
}

// firstItemKey extracts a cheap identity for a page's first item: the "id"
// field when present, else the item's raw bytes. Used for non-advancing-token
// detection.
func firstItemKey(items []json.RawMessage) (string, bool) {
	if len(items) == 0 {
		return "", false
	}
	var it struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(items[0], &it); err == nil && it.ID != "" {
		return it.ID, true
	}
	return string(items[0]), true
}

// listAll walks a GET list endpoint with cursor pagination. extra carries
// endpoint-specific query params (e.g. the filter for /attendees/activities).
// Stops when collected >= paging.totalCount, on an empty page, or on a
// missing token. Page size is the spec maximum (200).
func (c *Client) listAll(ctx context.Context, path string, extra url.Values) ([]json.RawMessage, error) {
	q := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return c.walkPages(ctx, http.MethodGet, path, q, nil)
}

// filterAll walks a POST …/filter endpoint (JSON body {"filter": expr},
// limit/token as query params): attendees, admission-items, sessions, speakers.
func (c *Client) filterAll(ctx context.Context, path, expr string) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"filter": expr})
	if err != nil {
		return nil, fmt.Errorf("cvent %s: encoding filter body: %v", path, err)
	}
	return c.walkPages(ctx, http.MethodPost, path, nil, body)
}

// walkPages drives the shared cursor-pagination loop for both GET list and
// POST filter endpoints. body (non-nil) is the POST body, identical on every
// page. limit is always sent; token is sent from page 2 on.
func (c *Client) walkPages(ctx context.Context, method, path string, extra url.Values, body []byte) ([]json.RawMessage, error) {
	var all []json.RawMessage
	var prevFirst string // first item id of the previous page
	havePrev := false
	token := ""
	for {
		q := url.Values{}
		for k, vs := range extra {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("limit", fmt.Sprint(pageSize))
		if token != "" {
			q.Set("token", token)
		}

		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method,
			strings.TrimRight(c.base, "/")+path+"?"+q.Encode(), rd)
		if err != nil {
			return nil, fmt.Errorf("cvent %s: %v", path, err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cvent %s: %v", path, err)
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("cvent %s: reading body: %v", path, rerr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cvent %s: HTTP %d", path, resp.StatusCode)
		}

		page, err := decodeCventPage(raw)
		if err != nil {
			return nil, fmt.Errorf("cvent %s: %v", path, err)
		}

		// Non-advancing-token detection (the /events quirk, measured live
		// 2026-09-22: the token param is silently ignored and the same page
		// repeats forever). If we sent a token and got the same first item
		// back, error out — never return silently-truncated data, never loop.
		if token != "" && havePrev {
			if first, ok := firstItemKey(page.Items); ok && first == prevFirst {
				total := "unknown"
				if page.HasTotal {
					total = fmt.Sprint(page.TotalCount)
				}
				return nil, fmt.Errorf("cvent %s: pagination token did not advance; collected %d of %s", path, len(all), total)
			}
		}

		all = append(all, page.Items...)
		if first, ok := firstItemKey(page.Items); ok {
			prevFirst = first
			havePrev = true
		}

		// Stop conditions, in order:
		if page.HasTotal && len(all) >= page.TotalCount {
			break // Cvent mints tokens even on final pages — never trust absence.
		}
		if len(page.Items) == 0 {
			break
		}
		if page.Token == "" {
			break
		}
		token = page.Token
	}
	return all, nil
}

// getOnePage fetches a single page (no pagination walk) and returns the event
// it contains: a list page ({"items":[...]} or {"data":[...]}) yields the
// first item; a single object with an "id" field (GET /events/{uuid}) is
// returned as is. Zero matching items → "event not found". Used for
// code→uuid resolution, where a walk is unsafe (the /events token quirk
// silently ignores the token param and repeats the page).
func (c *Client) getOnePage(ctx context.Context, path string, extra url.Values) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(pageSize))
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("cvent %s: %v", path, err)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cvent %s: %v", path, err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("cvent %s: reading body: %v", path, rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cvent %s: HTTP %d", path, resp.StatusCode)
	}

	var probe struct {
		Items *json.RawMessage `json:"items"`
		Data  *json.RawMessage `json:"data"`
		ID    *string          `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("cvent %s: decoding response: %v", path, err)
	}
	var list *json.RawMessage
	switch {
	case probe.Items != nil:
		list = probe.Items
	case probe.Data != nil:
		list = probe.Data
	}
	if list != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(*list, &items); err != nil {
			return nil, fmt.Errorf("cvent %s: decoding items: %v", path, err)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("cvent %s: event not found", path)
		}
		return items[0], nil
	}
	if probe.ID != nil {
		return raw, nil // single-object shape (GET /events/{uuid})
	}
	return nil, fmt.Errorf("cvent %s: unrecognized response shape (no items, no id)", path)
}
