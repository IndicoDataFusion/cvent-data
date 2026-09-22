package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// listUpstream is a fake Cvent list endpoint: it serves pre-built page
// bodies keyed by the ?token= query param ("" = first page), counts total
// data requests, and records the method, Content-Type, query string, and
// request body of each request.
type listUpstream struct {
	srv   *httptest.Server
	path  string
	pages map[string][]byte // token ("" = first page) → response body
	count int64

	mu     sync.Mutex
	reqLog []loggedReq
}

type loggedReq struct {
	method      string
	contentType string
	query       string
	body        string
}

func newListUpstream(t *testing.T, path string, pages map[string][]byte) *listUpstream {
	t.Helper()
	up := &listUpstream{path: path, pages: pages}
	up.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != up.path {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		atomic.AddInt64(&up.count, 1)
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.reqLog = append(up.reqLog, loggedReq{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			query:       r.URL.RawQuery,
			body:        string(body),
		})
		up.mu.Unlock()
		tok := r.URL.Query().Get("token")
		page, ok := up.pages[tok]
		if !ok {
			// A request beyond the scripted pages means the client looped
			// (e.g. followed a final-page continuation token).
			http.Error(w, "unexpected token "+tok, http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(page)
	}))
	t.Cleanup(up.srv.Close)
	return up
}

func (u *listUpstream) requests() int64 { return atomic.LoadInt64(&u.count) }

func (u *listUpstream) reqs() []loggedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]loggedReq, len(u.reqLog))
	copy(out, u.reqLog)
	return out
}

func listItem(id string) string {
	return fmt.Sprintf(`{"id":%q,"name":"item-%s"}`, id, id)
}

func listPage(ids []string, total int, token string) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(listItem(id))
	}
	b.WriteString(`],"paging":{"limit":200,"totalCount":`)
	b.WriteString(fmt.Sprint(total))
	b.WriteString(`,"currentToken":`)
	b.WriteString(fmt.Sprintf("%q", token))
	b.WriteString(`}}`)
	return []byte(b.String())
}

// warmToken seeds the client's token cache so data-request counts in these
// tests are exact (no OAuth round-trips); token() itself is covered in Task 2.
func warmToken(c *cventClient) {
	c.tok = "tok-1"
	c.exp = time.Now().Add(time.Hour)
}

// TestPaginationWalk: a well-behaved paginated upstream. Cvent quirk pinned
// here: the final page still mints a continuation token ("t3") — the walk MUST
// stop on collected >= totalCount, not on token absence, and must NOT make a
// 4th request.
func TestPaginationWalk(t *testing.T) {
	up := newListUpstream(t, "/events", map[string][]byte{
		"":   listPage([]string{"a", "b"}, 5, "t1"),
		"t1": listPage([]string{"c", "d"}, 5, "t2"),
		"t2": listPage([]string{"e"}, 5, "t3"), // final page still carries a token
	})
	c := newCventClient(up.srv.URL, "cid", "secret")
	warmToken(c)
	items, err := c.listAll(context.Background(), "/events", url.Values{})
	if err != nil {
		t.Fatalf("listAll: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("listAll returned %d items, want 5", len(items))
	}
	var ids []string
	for _, raw := range items {
		var it struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &it); err != nil {
			t.Fatalf("unmarshalling item %s: %v", raw, err)
		}
		ids = append(ids, it.ID)
	}
	if want := []string{"a", "b", "c", "d", "e"}; !stringSlicesEqual(ids, want) {
		t.Fatalf("item ids = %v, want %v (order must be preserved)", ids, want)
	}
	if got := up.requests(); got != 3 {
		t.Fatalf("upstream requests = %d, want exactly 3 (a 4th request with token t3 would loop/return duplicates)", got)
	}
}

