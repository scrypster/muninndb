package engine

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestConceptFromText(t *testing.T) {
	const max = 60
	for _, tc := range []struct {
		name, in, want string
		cut            bool
	}{
		{"fits", "Re-lay the siding.", "Re-lay the siding.", false},
		{"exactly at cap", strings.Repeat("a", max), strings.Repeat("a", max), false},
		{"sentence boundary", "Re-lay the siding with flat-bottom rail. Then ballast it and tamp twice.", "Re-lay the siding with flat-bottom rail.", true},
		{"word boundary", "Re-lay the siding with flat-bottom rail on concrete sleepers throughout the yard", "Re-lay the siding with flat-bottom rail on concrete…", true},
		{"no boundary hard cut", strings.Repeat("x", 100), strings.Repeat("x", max-len("…")) + "…", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := conceptFromText(tc.in, max)
			if got != tc.want || cut != tc.cut {
				t.Fatalf("conceptFromText(%q) = (%q, %v), want (%q, %v)", tc.in, got, cut, tc.want, tc.cut)
			}
			if len(got) > max {
				t.Fatalf("len %d > cap %d", len(got), max)
			}
		})
	}
	t.Run("never splits a rune", func(t *testing.T) {
		in := strings.Repeat("é", 100) // 2 bytes each, no spaces
		got, _ := conceptFromText(in, max)
		if !utf8.ValidString(got) || len(got) > max {
			t.Fatalf("invalid or over-cap concept %q (%d bytes)", got, len(got))
		}
	})
}

func TestConceptFromText_AdversarialShapes(t *testing.T) {
	const max = 60
	hasWord := func(s string) bool {
		return strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) && r != '…' }) >= 0
	}
	for _, tc := range []struct{ name, in string }{
		{"leading whitespace run", strings.Repeat(" ", 200) + "Re-lay the siding with flat-bottom rail on concrete sleepers throughout."},
		{"leading newlines", strings.Repeat("\n\t", 80) + "Re-lay the siding."},
		{"long first word then words", strings.Repeat("x", 70) + " then the rest of the decision text follows here"},
		{"combining marks at the cut", "a" + strings.Repeat("e\u0301", 40)},
		{"zwj emoji sequence at the cut", "abcde" + strings.Repeat("\U0001F468\u200D\U0001F469\u200D\U0001F467", 10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := conceptFromText(tc.in, max)
			if len(got) > max || !utf8.ValidString(got) {
				t.Fatalf("concept %q: %d bytes, valid=%v", got, len(got), utf8.ValidString(got))
			}
			if hasWord(tc.in) && !hasWord(got) {
				t.Fatalf("concept %q has no words though the text does", got)
			}
			if got != strings.TrimLeftFunc(got, unicode.IsSpace) {
				t.Errorf("concept %q starts with whitespace", got)
			}
			// No dangling combining mark / joiner at the cut.
			trimmed := strings.TrimSuffix(got, "…")
			if r, _ := utf8.DecodeLastRuneInString(trimmed); r == '\u200D' {
				t.Errorf("concept %q ends on a zero-width joiner (split grapheme)", got)
			}
			if rest := strings.TrimPrefix(strings.TrimLeftFunc(tc.in, unicode.IsSpace), trimmed); rest != "" {
				if r, _ := utf8.DecodeRuneInString(rest); unicode.Is(unicode.Mn, r) {
					t.Errorf("cut %q is followed by combining mark %U (split grapheme)", got, r)
				}
			}
		})
	}
}
