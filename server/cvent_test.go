package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tokenUpstream is a fake Cvent OAuth2 endpoint: it counts requests to
// POST /oauth2/token, records each request's Authorization header, and
// issues the fixed token "tok-1".
type tokenUpstream struct {
	srv   *httptest.Server
	count int64

	mu   sync.Mutex
	auth []string
}

func newTokenUpstream(t *testing.T) *tokenUpstream {
	t.Helper()
	up := &tokenUpstream{}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth2/token" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		atomic.AddInt64(&up.count, 1)
		up.mu.Lock()
		up.auth = append(up.auth, r.Header.Get("Authorization"))
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"tok-1","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(up.srv.Close)
	return up
}

func (u *tokenUpstream) requests() int64 { return atomic.LoadInt64(&u.count) }

func (u *tokenUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auth) == 0 {
		return ""
	}
	return u.auth[len(u.auth)-1]
}

func TestTokenCache(t *testing.T) {
	up := newTokenUpstream(t)
	ctx := context.Background()

	// Within the TTL, a second call is served from the cache.
	c := newCventClient(up.srv.URL, "cid", "secret")
	c.ttl = time.Hour
	tok, err := c.token(ctx)
	if err != nil {
		t.Fatalf("first token(): %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("first token() = %q, want %q", tok, "tok-1")
	}
	tok, err = c.token(ctx)
	if err != nil {
		t.Fatalf("second token(): %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("second token() = %q, want %q", tok, "tok-1")
	}
	if got := up.requests(); got != 1 {
		t.Fatalf("upstream token requests = %d, want 1 (second call must be a cache hit)", got)
	}

	// Short TTL + injectable clock: expiry is deterministic, no sleep.
	c2 := newCventClient(up.srv.URL, "cid", "secret")
	c2.ttl = time.Millisecond
	now := time.Now()
	c2.now = func() time.Time { return now }
	if _, err := c2.token(ctx); err != nil {
		t.Fatalf("cold token(): %v", err)
	}
	if got := up.requests(); got != 2 {
		t.Fatalf("upstream token requests = %d, want 2", got)
	}
	now = now.Add(2 * time.Millisecond) // past the 1ms TTL
	if _, err := c2.token(ctx); err != nil {
		t.Fatalf("expired token(): %v", err)
	}
	if got := up.requests(); got != 3 {
		t.Fatalf("upstream token requests = %d, want 3 (expired token must be re-fetched)", got)
	}
}

func TestTokenBasicAuth(t *testing.T) {
	up := newTokenUpstream(t)
	c := newCventClient(up.srv.URL, "cid", "secret")
	if _, err := c.token(context.Background()); err != nil {
		t.Fatalf("token(): %v", err)
	}
	got := up.lastAuth()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:secret"))
	if got != want {
		t.Fatalf("Authorization header = %q, want %q", got, want)
	}
	// Pin the Cvent gotcha: the base64 payload must decode back to exactly
	// "cid:secret" — no trailing newline.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "Basic "))
	if err != nil {
		t.Fatalf("decoding Authorization payload: %v", err)
	}
	if string(raw) != "cid:secret" {
		t.Fatalf("decoded Authorization payload = %q, want exactly %q", string(raw), "cid:secret")
	}
}

// TestTokenConcurrent: N goroutines racing on a cold cache must produce
// exactly one upstream request.
func TestTokenConcurrent(t *testing.T) {
	up := newTokenUpstream(t)
	c := newCventClient(up.srv.URL, "cid", "secret")
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = c.token(context.Background())
		}(i)
	}
	close(start) // release all goroutines onto the cold cache
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("token() goroutine %d: %v", i, err)
		}
	}
	if got := up.requests(); got != 1 {
		t.Fatalf("upstream token requests under %d concurrent callers = %d, want 1", n, got)
	}
}

func TestLoadCventEnv(t *testing.T) {
	const secret = "file-secret-do-not-leak"
	root := t.TempDir()
	sub := filepath.Join(root, "server")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, ".env")
	content := "# cvent credentials\n\nCVENT_CLIENT_ID=filecid\nCVENT_CLIENT_SECRET=" + secret + "\nCVENT_API_BASE=https://example.test/ea\n"
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pin the ambient environment (the dev shell may export real
	// CVENT_* values): empty means "unset" for the client.
	t.Setenv("CVENT_CLIENT_ID", "")
	t.Setenv("CVENT_CLIENT_SECRET", "")
	t.Setenv("CVENT_API_BASE", "")

	// Walking up from a subdirectory finds the root .env.
	cid, sec, base, err := loadCventEnvFrom(sub)
	if err != nil {
		t.Fatalf("loadCventEnvFrom: %v", err)
	}
	if cid != "filecid" || sec != secret || base != "https://example.test/ea" {
		t.Fatalf("got (%q, %q, %q), want (filecid, %q, https://example.test/ea)", cid, sec, base, secret)
	}

	// Real environment variables take precedence over .env values.
	t.Setenv("CVENT_CLIENT_ID", "envcid")
	t.Setenv("CVENT_API_BASE", "https://env.test/ea")
	cid, sec, base, err = loadCventEnvFrom(sub)
	if err != nil {
		t.Fatalf("loadCventEnvFrom (env override): %v", err)
	}
	if cid != "envcid" || sec != secret || base != "https://env.test/ea" {
		t.Fatalf("got (%q, %q, %q), want (envcid, %q, https://env.test/ea)", cid, sec, base, secret)
	}

	// A missing client id must error, naming the variable and never
	// leaking the secret value.
	empty := t.TempDir()
	onlySecret := filepath.Join(empty, ".env")
	if err := os.WriteFile(onlySecret, []byte("CVENT_CLIENT_SECRET="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CVENT_CLIENT_ID", "") // simulate unset
	_, _, _, err = loadCventEnvFrom(empty)
	if err == nil {
		t.Fatal("loadCventEnvFrom with missing CVENT_CLIENT_ID: want error, got nil")
	}
	if !strings.Contains(err.Error(), "CVENT_CLIENT_ID") {
		t.Fatalf("error should name the missing variable: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks the secret value: %v", err)
	}
}
