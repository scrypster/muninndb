package engine

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Security review findings on the label pipeline (both RED before the fix).

// TestFeedback_ReadOnlyLabelsCappedPerCredentialDay: read-only recalls are
// cheap and write nothing, so each one mints a fresh ro1. id. 1,000 of them
// labelling the same memory must yield at most ONE access bump per
// credential per UTC day — no bump path is uncapped.
func TestFeedback_ReadOnlyLabelsCappedPerCredentialDay(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "ro-cap"
	le.seed(ctx, vault)
	var mem string
	for i := 0; i < 1000; i++ {
		ro := le.recall(ctx, vault, true)
		if mem == "" {
			mem = ro.Activations[0].ID
		}
		le.label(ctx, vault, ro.QueryID, true, mem)
	}
	require.LessOrEqual(t, le.accessCount(vault, mem), uint32(1), "1,000 fresh read-only ids must not be 1,000 uses")
	// Each id is a distinct label row (it is data), but exactly one is the
	// credential's first for this memory today — what a use count may read.
	dayFirst := 0
	for _, r := range le.labels(vault) {
		if r.EngramID.String() == mem && r.Label.DayFirst == 1 {
			dayFirst++
		}
	}
	require.Equal(t, 1, dayFirst)

	// The same cap holds across normal query ids.
	other := le.recall(ctx, vault, false).Activations[1].ID
	for i := 0; i < 50; i++ {
		le.label(ctx, vault, le.recall(ctx, vault, false).QueryID, true, other)
	}
	require.LessOrEqual(t, le.accessCount(vault, other), uint32(1))
}

// TestFeedback_ROLabelOutsideMintedSetRejected: a read-only id authorises
// labels only for the ids that recall returned. A memory outside that set is
// rejected (not_in_recall) exactly as for a normal id — no row, no bump.
func TestFeedback_ROLabelOutsideMintedSetRejected(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "ro-outside"
	le.seed(ctx, vault)
	outsider, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "kiln", Content: "pottery kiln firing schedule unrelated"})
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	ro := le.recall(ctx, vault, true)
	for _, a := range ro.Activations {
		require.NotEqual(t, outsider.ID, a.ID)
	}
	le.label(ctx, vault, ro.QueryID, true, outsider.ID)
	require.Empty(t, le.labels(vault), "a label outside the minted set is not recorded")
	require.Equal(t, int64(1), le.rejected(vault, labelRejectNotInRecall))
	require.Zero(t, le.accessCount(vault, outsider.ID))
}

// TestFeedback_RejectedStatsBoundedForBogusVaults: 10,000 distinct vault
// names that do not exist allocate nothing in the rejection tally.
func TestFeedback_RejectedStatsBoundedForBogusVaults(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	for i := 0; i < 10000; i++ {
		le.label(ctx, fmt.Sprintf("bogus-%05d", i), storage.NewULID().String(), true, storage.NewULID().String())
	}
	le.eng.labels.mu.Lock()
	n := len(le.eng.labels.vaults)
	le.eng.labels.mu.Unlock()
	require.Zero(t, n, "no state for vaults that do not exist")
}

// TestFeedback_LegacyCapIgnoresUnknownIDs: the legacy form's per-day cap map
// is keyed partly by the caller's engram id; ids that name no memory must
// allocate nothing.
func TestFeedback_LegacyCapIgnoresUnknownIDs(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	le.seed(ctx, "legacy-bound")
	for i := 0; i < 10000; i++ {
		require.NoError(t, le.eng.RecordFeedback(ctx, "legacy-bound", storage.NewULID().String(), true))
	}
	le.eng.labels.mu.Lock()
	n := 0
	for _, vs := range le.eng.labels.vaults {
		for _, cd := range vs.creds {
			n += len(cd.mems)
		}
	}
	le.eng.labels.mu.Unlock()
	require.Zero(t, n)
}
