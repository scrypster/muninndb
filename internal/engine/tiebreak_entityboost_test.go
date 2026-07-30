package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestEntityBoost_TiedInjectionsDeterministic guards #698 on the entity-boost
// injection path. Eight target engrams share one rare entity with the seed, so
// each receives an identical boost (entityBoostFactor × idf) and ties. Before the
// ULID tie-break, applyEntityBoost appended injections in Go map-iteration order
// (pass 2b), so repeated calls on the SAME input produced different orderings and
// an arbitrary subset survived a MaxResults cutoff. This test runs applyEntityBoost
// 40× on identical input and requires the tied-injection order to be identical every
// run and equal to ascending-ULID. It is RED without the tie-break at
// engine_entity_boost.go's final sort.
func TestEntityBoost_TiedInjectionsDeterministic(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-tie-determinism"
	ws := eng.store.ResolveVaultPrefix(vault)

	// One rare entity, upserted once → df=1.
	require.NoError(t, eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "SharedEntity", Type: "topic", Source: "inline",
	}, "inline"))

	// Seed, linked to SharedEntity — the BFS result that triggers the boost.
	seed := &storage.Engram{Concept: "seed", Content: "seed about SharedEntity", Confidence: 0.9}
	idSeed, err := eng.store.WriteEngram(ctx, ws, seed)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idSeed, "SharedEntity"))
	fullSeed, err := eng.store.GetEngram(ctx, ws, idSeed)
	require.NoError(t, err)

	// Eight targets, each linked ONLY to SharedEntity → identical idf, identical
	// boost, all tie.
	const nTargets = 8
	targetSet := make(map[storage.ULID]struct{}, nTargets)
	for i := 0; i < nTargets; i++ {
		tg := &storage.Engram{Concept: fmt.Sprintf("t%d", i), Content: fmt.Sprintf("target %d re SharedEntity", i), Confidence: 0.8}
		id, err := eng.store.WriteEngram(ctx, ws, tg)
		require.NoError(t, err)
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, id, "SharedEntity"))
		targetSet[id] = struct{}{}
	}

	initial := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}
	req := &activation.ActivateRequest{Threshold: 0.05}

	orderOfTargets := func(res []activation.ScoredEngram) ([]storage.ULID, float64) {
		var ids []storage.ULID
		var tieScore float64
		for _, r := range res {
			if _, ok := targetSet[r.Engram.ID]; ok {
				ids = append(ids, r.Engram.ID)
				tieScore = r.Score
			}
		}
		return ids, tieScore
	}

	var first []storage.ULID
	for run := 0; run < 40; run++ {
		res := eng.applyEntityBoost(ctx, ws, 10, initial, req)
		got, tieScore := orderOfTargets(res)
		require.Len(t, got, nTargets, "run %d: all %d tied targets should be injected", run, nTargets)
		// Confirm the targets genuinely tie — else this wouldn't exercise the
		// tie-break (a same-score block is the whole point of #698).
		for _, r := range res {
			if _, ok := targetSet[r.Engram.ID]; ok {
				require.Equal(t, tieScore, r.Score, "run %d: targets must share one boost score to test tie-break", run)
			}
		}
		if run == 0 {
			first = got
		} else {
			require.Equal(t, first, got, "run %d: tied-injection order changed across identical calls (nondeterministic map-order)", run)
		}
	}

	// The deterministic order must be ascending ULID (the tie-break key).
	want := append([]storage.ULID(nil), first...)
	sort.Slice(want, func(i, j int) bool { return bytes.Compare(want[i][:], want[j][:]) < 0 })
	require.Equal(t, want, first, "tied injections must be ordered by ascending ULID")
}
