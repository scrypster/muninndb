package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// A caller-supplied `type` that is not one of the recognised MemoryType enum
// names was SILENTLY downgraded to TypeFact: the value was kept as a cosmetic
// TypeLabel, the memory was filed as a plain fact, and nothing told the writer
// their type had been discarded. That is a direct violation of the project's
// first principle — "explicit config is never silently substituted" — applied to
// the one argument that decides whether a memory can ever participate in a
// typed/graph capability.
//
// Measured blast radius on a real 4,216-engram corpus (2026-07-28 census,
// aggregate counts only): 66.3% of all memories were typed by their writer but
// untyped by the type system, TypeGoal and TypeIdentity were 0 corpus-wide, and
// only 4.2% of memories cleared a decision/constraint/goal gate at all. The loss
// is unrecoverable after the fact — the discarded labels encode topic, not kind,
// so no normalisation pass rebuilds them (a salvage attempt recovered 1.1%).
//
// The fix does not reject the write and does not change what is stored: it makes
// the miss LOUD, returning a hint that names the valid types at the only moment
// the writer still has the context to correct it. That is the same
// degrade-loudly-never-silently-wrong shape as #578/#740, delivered over the
// Hint channel already used to nudge about malformed entities.
func TestApplyTypeArgs_UnknownTypeIsReportedNotSwallowed(t *testing.T) {
	tests := []struct {
		name        string
		args        map[string]any
		wantType    uint8
		wantLabel   string
		wantUnknown string // the value the caller should be told was not recognised
	}{
		{
			// THE BUG: a plausible-but-unrecognised kind. Silently became "fact".
			name:        "unrecognised type is reported",
			args:        map[string]any{"type": "insight"},
			wantType:    uint8(storage.TypeFact),
			wantLabel:   "insight",
			wantUnknown: "insight",
		},
		{
			// A valid enum name must stay silent — no false alarm.
			name:        "valid type is silent",
			args:        map[string]any{"type": "decision"},
			wantType:    uint8(storage.TypeDecision),
			wantLabel:   "decision",
			wantUnknown: "",
		},
		{
			// Omitting type entirely is not a miss; it is the documented default.
			name:        "absent type is silent",
			args:        map[string]any{},
			wantType:    0,
			wantLabel:   "",
			wantUnknown: "",
		},
		{
			// An explicit type_label is a display concern and must not suppress
			// the report — the TYPE was still discarded.
			name:        "unrecognised type still reported when type_label given",
			args:        map[string]any{"type": "insight", "type_label": "my_label"},
			wantType:    uint8(storage.TypeFact),
			wantLabel:   "my_label",
			wantUnknown: "insight",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &mbp.WriteRequest{}
			gotUnknown := applyTypeArgs(tc.args, req)
			if gotUnknown != tc.wantUnknown {
				t.Errorf("unknown type reported = %q, want %q", gotUnknown, tc.wantUnknown)
			}
			if req.MemoryType != tc.wantType {
				t.Errorf("MemoryType = %d, want %d (storage behaviour must not change)", req.MemoryType, tc.wantType)
			}
			if req.TypeLabel != tc.wantLabel {
				t.Errorf("TypeLabel = %q, want %q (storage behaviour must not change)", req.TypeLabel, tc.wantLabel)
			}
		})
	}
}

// The hint must actually name the valid types — a bare "unrecognised type"
// leaves the writer no better off, and the whole point is that the correction is
// possible at the moment of the miss.
func TestUnknownTypeHint_NamesTheValidTypes(t *testing.T) {
	hint := unknownTypeHint("insight")
	if hint == "" {
		t.Fatal("unknownTypeHint returned empty for an unrecognised type")
	}
	if !strings.Contains(hint, "insight") {
		t.Errorf("hint must quote the rejected value so the writer knows what was dropped; got %q", hint)
	}
	// Every enum name ParseMemoryType accepts must be offered.
	for _, name := range []string{
		"fact", "decision", "observation", "preference", "issue", "bugfix",
		"bug_report", "task", "procedure", "event", "experience", "goal", "constraint",
	} {
		if !strings.Contains(hint, name) {
			t.Errorf("hint must list the valid type %q so the writer can correct it; got %q", name, hint)
		}
	}
	// And it must say the type was not stored, not merely that it was odd.
	if !strings.Contains(strings.ToLower(hint), "fact") {
		t.Errorf("hint must state what the memory was stored as; got %q", hint)
	}
	if unknownTypeHint("") != "" {
		t.Error("unknownTypeHint must be empty when nothing was rejected")
	}
}

// Every name ParseMemoryType accepts must round-trip through applyTypeArgs
// WITHOUT being reported as unknown. This is the anti-drift pin: if someone adds
// a MemoryType to the enum, the hint text and this test force it to be handled
// rather than silently becoming a new class of swallowed write.
func TestApplyTypeArgs_NoValidTypeIsEverReportedUnknown(t *testing.T) {
	for _, name := range []string{
		"fact", "decision", "observation", "preference", "issue", "bugfix",
		"bug_report", "task", "procedure", "event", "experience", "goal", "constraint",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := storage.ParseMemoryType(name); !ok {
				t.Fatalf("test vocabulary drifted: ParseMemoryType rejects %q", name)
			}
			req := &mbp.WriteRequest{}
			if got := applyTypeArgs(map[string]any{"type": name}, req); got != "" {
				t.Errorf("valid type %q was reported unknown (%q) — false alarm", name, got)
			}
		})
	}
}
