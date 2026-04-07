package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/scrypster/muninndb/internal/storage"
)

type trustTier int

const (
	trustSkip       trustTier = iota // legal: no LLM ever
	trustRestricted                  // work, personal: Ollama or Anthropic only
	trustOpen                        // everything else: any provider
)

func vaultTrustTier(vault string) trustTier {
	if isLegalVault(vault) {
		return trustSkip
	}
	lower := strings.ToLower(vault)
	if lower == "work" || lower == "personal" {
		return trustRestricted
	}
	return trustOpen
}

func resolveProvider(vault string, ollama, anthropic, openai LLMProvider) LLMProvider {
	tier := vaultTrustTier(vault)
	if tier == trustSkip {
		return nil
	}
	if ollama != nil {
		return ollama
	}
	if anthropic != nil {
		return anthropic
	}
	if tier == trustOpen && openai != nil {
		return openai
	}
	return nil
}

// DedupCluster represents a group of near-duplicate engrams for LLM review.
type DedupCluster struct {
	Members []ClusterMember
}

// ClusterMember is a single engram within a dedup cluster.
type ClusterMember struct {
	ID      string
	Concept string
	Content string
}

// dreamSystemPrompt instructs the LLM to review near-duplicate engram clusters.
const dreamSystemPrompt = `You are MuninnDB's dream consolidation engine. You review clusters of semantically similar engrams and decide which should be merged, which contradict each other, and whether any cross-vault connections exist.

Respond ONLY with a JSON object (no markdown fences, no commentary) matching this schema:

{
  "merges": [
    {
      "source_ids": ["id1", "id2"],
      "reason": "why these should be merged"
    }
  ],
  "contradictions": [
    {
      "kept_id": "id_to_keep",
      "superseded_id": "id_to_archive",
      "reason": "why this is a contradiction and which is more current/accurate"
    }
  ],
  "cross_vault_suggestions": [
    {
      "source_id": "id_in_this_vault",
      "target_vault": "other_vault_name",
      "reason": "why a cross-vault link would be useful"
    }
  ],
  "stability_recommendations": [
    {
      "id": "engram_id",
      "direction": "strengthen|weaken",
      "reason": "why"
    }
  ],
  "journal": "A 1-2 sentence narrative summary of what you found and did."
}

Rules:
- Only merge engrams that are truly duplicates (same fact, different wording).
- Flag contradictions only when two engrams assert incompatible facts.
- Be conservative: when unsure, leave engrams alone.
- The journal should be a human-readable summary, not a list of IDs.`

// DreamLLMResponse is the parsed response from the dream LLM.
type DreamLLMResponse struct {
	Merges          []dreamMerge          `json:"merges"`
	Contradictions  []dreamContradiction  `json:"contradictions"`
	CrossVaultSuggs []dreamCrossVaultSugg `json:"cross_vault_suggestions"`
	StabilityRecs   []dreamStabilityRec   `json:"stability_recommendations"`
	Journal         string                `json:"journal"`
}

type dreamMerge struct {
	SourceIDs []string `json:"source_ids"`
	Reason    string   `json:"reason"`
}

type dreamContradiction struct {
	KeptID       string `json:"kept_id"`
	SupersededID string `json:"superseded_id"`
	Reason       string `json:"reason"`
}

type dreamCrossVaultSugg struct {
	SourceID    string `json:"source_id"`
	TargetVault string `json:"target_vault"`
	Reason      string `json:"reason"`
}

type dreamStabilityRec struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Reason    string `json:"reason"`
}

// parseDreamResponse extracts a DreamLLMResponse from the raw LLM output.
// It handles JSON optionally wrapped in markdown code fences.
func parseDreamResponse(raw string) (*DreamLLMResponse, error) {
	cleaned := strings.TrimSpace(raw)

	// Strip markdown code fences if present.
	if strings.HasPrefix(cleaned, "```") {
		// Remove opening fence (```json or ```)
		if idx := strings.Index(cleaned, "\n"); idx != -1 {
			cleaned = cleaned[idx+1:]
		}
		// Remove closing fence
		if idx := strings.LastIndex(cleaned, "```"); idx != -1 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}

	var resp DreamLLMResponse
	if err := json.Unmarshal([]byte(cleaned), &resp); err != nil {
		return nil, fmt.Errorf("parse dream LLM response: %w", err)
	}
	return &resp, nil
}

