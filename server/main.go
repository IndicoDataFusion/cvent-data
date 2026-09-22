package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// buildSHA is the git commit SHA at build time, injected via
// -ldflags "-X main.buildSHA=$(git rev-parse --short=7 HEAD)".
// Empty for ad-hoc local builds.
var buildSHA = ""

const defaultEventID = "TESTCODE01"

type server struct {
	webDir  string // directory containing web assets
	dataDir string // directory for fetched data caches (unused until later tasks)
	eventID string // the one and only event this app serves
	// cvent is the single-event API handler group (Task 6). Nil when Cvent
	// credentials could not be loaded: /api/cvent/… then answers 503 while
	// static serving and /api/health keep working.
	cvent *cventHandlers
}

// sourceHandler serves the /api/<source>/… subtree for one source.
type sourceHandler func(s *server, w http.ResponseWriter, r *http.Request)

// sources is the registry of /api/<source>/… handlers. main() registers the
// "cvent" group (handlers.go) with the client/cache it constructs there.
var sources = map[string]sourceHandler{}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

// handleAPI routes /api/<source>/… to the source registry. /api/health is
// special-cased before the registry.
func (s *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/")
	source := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		source = rest[:i]
	}
	if source == "health" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"source": "cvent",
			"build":  buildSHA,
		})
		return
	}
	h, ok := sources[source]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown source"})
		return
	}
	if source == "cvent" && s.cvent == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cvent credentials not configured"})
		return
	}
	h(s, w, r)
}

// staticCacheControl picks the Cache-Control value for a served asset.
func staticCacheControl(p string) string {
	switch p {
	case "index.html", "sw.js", "manifest.webmanifest":
		// index.html and sw.js must revalidate so shell updates land;
		// the manifest is content-hashed by the publish job.
		if p == "manifest.webmanifest" {
			return "public, max-age=31536000, immutable"
		}
		return "no-cache"
	}
	if strings.HasPrefix(p, "icons/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func contentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".webmanifest":
		return "application/manifest+json"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	}
	return "application/octet-stream"
}

// serveIndex writes web/index.html with no-cache headers.
func (s *server) serveIndex(w http.ResponseWriter) {
	b, err := os.ReadFile(filepath.Join(s.webDir, "index.html"))
	if err != nil {
		log.Printf("static: index.html missing under %s: %v", s.webDir, err)
		http.Error(w, "index.html missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

// handleStatic serves files from the web dir with SPA fallback: a path that
// doesn't map to an existing file falls back to index.html.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		s.serveIndex(w)
		return
	}
	// Resolve within the web dir (no traversal).
	full := filepath.Join(s.webDir, filepath.Clean("/"+p))
	if !strings.HasPrefix(full, filepath.Clean(s.webDir)+string(filepath.Separator)) && full != filepath.Clean(s.webDir) {
		http.NotFound(w, r)
		return
	}
	if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
		w.Header().Set("Content-Type", contentType(p))
		w.Header().Set("Cache-Control", staticCacheControl(p))
		http.ServeFile(w, r, full)
		return
	}
	// SPA fallback for non-existent paths (and directory paths).
	s.serveIndex(w)
}

// statusWriter captures the response status for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// security adds the standard security headers to every response.
func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

// accessLog logs one line per request: method, path, status, duration.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Microsecond))
	})
}

// loadEnv parses KEY=VALUE lines from path (e.g. .env). Lines starting with
// '#' and blank lines are skipped; values may be double-quoted. It never
// overrides variables already set in the environment.
func loadEnv(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("env: %v", err)
		}
		return out
	}
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
		v = strings.Trim(strings.TrimSpace(v), `"`+"'")
		if k == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); exists {
			continue // real environment wins
		}
		out[k] = v
	}
	return out
}

func main() {
	var addr, webDir, dataDir, eventID string
	flag.StringVar(&addr, "addr", ":8766", "listen address")
	flag.StringVar(&webDir, "web", "web", "directory containing web assets")
	flag.StringVar(&dataDir, "data", "data", "directory for fetched data caches")
	flag.StringVar(&eventID, "event", defaultEventID, "Cvent event ID (overridable via CVENT_EVENT in .env)")
	flag.Parse()

	// If --event was left at its default, CVENT_EVENT takes over: first the
	// real environment, then the repo-root .env (run.sh cds to the repo
	// root before exec'ing this binary). An explicitly passed --event
	// always wins.
	if eventID == defaultEventID {
		if v := os.Getenv("CVENT_EVENT"); v != "" {
			eventID = v
		} else if v := loadEnv(".env")["CVENT_EVENT"]; v != "" {
			eventID = v
		}
	}

	s := &server{webDir: webDir, dataDir: dataDir, eventID: eventID}

	// Task 6: construct the Cvent client + event cache. Missing credentials are
	// not fatal: static serving and /api/health keep working; the /api/cvent/…
	// routes answer 503 "cvent credentials not configured" instead.
	if cid, sec, base, err := loadCventEnv(); err != nil {
		log.Printf("cvent: %v — /api/cvent/… will return 503 until credentials are configured", err)
	} else {
		client := newCventClient(base, cid, sec)
		s.cvent = newCventHandlers(eventID, client, newEventCache(client))
		sources["cvent"] = func(srv *server, w http.ResponseWriter, r *http.Request) {
			srv.cvent.route(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", http.HandlerFunc(s.handleAPI))
	mux.Handle("/", http.HandlerFunc(s.handleStatic))

	srv := &http.Server{
		Addr:              addr,
		Handler:           accessLog(security(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Cvent Data PWA listening on %s (web=%s data=%s event=%s build=%s)", addr, webDir, dataDir, s.eventID, buildSHA)
	log.Fatal(srv.ListenAndServe())
}
