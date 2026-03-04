package engine

import (
	"testing"

	"github.com/scrypster/muninndb/internal/provenance"
)

// TestSourceTypeString verifies that sourceTypeString returns the correct label
// for each of the 7 defined SourceType constants and returns "" for an unknown value.
func TestSourceTypeString(t *testing.T) {
	cases := []struct {
		input provenance.SourceType
		want  string
	}{
		{provenance.SourceHuman, "human"},
		{provenance.SourceLLM, "llm"},
		{provenance.SourceDocument, "document"},
		{provenance.SourceInferred, "inferred"},
		{provenance.SourceExternal, "external"},
		{provenance.SourceWorkingMem, "working_memory"},
		{provenance.SourceSynthetic, "synthetic"},
		{provenance.SourceType(99), ""},
	}

	for _, tc := range cases {
		got := sourceTypeString(tc.input)
		if got != tc.want {
			t.Errorf("sourceTypeString(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
