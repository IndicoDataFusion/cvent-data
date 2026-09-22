package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newStaticTestServer wires a test server with the same mux layout main()
// uses, pointed at temp web/data dirs.
func newStaticTestServer(t *testing.T, webDir, dataDir string) (*server, *httptest.Server) {
	t.Helper()
	s := &server{webDir: webDir, dataDir: dataDir}
	mux := http.NewServeMux()
	mux.Handle("/api/", http.HandlerFunc(s.handleAPI))
	mux.Handle("/data/", http.HandlerFunc(s.handleData))
	mux.Handle("/", http.HandlerFunc(s.handleStatic))
	return s, httptest.NewServer(security(mux))
}

func TestCventMissingCreds(t *testing.T) {
	// Deterministic "no credentials" setup:
	//  1. t.Chdir into a temp dir so loadCventEnv's up-walk from cwd finds no
	//     .env (the repo-root .env is left behind; no ancestor of the temp dir
	//     defines CVENT_CLIENT_ID).
	//  2. t.Setenv the CVENT_* vars to "" so a dev shell exporting real creds
	//     cannot leak in (envOr treats empty as unset).
	t.Chdir(t.TempDir())
	t.Setenv("CVENT_CLIENT_ID", "")
	t.Setenv("CVENT_CLIENT_SECRET", "")
	t.Setenv("CVENT_API_BASE", "")

	webDir := t.TempDir()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"),
		[]byte("<html>INDEX</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{webDir: webDir, dataDir: dataDir, eventID: defaultEventID}
	srv := httptest.NewServer(buildRouter(s))
	defer srv.Close()

	get := func(path string) (int, string) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(res.Body); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return res.StatusCode, buf.String()
	}

	// Every /api/cvent/… route → 503 "cvent credentials not configured",
	// including an unknown cvent sub-path (the source exists, just has no
	// client). Spec (Task 6): missing creds must NOT 404 "unknown source".
	for _, path := range []string{
		"/api/cvent/event",
		"/api/cvent/event/payments",
		"/api/cvent/event/attendees",
		"/api/cvent/event/checkin",
		"/api/cvent/event/repull",
		"/api/cvent/event/repull-status",
		"/api/cvent/bogus",
	} {
		code, body := get(path)
		if code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: status = %d, want 503 (body %q)", path, code, body)
		}
		if !strings.Contains(body, "cvent credentials not configured") {
			t.Errorf("GET %s: body = %q, want it to contain %q",
				path, body, "cvent credentials not configured")
		}
	}

	// /api/health keeps working.
	code, body := get("/api/health")
	if code != http.StatusOK {
		t.Errorf("GET /api/health: status = %d, want 200 (body %q)", code, body)
	}

	// An unknown source is STILL 404 "unknown source" — the fix must not turn
	// every missing source into 503.
	code, body = get("/api/nope/x")
	if code != http.StatusNotFound {
		t.Errorf("GET /api/nope/x: status = %d, want 404 (body %q)", code, body)
	}
	if !strings.Contains(body, "unknown source") {
		t.Errorf("GET /api/nope/x: body = %q, want it to contain %q",
			body, "unknown source")
	}

	// Static serving keeps working: / → 200 HTML.
	code, body = get("/")
	if code != http.StatusOK {
		t.Errorf("GET /: status = %d, want 200 (body %q)", code, body)
	}
	if !strings.Contains(body, "INDEX") {
		t.Errorf("GET /: body = %q, want it to contain INDEX", body)
	}
}

func TestStaticDataServing(t *testing.T) {
	webDir := t.TempDir()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>INDEX</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexBody := `{"events":[{"code":"TESTCODE01"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "index.json"), []byte(indexBody), 0o644); err != nil {
		t.Fatal(err)
	}
	metaBody := `{"counts":{"attendees":0}}`
	if err := os.MkdirAll(filepath.Join(dataDir, "TESTCODE01"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "TESTCODE01", "meta.json"), []byte(metaBody), 0o644); err != nil {
		t.Fatal(err)
	}

	s, srv := newStaticTestServer(t, webDir, dataDir)
	defer srv.Close()

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(res.Body); err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return res, buf.Bytes()
	}

	// /data/index.json: exact bytes, JSON content-type, no-cache, no immutable.
	res, body := get("/data/index.json")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /data/index.json: status = %d, want 200", res.StatusCode)
	}
	if !bytes.Equal(body, []byte(indexBody)) {
		t.Fatalf("GET /data/index.json: body = %q, want exact dump bytes", body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET /data/index.json: Content-Type = %q, want application/json", ct)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("GET /data/index.json: Cache-Control = %q, want no-cache", cc)
	}

	// Nested dump file serves too.
	res, body = get("/data/TESTCODE01/meta.json")
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, []byte(metaBody)) {
		t.Fatalf("GET /data/TESTCODE01/meta.json: status=%d body=%q", res.StatusCode, body)
	}

	// Missing data file and the dir root: real 404 JSON, never the SPA index.
	for _, path := range []string{"/data/nope.json", "/data/"} {
		res, body = get(path)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status = %d, want 404", path, res.StatusCode)
		}
		var errObj map[string]any
		if err := json.Unmarshal(body, &errObj); err != nil {
			t.Fatalf("GET %s: body %q is not a JSON error object", path, body)
		}
		if strings.Contains(strings.ToLower(string(body)), "<html") {
			t.Fatalf("GET %s: fell through to SPA HTML: %q", path, body)
		}
	}

	// Traversal must not escape the data dir. net/http 301-redirects
	// uncleaned paths before routing, so exercise the handler directly with
	// the raw path (what curl --path-as-is / a hostile client sends).
	req := httptest.NewRequest(http.MethodGet, "/data/../server/main.go", nil)
	rec := httptest.NewRecorder()
	s.handleData(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /data/../server/main.go: status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "package main") {
		t.Fatalf("traversal escaped the data dir: %q", rec.Body.String())
	}

	// SPA fallback for non-file paths is untouched.
	res, body = get("/somepage")
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "INDEX") {
		t.Fatalf("GET /somepage: status=%d body=%q, want SPA index", res.StatusCode, body)
	}
	res, body = get("/")
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "INDEX") {
		t.Fatalf("GET /: status=%d body=%q, want SPA index", res.StatusCode, body)
	}
}