// TestPaginationTokenIgnored: the REAL /events behavior, measured live
// 2026-09-22 — the token param is silently ignored, so page 2 with token t1
// returns the SAME 3 items (identical ids) again. The client must detect the
// non-advancing token and error with the collected/total counts — never
// return silently-truncated data, never loop.
func TestPaginationTokenIgnored(t *testing.T) {
	up := newListUpstream(t, "/events", map[string][]byte{
		"":   listPage([]string{"e1", "e2", "e3"}, 9, "t1"),
		"t1": listPage([]string{"e1", "e2", "e3"}, 9, "t2"), // identical items; token ignored upstream
	})
	c := newCventClient(up.srv.URL, "cid", "secret")
	warmToken(c)
	_, err := c.listAll(context.Background(), "/events", url.Values{})
	if err == nil {
		t.Fatal("listAll: want error for non-advancing token, got nil")
	}
	if !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("error should say the token did not advance: %v", err)
	}
	if !strings.Contains(err.Error(), "3 of 9") {
		t.Fatalf("error should name collected/total counts (3 of 9): %v", err)
	}
	if got := up.requests(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2 (must stop at detection, never loop)", got)
	}
}

// TestFilterPost: POST …/filter with the JSON OData-ish filter body; limit
// (the client's page size) travels as a query param on page 1 and the token
// param on page 2.
func TestFilterPost(t *testing.T) {
	const expr = `event.id eq '123e4567-e89b-12d3-a456-426614174000'`
	const wantBody = `{"filter":"event.id eq '123e4567-e89b-12d3-a456-426614174000'"}`

	// (a)–(c) on a single-page result: the walk must stop after 1 request.
	up := newListUpstream(t, "/attendees/filter", map[string][]byte{
		"": listPage([]string{"p1"}, 1, "t1"), // totalCount == items returned
	})
	c := newCventClient(up.srv.URL, "cid", "secret")
	warmToken(c)
	items, err := c.filterAll(context.Background(), "/attendees/filter", expr)
	if err != nil {
		t.Fatalf("filterAll: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("filterAll returned %d items, want 1", len(items))
	}
	if got := up.requests(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 (single-page result must stop the walk)", got)
	}
	reqs := up.reqs()
	if reqs[0].method != http.MethodPost {
		t.Fatalf("request method = %q, want POST", reqs[0].method)
	}
	if reqs[0].contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", reqs[0].contentType)
	}
	if reqs[0].body != wantBody {
		t.Fatalf("request body = %q, want exactly %q", reqs[0].body, wantBody)
	}
	q1, err := url.ParseQuery(reqs[0].query)
	if err != nil {
		t.Fatalf("parsing page-1 query %q: %v", reqs[0].query, err)
	}
	if got := q1.Get("limit"); got == "" {
		t.Fatalf("page-1 query must carry the client's page size limit, got %q", reqs[0].query)
	}
	if got := q1.Get("token"); got != "" {
		t.Fatalf("page-1 query must carry no token, got token=%q", got)
	}

	// (c) page 2: the continuation token travels as ?token= on POST too.
	up2 := newListUpstream(t, "/attendees/filter", map[string][]byte{
		"":   listPage([]string{"p1", "p2"}, 3, "t1"),
		"t1": listPage([]string{"p3"}, 3, "t2"),
	})
	c2 := newCventClient(up2.srv.URL, "cid", "secret")
	warmToken(c2)
	if _, err := c2.filterAll(context.Background(), "/attendees/filter", expr); err != nil {
		t.Fatalf("filterAll (two pages): %v", err)
	}
	if got := up2.requests(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2", got)
	}
	reqs2 := up2.reqs()
	q2, err := url.ParseQuery(reqs2[1].query)
	if err != nil {
		t.Fatalf("parsing page-2 query %q: %v", reqs2[1].query, err)
	}
	if got := q2.Get("token"); got != "t1" {
		t.Fatalf("page-2 query must carry token=t1, got %q", reqs2[1].query)
	}
	if got := q2.Get("limit"); got == "" {
		t.Fatalf("page-2 query must also carry limit, got %q", reqs2[1].query)
	}
	if reqs2[1].method != http.MethodPost {
		t.Fatalf("page-2 method = %q, want POST", reqs2[1].method)
	}
	if reqs2[1].body != wantBody {
		t.Fatalf("page-2 body = %q, want exactly %q", reqs2[1].body, wantBody)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
