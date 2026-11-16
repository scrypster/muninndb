package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRecentModeWording_NoVaultDefaultEquivalence: a mode sent on the wire
// REPLACES the vault's default recall mode, so mode="recent" scores exactly
// like mode="balanced" (engine defaults) — NOT like "the vault default". On a
// vault whose default mode is semantic, a no-mode recall and a recent recall
// differ (TestActivate_RecentMode_EqualsDefault/wire_recent_ignores_vault_default).
// No surface may claim the false equivalence, and no doc may still describe
// recent as recency-weighted.
func TestRecentModeWording_NoVaultDefaultEquivalence(t *testing.T) {
	banned := []string{
		"alias of the vault default",
		"scores exactly like the default",
		"like the vault default",
		"identical to the vault default",
		"temporal-dominant",
		"recency-biased",
		"prioritize recently created",
		"same results as balanced", // the Balanced button sends NO mode (app.js), so it is the vault default
	}
	texts := map[string]string{}

	for _, tool := range allToolDefinitions() {
		if tool.Name == "muninn_recall" {
			texts["tool muninn_recall"] = strings.ToLower(mustJSON(tool))
		}
	}
	if texts["tool muninn_recall"] == "" {
		t.Fatal("muninn_recall tool definition not found")
	}
	on := true
	texts["guide single"] = strings.ToLower(generateGuide("v", auth.ResolvePlasticity(nil), engineStats{}, ""))
	texts["guide shared"] = strings.ToLower(generateGuide("v", auth.ResolvePlasticity(&auth.PlasticityConfig{MultiUser: &on}), engineStats{}, ""))
	texts["empty-result hint"] = strings.ToLower(recallHint(t, &abstainedFakeEngine{resp: &mbp.ActivateResponse{Abstained: true, AbstainedReason: "below_threshold"}},
		`{"vault":"default","context":["anything"],"mode":"recent"}`))

	for _, rel := range []string{
		"internal/transport/rest/openapi.yaml",
		"docs/feature-reference.md",
		"docs/retrieval-design.md",
		"docs/internals/invariants.md",
		"internal/auth/recall_modes.go",
		"web/templates/index.html",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		texts[rel] = strings.ToLower(string(b))
	}

	for name, text := range texts {
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s claims %q — recent equals balanced (engine defaults, ignoring the vault's default mode), not the vault default", name, b)
			}
		}
	}
	if !strings.Contains(texts["tool muninn_recall"], "balanced") || !strings.Contains(texts["tool muninn_recall"], "ignor") {
		t.Error("muninn_recall's mode description must state recent equals balanced and ignores the vault's default mode")
	}
}
