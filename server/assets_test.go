package main

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

// The whole module graph must be hash-addressed: no served JS module may
// still name an original (unhashed) asset path in its import specifiers.
// Guards the two-pass build in loadAssets — a single pass leaves app.js's
// import of sources/registry.js unrewritten (WalkDir visits root files
// before the sources/ dir, so the target's hash is unknown when app.js is
// processed).
func TestAllServedModulesReferenceOnlyHashedAssets(t *testing.T) {
	tbl, err := loadAssets("../web")
	if err != nil {
		t.Fatal(err)
	}
	if tbl == nil {
		t.Fatal("nil asset table for ../web")
	}
	specRe := regexp.MustCompile(`(?:from|import)\s*\(?\s*["']([^"']+)["']`)
	for rel, body := range tbl.served {
		dir := ""
		if i := strings.LastIndexByte(rel, '/'); i >= 0 {
			dir = rel[:i]
		}
		for _, m := range specRe.FindAllStringSubmatch(string(body), -1) {
			spec := m[1]
			var orig string
			switch {
			case strings.HasPrefix(spec, "/"):
				orig = spec[1:]
			case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"):
				orig = path.Join(dir, spec)
			default:
				continue // bare specifiers: not used in this codebase
			}
			if _, isAsset := tbl.byOriginal[orig]; isAsset {
				t.Errorf("%s: import %q still names the unhashed original %q", rel, spec, orig)
			}
		}
	}
}
