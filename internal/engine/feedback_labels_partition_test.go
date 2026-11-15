package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Second security review: the bounds on the label bookkeeping must be
// PARTITIONED — per existing vault, and per credential inside it — so a
// principal that fills its own bound affects only itself.

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

func dayFirstFor(le *labelEnv, vault, mem string, lc [8]byte) int {
	n := 0
	for _, r := range le.labels(vault) {
		if r.EngramID.String() == mem && r.LabelCred == lc && r.Label.DayFirst == 1 {
			n++
		}
	}
	return n
}

// TestFeedback_PartitionFullAffectsOnlyThatPrincipal: credential A fills its
// own per-day bound in vault V; credential B in V and credential A in vault C
// are unaffected (first positive label still bumps exactly once and still
// reports first-of-day), and A — at its bound — gets no further bump, and
// never a second bump for a memory it already bumped today.
func TestFeedback_PartitionFullAffectsOnlyThatPrincipal(t *testing.T) {
	old := labelPerCredentialDayMax
	labelPerCredentialDayMax = 4
	t.Cleanup(func() { labelPerCredentialDayMax = old })

	le := newLabelEnv(t, testSecret)
	a, b := caller("tok-a", "sess-a"), caller("tok-b", "sess-b")
	const vault, vaultC = "partition-v", "partition-c"
	le.seedN(a, vault, 6)
	le.seedN(a, vaultC, 2)

	resp := le.recall(a, vault, false)
	require.GreaterOrEqual(t, len(resp.Activations), 6)
	ids := make([]string, len(resp.Activations))
	for i, x := range resp.Activations {
		ids[i] = x.ID
	}
	// A fills its partition with 4 memories, then one more.
	for _, m := range ids[:5] {
		le.label(a, vault, resp.QueryID, true, m)
	}
	require.Equal(t, uint32(1), le.accessCount(vault, ids[0]))
	require.Zero(t, le.accessCount(vault, ids[4]), "at its own bound, A gets no further bump (fail closed for A only)")

	// A cannot obtain a second bump for a memory it already bumped today,
	// through a fresh query id.
	le.label(a, vault, le.recall(a, vault, false).QueryID, true, ids[0])
	require.Equal(t, uint32(1), le.accessCount(vault, ids[0]))

	// B, same vault: unaffected.
	lcB := le.eng.qidKey.CredHash("tok-b")
	le.label(b, vault, resp.QueryID, true, ids[5])
	require.Equal(t, uint32(1), le.accessCount(vault, ids[5]), "B's first positive label still bumps once")
	require.Equal(t, 1, dayFirstFor(le, vault, ids[5], lcB), "and is still first-of-day")
	le.label(b, vault, le.recall(b, vault, false).QueryID, true, ids[5])
	require.Equal(t, uint32(1), le.accessCount(vault, ids[5]), "once, not twice")

	// A, vault C: unaffected.
	lcA := le.eng.qidKey.CredHash("tok-a")
	rc := le.recall(a, vaultC, false)
	le.label(a, vaultC, rc.QueryID, true, rc.Activations[0].ID)
	require.Equal(t, uint32(1), le.accessCount(vaultC, rc.Activations[0].ID))
	require.Equal(t, 1, dayFirstFor(le, vaultC, rc.Activations[0].ID, lcA))
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
