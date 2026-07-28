package auth

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// The plasticity presets are hand-duplicated between Go and the web console:
// the Go table here, and four separate places in web/. Nothing generates one
// from the other, and until this test nothing checked them, which
// docs/internals/drift-and-obligations.md tracks as a known drift surface
// (obligation #4).
//
// The failure this catches is adding or renaming a preset in Go and missing one
// of the UI sites — a preset the console cannot select, or a card that writes a
// name the engine will silently fall back to `default` for (COG-1). Both look
// fine in isolation and neither breaks a build.
//
// Deliberately NOT checked: the radar-chart numbers in `_plasticityData`. They
// are hand-tuned for visual separation and are not a faithful rendering of the
// preset values — e.g. knowledge-graph plots depth 1.00 where HopDepth/8 would
// be 0.50. Asserting on them would either fail immediately or force the chart
// to be less legible. If they are ever meant to be derived, that is a change to
// the chart, not to this test.
var webPresetSites = []struct {
	name    string
	relPath string
	pattern string // must contain exactly one capture group: the preset name
}{
	{"_plasticityData", "web/static/js/app.js", `(?s)_plasticityData:\s*\{(.*?)\}`},
	{"_plasticityColors", "web/static/js/app.js", `(?s)_plasticityColors:\s*\{(.*?)\n\s*\},`},
	{"plasticityPresetDescription", "web/static/js/app.js", `(?s)plasticityPresetDescription\(preset\)\s*\{.*?const d = \{(.*?)\n\s*\};`},
	{"preset cards", "web/templates/index.html", `(?s)(.*)`},
}

var (
	jsKeyRe   = regexp.MustCompile(`'([a-z][a-z-]*)':`)
	cardKeyRe = regexp.MustCompile(`preset\s*=\s*'([a-z][a-z-]*)'`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	// This file lives at <root>/internal/auth/.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q has no go.mod — test needs relocating: %v", root, err)
	}
	return root
}

func TestPlasticityPresets_WebConsoleParity(t *testing.T) {
	root := repoRoot(t)

	want := make([]string, 0, len(plasticityPresets))
	for name := range plasticityPresets {
		want = append(want, name)
	}
	sort.Strings(want)

	for _, site := range webPresetSites {
		t.Run(site.name, func(t *testing.T) {
			path := filepath.Join(root, site.relPath)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (did the web console move? update webPresetSites)", site.relPath, err)
			}

			section := string(src)
			if site.pattern != `(?s)(.*)` {
				m := regexp.MustCompile(site.pattern).FindStringSubmatch(section)
				if m == nil {
					t.Fatalf("could not locate %s in %s — the block was renamed or restructured; "+
						"update the pattern in webPresetSites rather than deleting this check",
						site.name, site.relPath)
				}
				section = m[1]
			}

			keyRe := jsKeyRe
			if site.name == "preset cards" {
				keyRe = cardKeyRe
			}
			seen := map[string]bool{}
			for _, m := range keyRe.FindAllStringSubmatch(section, -1) {
				seen[m[1]] = true
			}
			got := make([]string, 0, len(seen))
			for k := range seen {
				got = append(got, k)
			}
			sort.Strings(got)

			if len(got) == 0 {
				t.Fatalf("found no preset names in %s (%s) — the extraction broke, "+
					"which would make this test silently vacuous", site.name, site.relPath)
			}

			for _, w := range want {
				if !seen[w] {
					t.Errorf("preset %q exists in Go but is missing from %s (%s) — "+
						"the console cannot offer it", w, site.name, site.relPath)
				}
			}
			for _, g := range got {
				if _, ok := plasticityPresets[g]; !ok {
					t.Errorf("%s (%s) offers preset %q which Go does not define — "+
						"selecting it silently falls back to `default` (COG-1)",
						site.name, site.relPath, g)
				}
			}
		})
	}
}
