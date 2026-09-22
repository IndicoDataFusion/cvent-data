package main

import (
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
