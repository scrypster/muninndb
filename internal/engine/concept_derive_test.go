package engine

import (
	"strings"
	"testing"
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
