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
