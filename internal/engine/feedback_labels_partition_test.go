package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Security reviews 2 and 3: no in-memory bound may decide an effect on
// stored state, and what stays in memory (metrics) is fixed-size.

// seedN writes n distinct trail memories and returns their ids.
func (le *labelEnv) seedN(ctx context.Context, vault string, n int) []string {
	le.t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		r, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: fmt.Sprintf("trail marker %d", i),
			Content: fmt.Sprintf("ridge trail drainage marker post number %d", i)})
		require.NoError(le.t, err)
		ids = append(ids, r.ID)
	}
	awaitFTS(le.t, le.eng)
	return ids
}

func markerFor(le *labelEnv, vault, mem, token string) *storage.RecallDayMarker {
	le.t.Helper()
	m, err := le.store.GetRecallDayMarker(context.Background(), le.store.ResolveVaultPrefix(vault), unixDay(time.Now()),
		mustParseULID(le.t, mem), le.eng.qidKey.CredHash(token), storage.RecallPurposeLabel)
	require.NoError(le.t, err)
	return m
}

// TestFeedback_MetricBucketExhaustionChangesNothingForOthers: the in-memory
// tally is metrics only. Filling it (and forcing evictions) with one
// principal's rejections across many vaults changes nothing about what
// another credential, or another vault, is granted: B's first positive label
// of the day still bumps once and still has its own day marker; A cannot
// obtain a second bump for a memory it already bumped today.
func TestFeedback_MetricBucketExhaustionChangesNothingForOthers(t *testing.T) {
	old := labelMaxTrackedVaults
	labelMaxTrackedVaults = 2
	t.Cleanup(func() { labelMaxTrackedVaults = old })

	le := newLabelEnv(t, testSecret)
	a, b := caller("tok-a", "sess-a"), caller("tok-b", "sess-b")
	const vault, vaultC = "metric-v", "metric-c"
	le.seed(a, vault)
	le.seed(a, vaultC)
	for i := 0; i < 6; i++ { // A fills and churns the tally across vaults
		v := fmt.Sprintf("metric-fill-%d", i)
		le.seedN(a, v, 1)
		le.label(a, v, storage.NewULID().String(), true, storage.NewULID().String())
	}
	le.eng.labels.mu.Lock()
	require.LessOrEqual(t, len(le.eng.labels.vaults), 2)
	le.eng.labels.mu.Unlock()

	resp := le.recall(a, vault, false)
	x := resp.Activations[0].ID
	le.label(a, vault, resp.QueryID, true, x)
	le.label(a, vault, le.recall(a, vault, true).QueryID, true, x)
	require.Equal(t, uint32(1), le.accessCount(vault, x), "A: one bump, however many ids")

	le.label(b, vault, le.recall(b, vault, false).QueryID, true, x)
	require.Equal(t, uint32(2), le.accessCount(vault, x), "B's first positive label still earns its own bump")
	mb := markerFor(le, vault, x, "tok-b")
	require.NotNil(t, mb)
	require.NotZero(t, mb.BumpedAt)
	require.Equal(t, uint32(1), mb.Posts)

	rc := le.recall(a, vaultC, false)
	le.label(a, vaultC, rc.QueryID, true, rc.Activations[0].ID)
	require.Equal(t, uint32(1), le.accessCount(vaultC, rc.Activations[0].ID), "vault C unaffected")
}

// TestFeedback_ManyDistinctLabelsInMemoryBounded: one credential posting
// 100,000 distinct verified labels (10,000 read-only recalls × 10 rows)
// leaves the in-memory state empty — every decision is durable — and each
// memory is bumped at most once. Durable growth is exactly one label row per
// (query, memory) and one day marker per memory.
func TestFeedback_ManyDistinctLabelsInMemoryBounded(t *testing.T) {
	if testing.Short() || raceBuild {
		// ~7s plain; the bound it pins is single-threaded bookkeeping, which
		// TestFeedback_ConcurrentSameQueryMemory covers under -race.
		t.Skip("100,000 labels: skipped under -short and -race")
	}
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "many-labels"
	ids := le.seedN(ctx, vault, 10)
	var groups []FeedbackGroup
	for i := 0; i < 10000; i++ {
		ro := le.recall(ctx, vault, true)
		require.Len(t, ro.Activations, 10)
		g := FeedbackGroup{QueryID: ro.QueryID, Useful: true}
		for _, a := range ro.Activations {
			g.IDs = append(g.IDs, a.ID)
		}
		groups = append(groups, g)
		if len(groups) == 20 { // 200 ids per call, the limit
			require.NoError(t, le.eng.RecordFeedbackLabels(ctx, vault, groups, FeedbackChannelHandoff))
			groups = groups[:0]
		}
	}
	le.eng.labels.mu.Lock()
	require.Zero(t, len(le.eng.labels.vaults), "no in-memory state for accepted labels")
	le.eng.labels.mu.Unlock()
	for _, id := range ids {
		require.Equal(t, uint32(1), le.accessCount(vault, id))
	}
	require.Len(t, le.labels(vault), 100000)
	require.Equal(t, 10, countDayMarkers(le))
}

// TestFeedback_ClearVaultDropsLabelPartition: clearing (or deleting) a vault
// removes its in-memory label partition.
func TestFeedback_ClearVaultDropsLabelPartition(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "partition-clear"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	le.label(ctx, vault, resp.QueryID, true, resp.Activations[0].ID)
	le.label(ctx, vault, "01ARZ3NDEKTSV4RRFFQ69G5FAV", true, resp.Activations[0].ID) // a rejection too
	ws := le.store.ResolveVaultPrefix(vault)
	require.True(t, le.eng.labelPartitionExists(ws))
	require.NoError(t, le.eng.ClearVault(withMode("full"), vault))
	require.False(t, le.eng.labelPartitionExists(ws), "a cleared vault keeps no label state")
}
