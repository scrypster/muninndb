package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// muninn_link's `relation` argument was SILENTLY coerced to relates_to when it
// was not one of the 16 recognised names — the same silent-substitution class as
// the memory-type downgrade fixed in #742, still live on the verb that matters
// most.
//
// relates_to is INERT: it is the one relation with no downstream consumer, so a
// mistyped relation does not merely land in the wrong bucket — the declaration
// evaporates. An agent that writes link(contradicts) but spells it
// "contradicted_by" gets a silent success and no contradiction flag, no
// confidence update, no adversarial-profile boost. An agent that means
// supersedes and mistypes it gets no ValidUntil stamp and no chain demotion: the
// stale fact keeps leading recall, which is the project's worst failure class.
//
// Why this verb specifically. Measured on a real 4,216-engram corpus (aggregate
// counts only): only 4.81% of writes carry ANY agent declaration, and of 139,866
// association edges just 189 (0.135%) were authored by an agent rather than by
// the Hebbian or cosine workers. Declarations are the scarcest resource in the
// system — and a counterfactual replay showed the content-based heuristic
// recovers 0 of 114 declared supersessions, because the intent is not in the
// content. Silently discarding one is discarding information nothing else can
// reconstruct.
//
// Storage behaviour is unchanged: the link is still created, still as
// relates_to. What changes is that the caller is TOLD, and given the valid
// names, at the only moment they can still correct it.
func TestRelTypeFromString_UnknownRelationIsReported(t *testing.T) {
	tests := []struct {
		name        string
		rel         string
		wantType    uint16
		wantUnknown string
	}{
		{
			// THE BUG: a plausible near-miss silently became inert relates_to.
			name:        "unrecognised relation is reported",
			rel:         "contradicted_by",
			wantType:    uint16(storage.RelRelatesTo),
			wantUnknown: "contradicted_by",
		},
		{
			name:        "supersedes is silent",
			rel:         "supersedes",
			wantType:    uint16(storage.RelSupersedes),
			wantUnknown: "",
		},
		{
			name:        "contradicts is silent",
			rel:         "contradicts",
			wantType:    uint16(storage.RelContradicts),
			wantUnknown: "",
		},
		{
			// An explicit relates_to is a real choice, not a miss.
			name:        "explicit relates_to is silent",
			rel:         "relates_to",
			wantType:    uint16(storage.RelRelatesTo),
			wantUnknown: "",
		},
		{
			// Empty is rejected upstream by the handler; treat as no report so
			// the notice never fires on a path that already errors.
			name:        "empty relation is silent",
			rel:         "",
			wantType:    uint16(storage.RelRelatesTo),
			wantUnknown: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, unknown := relTypeFromStringChecked(tc.rel)
			if unknown != tc.wantUnknown {
				t.Errorf("unknown relation reported = %q, want %q", unknown, tc.wantUnknown)
			}
			if got != tc.wantType {
				t.Errorf("RelType = %d, want %d (storage behaviour must not change)", got, tc.wantType)
			}
			// The unchecked helper must keep behaving identically for every
			// existing caller.
			if plain := relTypeFromString(tc.rel); plain != tc.wantType {
				t.Errorf("relTypeFromString drifted from the checked variant: %d vs %d", plain, tc.wantType)
			}
		})
	}
}

// The hint has to name the valid relations, or the caller learns nothing
// actionable — and it must say the link was stored as the inert relates_to, so
// the caller understands a declaration was lost rather than merely renamed.
func TestUnknownRelationHint_NamesTheValidRelations(t *testing.T) {
	hint := unknownRelationHint("contradicted_by")
	if hint == "" {
		t.Fatal("unknownRelationHint returned empty for an unrecognised relation")
	}
	if !strings.Contains(hint, "contradicted_by") {
		t.Errorf("hint must quote the rejected value; got %q", hint)
	}
	if !strings.Contains(hint, "relates_to") {
		t.Errorf("hint must state the link was stored as relates_to; got %q", hint)
	}
	// Every relation the map accepts must be offered.
	for rel := range relTypeMap {
		if !strings.Contains(hint, rel) {
			t.Errorf("hint must list the valid relation %q; got %q", rel, hint)
		}
	}
	if unknownRelationHint("") != "" {
		t.Error("unknownRelationHint must be empty when nothing was rejected")
	}
}

// Anti-drift pin: no relation the map accepts may ever be reported unknown. If
// someone adds a RelType, this forces the hint vocabulary to learn it rather
// than letting it become a new class of silently-discarded declaration.
func TestRelTypeFromStringChecked_NoValidRelationIsReportedUnknown(t *testing.T) {
	for rel := range relTypeMap {
		t.Run(rel, func(t *testing.T) {
			if _, unknown := relTypeFromStringChecked(rel); unknown != "" {
				t.Errorf("valid relation %q was reported unknown (%q) — false alarm", rel, unknown)
			}
		})
	}
}
