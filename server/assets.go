package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Content-hashed shell assets.
//
// Every .js/.css file under the web dir (except sw.js — its URL is fixed by
// navigator.serviceWorker.register) is served under an additional content
// hash in the file name: styles.css -> styles.<sha1>.css,
// sources/cvent.js -> sources/cvent.<sha1>.js (same directory, hash before
// the extension). index.html (and the import specifiers INSIDE every JS
// module) are rewritten to point at the hashed names, so the ENTIRE module
// graph is hash-addressed — not just the entry points in index.html. A
// content change is a NEW URL, so no client (plain-HTTP or
// service-worker-cached) can serve a stale shell. Hashed paths get
// Cache-Control: immutable; index.html stays no-cache. /assets.json
// publishes the hashed list for the service worker's precache.
//
// On-disk files are never renamed: the server serves pre-rewritten bytes
// under both the original and the hashed path (identical bytes either way).
//
// importSpecRe pulls module specifiers out of a JS file: static
// "import ... from \"x\"", side-effect "import \"x\"" and dynamic
// "import(\"x\")" forms.
var importSpecRe = regexp.MustCompile(`(?:from|import)\s*\(?\s*["']([^"']+)["']`)

type assetTable struct {
	byOriginal map[string]string // "styles.css" -> "styles.<sha1>.css"
	byHashed   map[string]string // "styles.<sha1>.css" -> "styles.css"
	served     map[string][]byte // original path -> the bytes the server must
	// serve (import specifiers rewritten to the
	// hashed names); the on-disk file is untouched
	static []string // all other files, sorted ("icons/icon-192.png", "sw.js", ...)
}

// loadAssets walks webDir and builds the hash table. A missing webDir
// yields a nil table (the server then serves everything unhashed — the
// pre-hashing behavior), which the tests that use a minimal web dir rely on.
//
// Two passes: the first builds byOriginal/byHashed for every asset, the
// second computes each JS module's rewritten bytes. The split matters —
// rewrite() looks up import targets in byOriginal, and WalkDir visits root
// files (app.js) BEFORE subdirectories (sources/), so a single pass would
// leave app.js's import of sources/registry.js unrewritten (the target's
// hash isn't known yet when app.js is processed).
func loadAssets(webDir string) (*assetTable, error) {
	t := &assetTable{
		byOriginal: map[string]string{},
		byHashed:   map[string]string{},
		served:     map[string][]byte{},
	}
	type jsAsset struct {
		rel, dir string
		body     []byte
	}
	var jsFiles []jsAsset
	err := filepath.WalkDir(webDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(webDir, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(rel))
		if ext == ".js" && rel != "sw.js" || ext == ".css" {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			sum := sha1.Sum(b)
			hexSum := hex.EncodeToString(sum[:])
			dir := filepath.ToSlash(filepath.Dir(rel))
			base := filepath.Base(rel)
			if dir == "." {
				dir = ""
			}
			hashedBase := strings.TrimSuffix(base, ext) + "." + hexSum + ext
			hashed := hashedBase
			if dir != "" {
				hashed = dir + "/" + hashedBase
			}
			t.byOriginal[rel] = hashed
			t.byHashed[hashed] = rel
			if ext == ".js" {
				jsFiles = append(jsFiles, jsAsset{rel: rel, dir: dir, body: b})
			}
			return nil
		}
		t.static = append(t.static, rel)
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loadAssets: %v", err)
	}
	// Pass 2: byOriginal is complete, so every import target resolves.
	for _, j := range jsFiles {
		t.served[j.rel] = []byte(t.rewrite(j.dir, string(j.body)))
	}
	sort.Strings(t.static)
	return t, nil
}

// rewrite returns content with every asset reference swapped for its
// hashed form. Two reference shapes occur:
//   - JS module specifiers (any .js file): import "./cvent.js",
//     import * as x from "./cvent.js", import("/x.js") — resolved relative
//     to the IMPORTER's directory; the hash keeps the original's directory,
//     so the specifier's prefix is preserved and only the base name changes
//   - HTML attribute values (index.html): src="app.js", href="styles.css"
//
// Only specifiers that resolve to a hashable asset are touched; everything
// else (URLs, plain strings, non-asset paths) passes through byte-identical.
func (t *assetTable) rewrite(importerDir, content string) string {
	if t == nil {
		return content
	}
	content = importSpecRe.ReplaceAllStringFunc(content, func(m string) string {
		groups := importSpecRe.FindStringSubmatch(m)
		if len(groups) != 2 {
			return m
		}
		repl, ok := t.hashedSpec(importerDir, groups[1])
		if !ok {
			return m
		}
		return strings.Replace(m, groups[1], repl, 1)
	})
	for orig, hashed := range t.byOriginal {
		content = strings.ReplaceAll(content, `"`+orig+`"`, `"`+hashed+`"`)
	}
	return content
}

// hashedSpec maps an import specifier (relative "./" or "../", or
// root-absolute "/") to its hashed form. ok=false when the specifier does
// not name a hashable asset (bare specifiers are not used in this
// codebase).
func (t *assetTable) hashedSpec(importerDir, spec string) (string, bool) {
	var orig string
	switch {
	case strings.HasPrefix(spec, "/"):
		orig = spec[1:]
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"):
		orig = filepath.ToSlash(path.Join(importerDir, spec))
	default:
		return "", false
	}
	hashed, ok := t.byOriginal[orig]
	if !ok {
		return "", false
	}
	_, base := path.Split(spec)
	return strings.TrimSuffix(spec, base) + path.Base(hashed), true
}

// hashedFor reports the original file that serves a hashed request path.
func (t *assetTable) hashedFor(p string) (string, bool) {
	if t == nil {
		return "", false
	}
	orig, ok := t.byHashed[p]
	return orig, ok
}

// servedBytes returns the bytes to serve for a hashable original path
// (rewritten JS), or (nil, false) for everything else.
func (t *assetTable) servedBytes(p string) ([]byte, bool) {
	if t == nil {
		return nil, false
	}
	b, ok := t.served[p]
	return b, ok
}

// fileList is the full precache list for /assets.json: the root document,
// the hashed shell, and the static files (icons, fonts, manifest, sw.js).
func (t *assetTable) fileList() []string {
	files := []string{"/", "/index.html"}
	if t == nil {
		return files
	}
	for _, s := range t.static {
		files = append(files, "/"+s)
	}
	for _, h := range t.byOriginal {
		files = append(files, "/"+h)
	}
	sort.Strings(files)
	return files
}
