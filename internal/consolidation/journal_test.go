package consolidation

import (
	"strings"
	"testing"
	"time"
)

func TestFormatJournalEntry_WithLLMNarrative(t *testing.T) {
	ts := time.Date(2026, 3, 29, 6, 0, 0, 0, time.UTC)

	dreport := &DreamReport{
		Reports: []*ConsolidationReport{
			{
				Vault:                 "default",
				Journal:               "The engine surfaced three related memories about distributed systems.",
				MergedEngrams:         2,
				LLMContradictions:     1,
				StabilityStrengthened: 3,
				StabilityWeakened:     1,
				LegalSkipped:          0,
				Orient:                &VaultSummary{Vault: "default", EngramCount: 47},
			},
		},
		Skipped: []string{"legal-docs"},
	}

	entry := formatJournalEntry(dreport, ts)

	if !strings.Contains(entry, "2026-03-29T06:00:00Z") {
		t.Errorf("entry missing timestamp; got:\n%s", entry)
	}
	if !strings.Contains(entry, "Dream") {
		t.Errorf("entry missing 'Dream' header; got:\n%s", entry)
	}
	if !strings.Contains(entry, "The engine surfaced three related memories") {
		t.Errorf("entry missing LLM narrative; got:\n%s", entry)
	}
	if !strings.Contains(entry, "Merged 2") {
		t.Errorf("entry missing 'Merged 2'; got:\n%s", entry)
	}
	if !strings.Contains(entry, "legal") {
		t.Errorf("entry missing 'legal' mention; got:\n%s", entry)
	}
}

func TestFormatJournalEntry_NoLLM_MinimalSummary(t *testing.T) {
	ts := time.Date(2026, 3, 29, 8, 0, 0, 0, time.UTC)

	dreport := &DreamReport{
		Reports: []*ConsolidationReport{
			{
				Vault:  "work",
				Orient: &VaultSummary{Vault: "work", EngramCount: 10},
			},
		},
	}

	entry := formatJournalEntry(dreport, ts)

	if entry == "" {
		t.Fatal("expected non-empty journal entry")
	}
	if !strings.Contains(entry, "Dream") {
		t.Errorf("entry missing 'Dream' header; got:\n%s", entry)
	}
	if len(entry) <= 50 {
		t.Errorf("entry too short (%d chars); got:\n%s", len(entry), entry)
	}
}
