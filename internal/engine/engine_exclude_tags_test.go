package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// End-to-end coverage for the per-vault exclude-tags recall knob (#713):
// candidates carrying a configured tag are dropped from recall RANKING, while
// remaining reachable by explicit direct-id read (ranking-only) and by an
// explicit per-request tag include (caller intent overrides the standing
// exclude). All fixtures use invented synthetic tags/content only.

// writeTagged writes an engram carrying tag and returns its ID.
func writeTagged(t *testing.T, eng *Engine, ctx context.Context, vault, concept, tag string) string {
	t.Helper()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: concept,
		// Distinct per-engram content (concept-prefixed) so the two fixtures do
		// not collapse under content dedup, while still sharing the query terms
		// so both are FTS candidates for the shared recall context.
		Content: concept + " common searchable body text for recall",
		Tags:    []string{tag},
	})
	if err != nil {
		t.Fatalf("Write(%s): %v", concept, err)
	}
	return resp.ID
}

// recallIDs runs a recall and returns the set of returned engram IDs.
func recallIDs(t *testing.T, eng *Engine, ctx context.Context, req *mbp.ActivateRequest) map[string]bool {
	t.Helper()
	resp, err := eng.Activate(ctx, req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	got := make(map[string]bool, len(resp.Activations))
	for _, a := range resp.Activations {
		got[a.ID] = true
	}
	return got
}

// TestExcludeTags_DefaultIdentity pins that a vault with NO exclude-tags config
// returns a tagged engram exactly as before — the default-empty / opt-in
// invariant. This is the byte-identical-to-today control for the drop test.
func TestExcludeTags_DefaultIdentity(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "excl-default"
	if err := as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	noiseID := writeTagged(t, eng, ctx, vault, "alpha topic", "noise-tag")
	keepID := writeTagged(t, eng, ctx, vault, "beta topic", "keep-tag")
	awaitFTS(t, eng)

	got := recallIDs(t, eng, ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"common searchable body text for recall"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if !got[noiseID] {
		t.Errorf("default (no ExcludeTags): tagged engram was dropped — expected identity behavior")
	}
	if !got[keepID] {
		t.Errorf("default (no ExcludeTags): untagged-relative engram missing from recall")
	}
}

// TestExcludeTags_DropsFromRanking is the core feature test: a candidate
// carrying an excluded tag is dropped from recall ranking, a non-excluded
// candidate still ranks, and the dropped engram remains reachable by explicit
// direct-id read (exclusion is ranking-only).
func TestExcludeTags_DropsFromRanking(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "excl-drop"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vault,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ExcludeTags: []string{"noise-tag"}},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	noiseID := writeTagged(t, eng, ctx, vault, "alpha topic", "noise-tag")
	keepID := writeTagged(t, eng, ctx, vault, "beta topic", "keep-tag")
	awaitFTS(t, eng)

	got := recallIDs(t, eng, ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"common searchable body text for recall"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if got[noiseID] {
		t.Errorf("excluded-tag engram leaked into recall ranking (#713 drop failed)")
	}
	if !got[keepID] {
		t.Errorf("non-excluded engram was dropped — exclusion must be tag-scoped, not blanket")
	}

	// Ranking-only: the excluded engram is still retrievable by direct id.
	rd, err := eng.Read(ctx, &mbp.ReadRequest{ID: noiseID, Vault: vault})
	if err != nil {
		t.Fatalf("Read(excluded id): %v — exclusion must be ranking-only, not a hide", err)
	}
	if rd.ID != noiseID {
		t.Errorf("Read returned id %q, want %q", rd.ID, noiseID)
	}
}

// TestExcludeTags_ExplicitIncludeOverrides pins that an explicit per-request tag
// include (tags_any naming an excluded tag) overrides the standing exclude —
// the caller can always reach an excluded tag on purpose.
func TestExcludeTags_ExplicitIncludeOverrides(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "excl-override"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vault,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ExcludeTags: []string{"noise-tag"}},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	noiseID := writeTagged(t, eng, ctx, vault, "alpha topic", "noise-tag")
	awaitFTS(t, eng)

	got := recallIDs(t, eng, ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"common searchable body text for recall"},
		MaxResults: 10,
		Threshold:  0.0,
		Filters:    []mbp.Filter{{Field: "tags_any", Value: []string{"noise-tag"}}},
	})
	if !got[noiseID] {
		t.Errorf("explicit tags_any include did not override the standing exclude (#713 override failed)")
	}
}
