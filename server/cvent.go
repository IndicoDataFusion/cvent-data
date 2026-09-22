package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultCventBase = "https://api-platform.cvent.com/ea"

// tokenTTL is the effective life of a cached Cvent token. Tokens are 60-min
// JWTs; we expire ours at 55 min so a token is never presented at the very
// edge of its lifetime.
const tokenTTL = 55 * time.Minute

type cventClient struct {
	base string   // default https://api-platform.cvent.com/ea, overridable via CVENT_API_BASE env
	cid  string
	sec  string
	ttl  time.Duration // effective token lifetime (tests may shrink it)
	now  func() time.Time
	mu   sync.Mutex
	tok  string
	exp  time.Time
	http *http.Client // Timeout 20s
}

func newCventClient(base, cid, sec string) *cventClient {
	return &cventClient{
		base: base,
		cid:  cid,
		sec:  sec,
		ttl:  tokenTTL,
		now:  time.Now,
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// token returns a valid Cvent access token, fetching one from the OAuth2
// endpoint if the cached token is missing or past its TTL. The mutex is held
// across the fetch so concurrent callers on a cold cache produce exactly one
// upstream request.
func (c *cventClient) token(ctx context.Context) (string, error) {
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

// loadCventEnv resolves the Cvent credentials: real environment variables
// win, then the .env at the repo root (walked up from the working directory).
// base defaults to defaultCventBase when unset. Errors never contain secret
// values.
func loadCventEnv() (cid, sec, base string, err error) {
	return loadCventEnvFrom("")
}

// loadCventEnvFrom is loadCventEnv with the starting directory made explicit
// ("" = os.Getwd()), so tests can point it at a temp tree.
func loadCventEnvFrom(start string) (cid, sec, base string, err error) {
	dir := start
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", "", "", fmt.Errorf("cvent env: %v", err)
		}
	}
	fileVals := map[string]string{}
	// Walk up until a .env that actually defines CVENT_CLIENT_ID.
	for {
		b, rerr := os.ReadFile(filepath.Join(dir, ".env"))
		if rerr == nil {
			fileVals = parseDotEnv(b)
			if _, ok := fileVals["CVENT_CLIENT_ID"]; ok {
				break
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached filesystem root
			break
		}
		dir = parent
	}

	cid = envOr("CVENT_CLIENT_ID", fileVals)
	sec = envOr("CVENT_CLIENT_SECRET", fileVals)
	base = envOr("CVENT_API_BASE", fileVals)
	if base == "" {
		base = defaultCventBase
	}
	if cid == "" {
		return "", "", "", fmt.Errorf("cvent env: CVENT_CLIENT_ID is not set (set it in the environment or in a .env at the repo root)")
	}
	if sec == "" {
		return "", "", "", fmt.Errorf("cvent env: CVENT_CLIENT_SECRET is not set (set it in the environment or in a .env at the repo root)")
	}
	return cid, sec, base, nil
}

// envOr returns the real environment value when set and non-empty, else the
// .env-file value.
func envOr(key string, file map[string]string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return file[key]
}

// parseDotEnv parses KEY=VALUE lines: blank lines and # comments are skipped,
// surrounding single/double quotes are stripped from values.
func parseDotEnv(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// pageSize is the maximum page size in the Cvent OpenAPI spec. The /events
// Active list (113) fits in one request; bigger resources page through.
const pageSize = 200

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
func (c *cventClient) listAll(ctx context.Context, path string, extra url.Values) ([]json.RawMessage, error) {
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
func (c *cventClient) filterAll(ctx context.Context, path, expr string) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"filter": expr})
	if err != nil {
		return nil, fmt.Errorf("cvent %s: encoding filter body: %v", path, err)
	}
	return c.walkPages(ctx, http.MethodPost, path, nil, body)
}

// walkPages drives the shared cursor-pagination loop for both GET list and
// POST filter endpoints. body (non-nil) is the POST body, identical on every
// page. limit is always sent; token is sent from page 2 on.
func (c *cventClient) walkPages(ctx context.Context, method, path string, extra url.Values, body []byte) ([]json.RawMessage, error) {
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
