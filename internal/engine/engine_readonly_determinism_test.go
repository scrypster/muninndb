package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// A read-only recall must be a pure read: repeating it back to back on an
// unchanged store must return the same ids with the same scores.
//
// The defect this pins: applyEntityBoost reads every entity-injection
// candidate with store.GetEngram on the REQUEST ctx. Phase 6 suppresses the
// L1 cache recency stamp for a read-only call (storage.ContextWithNoAccessCacheStamp),
// but the post-pipeline phases did not inherit that suppression, so the
// injection read stamped lastAccess=now on every injected engram. The NEXT
// recall reads that stamp back through EngramLastAccessNs as the engram's
// ACT-R recency — a cold, months-old memory suddenly scores as "accessed a
// moment ago" because a read-only call glanced at it. Measured on a real
// production vault clone: only 31 of 40 exact-concept queries returned
// identical results across 5 back-to-back read-only calls.
//
// Fixture shape (each property is load-bearing):
//   - the target is written at STORE level with a LastAccess 400 days old, so
//     it is not in the L1 cache before the first recall (WriteEngram does not
//     cache) and its persisted recency is genuinely cold;
//   - its lexical match is real (ContentMatch above the 0.1 gate) but its cold
//     ACT-R base level keeps its absolute score below the gate, so on the
//     first call it reaches the results ONLY through entity-boost injection —
//     the GetEngram that stamped;
//   - it shares two rare entities with a fresh, strongly matching seed, so the
//     injection clears the threshold.
//
// On develop the second call admits the target through the pipeline at a
// "fresh" ACT-R score plus the entity boost, so its score jumps.
func TestActivateReadOnly_RepeatedCallsAreIdentical_EntityBoostInjection(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "trail-log"
	ws := eng.store.ResolveVaultPrefix(vault)
	place, person := "Falcon Spur", "Odalys Prewitt"

	// Fillers: unrelated vocabulary, engine-written so the vault count (the
	// entity-rarity n) is real.
	for i := 0; i < 10; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("hive %d frames checked", i),
			Content: fmt.Sprintf("brood pattern on frame set %d looked even after the cold snap", i),
		})
		require.NoError(t, err)
	}

	// Seed: fresh, matches every query term, carries both entities.
	seedResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "culvert headwall rebuilt",
		Content: "the crew rebuilt the culvert headwall on the spur after spring runoff",
		Entities: []mbp.InlineEntity{
			{Name: place, Type: "place"},
			{Name: person, Type: "person"},
		},
	})
	require.NoError(t, err)

	// Target: cold (400 days since last access), partial lexical match, same
	// two entities. Store-level so nothing has read it into the L1 cache.
	old := time.Now().Add(-400 * 24 * time.Hour)
	target := &storage.Engram{
		Concept:    "culvert headwall inspection",
		Content:    "notes on the culvert headwall stonework",
		Confidence: 1.0,
		CreatedAt:  old,
		UpdatedAt:  old,
		LastAccess: old,
	}
	targetID, err := eng.store.WriteEngram(ctx, ws, target)
	require.NoError(t, err)
	require.NoError(t, eng.fts.IndexEngram(ws, [16]byte(targetID), target.Concept, "", target.Content, nil))
	for _, name := range []string{place, person} {
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, targetID, name))
		upsertEntityTimes(t, eng, ws, name, 1)
	}
	awaitFTS(t, eng)
	require.Zero(t, eng.store.EngramLastAccessNs(ws, targetID),
		"fixture: the cold target must not be in the L1 cache before the first recall")

	req := func() *mbp.ActivateRequest {
		return &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"culvert headwall rebuilt"},
			MaxResults: 10,
			ReadOnly:   true,
		}
	}
	type row struct {
		id    string
		score float32
	}
	snapshot := func(resp *mbp.ActivateResponse) []row {
		out := make([]row, len(resp.Activations))
		for i, a := range resp.Activations {
			out[i] = row{a.ID, a.Score}
		}
		return out
	}

	first, err := eng.Activate(ctx, req())
	require.NoError(t, err)
	want := snapshot(first)

	// Fixture sanity: the path under test actually ran — the seed ranked and
	// the cold target is in the result set (it can only be there via injection).
	var sawSeed, sawTarget bool
	for _, r := range want {
		sawSeed = sawSeed || r.id == seedResp.ID
		sawTarget = sawTarget || r.id == targetID.String()
	}
	require.True(t, sawSeed, "fixture: seed must rank")
	require.True(t, sawTarget, "fixture: cold target must be entity-boost injected")

	// The direct invariant: a read-only recall stamps nothing.
	require.Zero(t, eng.store.EngramLastAccessNs(ws, targetID),
		"a read-only recall stamped the L1 recency of an entity-boost injection; the next recall will score it as just accessed")

	for i := 2; i <= 5; i++ {
		resp, err := eng.Activate(ctx, req())
		require.NoError(t, err)
		require.Equal(t, want, snapshot(resp), "read-only call %d differs from call 1 on an unchanged store", i)
	}
}
