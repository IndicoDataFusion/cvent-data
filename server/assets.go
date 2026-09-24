package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Content-hashed shell assets.
//
// Every .js/.css file under the web dir (except sw.js — its URL is fixed by
// navigator.serviceWorker.register) is served under an additional content
// hash in the file name: styles.css -> styles.<sha1>.css,
// sources/cvent.js -> sources/cvent.<sha1>.js (same directory, hash before
// the extension). index.html (and JS import specifiers) are rewritten at
// serve time to point at the hashed names.
//
// Why: a content change is a NEW URL, so a plain-HTTP client (phone on LAN,
// no service worker) can never serve a stale shell from its HTTP cache.
// Hashed paths get Cache-Control: immutable; index.html stays no-cache.
// /assets.json publishes the hashed list for the service worker's precache.
type assetTable struct {
	byOriginal map[string]string // "styles.css" -> "styles.<sha1>.css"
	byHashed   map[string]string // "styles.<sha1>.css" -> "styles.css"
	static     []string          // all other files, sorted ("icons/icon-192.png", "sw.js", ...)
}

// loadAssets walks webDir and builds the hash table. A missing webDir
// yields a nil table (the server then serves everything unhashed — the
// pre-hashing behavior), which the tests that use a minimal web dir rely on.
func loadAssets(webDir string) (*assetTable, error) {
	t := &assetTable{
		byOriginal: map[string]string{},
		byHashed:   map[string]string{},
	}
	err := filepath.WalkDir(webDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(webDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(rel))
		if ext == ".js" && rel != "sw.js" || ext == ".css" {
			b, rerr := os.ReadFile(path)
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
	sort.Strings(t.static)
	return t, nil
}

// rewrite rewrites quoted references from original to hashed asset paths in
// an HTML document or a JS module. Two reference forms occur:
//   - bare paths in index.html:   href="styles.css", src="app.js"
//   - relative specifiers in JS:  from "./cvent.js"  (registry.js imports
//     its sibling; the hash keeps the same directory, so only the base name
//     changes)
// Exact quoted matches are used, so "app.js" never clobbers "myapp.js" and
// an already-hashed name ("cvent.a1b2…js") can't contain the quoted
// original ("cvent.js") as a substring.
func (t *assetTable) rewrite(content string) string {
	for orig, hashed := range t.byOriginal {
		base := filepath.Base(orig)
		hashedBase := filepath.Base(hashed)
		content = strings.ReplaceAll(content, `"`+"./"+base+`"`, `"`+"./"+hashedBase+`"`)
		content = strings.ReplaceAll(content, `"`+orig+`"`, `"`+hashed+`"`)
	}
	return content
}

// hashedFor reports the original file that serves a hashed request path.
func (t *assetTable) hashedFor(p string) (string, bool) {
	if t == nil {
		return "", false
	}
	orig, ok := t.byHashed[p]
	return orig, ok
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
