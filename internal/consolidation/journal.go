package consolidation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// formatJournalEntry generates a markdown dream journal entry from the dream report.
func formatJournalEntry(dreport *DreamReport, timestamp time.Time) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("## %s -- Dream\n", timestamp.UTC().Format(time.RFC3339)))

	// Collect LLM narratives across all vault reports.
	var narratives []string
	for _, r := range dreport.Reports {
		if r.Journal != "" {
			narratives = append(narratives, r.Journal)
		}
	}
	if len(narratives) > 0 {
		sb.WriteString("\n")
		for _, n := range narratives {
			sb.WriteString(n)
			sb.WriteString("\n")
		}
	}

	// Aggregate stability counts.
	var strengthened, weakened int
	for _, r := range dreport.Reports {
		strengthened += r.StabilityStrengthened
		weakened += r.StabilityWeakened
	}
	sb.WriteString(fmt.Sprintf("\n**Strengthened:** %d memories reinforced\n", strengthened))
	sb.WriteString(fmt.Sprintf("**Weakened:** %d low-signal memories decayed\n", weakened))

	// Collect per-vault merge/contradiction stats.
	type vaultStats struct {
		name          string
		merged        int
		llmMerged     int
		contradictions int
	}
	var vaultLines []vaultStats
	for _, r := range dreport.Reports {
		if r.MergedEngrams > 0 || r.LLMMerged > 0 || r.LLMContradictions > 0 {
			vaultLines = append(vaultLines, vaultStats{
				name:          r.Vault,
				merged:        r.MergedEngrams,
				llmMerged:     r.LLMMerged,
				contradictions: r.LLMContradictions,
			})
		}
	}
	if len(vaultLines) > 0 {
		sb.WriteString("\n**Cleaned up:**\n")
		for _, vs := range vaultLines {
			if vs.merged > 0 {
				sb.WriteString(fmt.Sprintf("- Merged %d near-duplicate engrams in %s\n", vs.merged, vs.name))
			}
			if vs.llmMerged > 0 {
				sb.WriteString(fmt.Sprintf("- LLM merged %d engrams in %s\n", vs.llmMerged, vs.name))
			}
			if vs.contradictions > 0 {
				sb.WriteString(fmt.Sprintf("- Resolved %d contradictions in %s\n", vs.contradictions, vs.name))
			}
		}
	}

	// Summary line.
	var totalEngrams int
	var legalSkipped int
	for _, r := range dreport.Reports {
		if r.Orient != nil {
			totalEngrams += r.Orient.EngramCount
		}
		legalSkipped += r.LegalSkipped
	}
	vaultCount := len(dreport.Reports)
	duration := dreport.TotalDuration.Round(time.Millisecond)

	sb.WriteString(fmt.Sprintf(
		"\n*Scanned %d engrams across %d vaults (legal: %d skipped) in %s*\n",
		totalEngrams, vaultCount, legalSkipped, duration,
	))

	return sb.String()
}

// appendJournal writes the journal entry to ~/.muninn/dream.journal.md.
// Creates the file if it doesn't exist. Returns the path written to.
func appendJournal(entry string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("journal: get home dir: %w", err)
	}

	dir := filepath.Join(home, ".muninn")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("journal: mkdir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "dream.journal.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("journal: open %s: %w", path, err)
	}
	defer f.Close()

	if _, err := fmt.Fprintln(f, entry); err != nil {
		return "", fmt.Errorf("journal: write %s: %w", path, err)
	}

	return path, nil
}