// buildClusterPrompt formats dedup clusters into a user prompt for the LLM.
// TODO: engram content is interpolated as-is — a crafted engram could attempt
// prompt injection. Consider JSON-encoding cluster members to reduce the attack surface.
func buildClusterPrompt(clusters []DedupCluster, vault string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Vault: %s\n\n", vault)
	fmt.Fprintf(&b, "Review these %d clusters of semantically similar engrams:\n\n", len(clusters))

	for i, cluster := range clusters {
		fmt.Fprintf(&b, "--- Cluster %d ---\n", i+1)
		for _, m := range cluster.Members {
			fmt.Fprintf(&b, "ID: %s\nConcept: %s\nContent: %s\n\n", m.ID, m.Concept, m.Content)
		}
	}

	return b.String()
}

// runPhase2bLLMConsolidation sends near-duplicate clusters to an LLM for review
// and applies the recommended merges and contradiction resolutions.
func (w *Worker) runPhase2bLLMConsolidation(
	ctx context.Context,
	store *storage.PebbleStore,
	wsPrefix [8]byte,
	report *ConsolidationReport,
	vault string,
	clusters []DedupCluster,
) error {
	provider := resolveProvider(vault, w.OllamaLLM, w.AnthropicLLM, w.OpenAILLM)
	if provider == nil {
		slog.Info("dream: phase 2b skipped (no eligible LLM provider for vault)", "vault", vault)
		return nil
	}

	if len(clusters) == 0 {
		slog.Debug("dream: phase 2b skipped (no clusters for LLM review)", "vault", vault)
		return nil
	}

	userPrompt := buildClusterPrompt(clusters, vault)

	raw, err := provider.Complete(ctx, dreamSystemPrompt, userPrompt)
	if err != nil {
		slog.Warn("dream: phase 2b LLM call failed", "vault", vault, "error", err)
		report.Errors = append(report.Errors, "phase2b_llm: "+err.Error())
		return nil // non-fatal
	}

	resp, err := parseDreamResponse(raw)
	if err != nil {
		slog.Warn("dream: phase 2b LLM response parse failed", "vault", vault, "error", err)
		report.Errors = append(report.Errors, "phase2b_llm_parse: "+err.Error())
		return nil // non-fatal
	}

	// Apply merges: archive all but first source_id.
	for _, merge := range resp.Merges {
		if len(merge.SourceIDs) < 2 {
			continue
		}
		for _, id := range merge.SourceIDs[1:] {
			if !w.DryRun {
				if err := w.Engine.UpdateLifecycleState(ctx, vault, id, "archived"); err != nil {
					slog.Warn("dream: phase 2b merge archive failed", "id", id, "error", err)
					continue
				}
			}
			report.LLMMerged++
		}
	}

	// Apply contradictions: archive superseded_id.
	for _, c := range resp.Contradictions {
		if c.SupersededID == "" {
			continue
		}
		if !w.DryRun {
			if err := w.Engine.UpdateLifecycleState(ctx, vault, c.SupersededID, "archived"); err != nil {
				slog.Warn("dream: phase 2b contradiction archive failed", "id", c.SupersededID, "error", err)
				continue
			}
		}
		report.LLMContradictions++
	}

	// Log cross-vault suggestions (informational only, don't create links).
	for _, s := range resp.CrossVaultSuggs {
		slog.Info("dream: phase 2b cross-vault suggestion",
			"vault", vault,
			"source_id", s.SourceID,
			"target_vault", s.TargetVault,
			"reason", s.Reason,
		)
		report.LLMSuggestions++
	}

	// Set journal from LLM response.
	if resp.Journal != "" {
		report.Journal = resp.Journal
	}

	slog.Info("dream: phase 2b completed",
		"vault", vault,
		"merged", report.LLMMerged,
		"contradictions", report.LLMContradictions,
		"suggestions", report.LLMSuggestions,
	)

	return nil
}
