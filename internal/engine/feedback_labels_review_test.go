package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Adversarial review of slice 2a, items 2, 3, 5, 7 and 9.

func (le *labelEnv) labelOutcomes(ctx context.Context, vault, qid string, useful bool, ids ...string) []FeedbackOutcome {
	le.t.Helper()
	out, err := le.eng.RecordFeedbackLabelsDetailed(ctx, vault, []FeedbackGroup{{QueryID: qid, IDs: ids, Useful: useful}}, "", FeedbackOptions{})
	require.NoError(le.t, err)
	return out
}

func labelFor(le *labelEnv, vault, mem string) *storage.RecallLabel {
	for _, r := range le.labels(vault) {
		if r.EngramID.String() == mem {
			l := r.Label
			return &l
		}
	}
	return nil
}

// TestFeedback_NotLiveMemoriesNeverBumped (item 2): a memory that is
// soft-deleted, superseded (has a successor, by evolve or by a supersedes
// link) or expired when labelled is recorded with sup=1 and is NOT bumped —
// labelled and legacy forms alike.
func TestFeedback_NotLiveMemoriesNeverBumped(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "not-live"
	le.seedN(ctx, vault, 6)
	resp := le.recall(ctx, vault, false)
	require.GreaterOrEqual(t, len(resp.Activations), 5)
	id := func(i int) string { return resp.Activations[i].ID }

	_, err := le.eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id(0)}) // soft delete
	require.NoError(t, err)
	_, err = le.eng.Evolve(ctx, vault, id(1), "marker post replaced by a cairn", "re-surveyed", nil, "")
	require.NoError(t, err)
	succ, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the marker post was moved uphill"})
	require.NoError(t, err)
	_, err = le.eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: succ.ID, TargetID: id(2), RelType: uint16(storage.RelSupersedes)})
	require.NoError(t, err)
	past := time.Now()
	_, err = le.eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id(3), NotTrueSince: &past}) // expired
	require.NoError(t, err)

	for i, state := range []string{"soft-deleted", "evolved", "superseded-by-link", "expired"} {
		le.label(ctx, vault, resp.QueryID, true, id(i))
		require.Zero(t, le.accessCount(vault, id(i)), "%s memory must not be bumped", state)
		l := labelFor(le, vault, id(i))
		require.NotNil(t, l, "%s: the label is still recorded", state)
		require.Equal(t, uint8(1), l.Superseded, "%s: recorded as not live", state)
		require.NoError(t, le.eng.RecordFeedback(caller("tok-b", "s"), vault, id(i), true))
		require.Zero(t, le.accessCount(vault, id(i)), "%s: legacy form must not bump either", state)
	}
	le.label(ctx, vault, resp.QueryID, true, id(4))
	require.Equal(t, uint32(1), le.accessCount(vault, id(4)), "control: a live memory is bumped")
}

// TestFeedback_HardDeletedMemoryConsumesNoBump (item 3): a verified member
// hard-deleted before the label is recorded (sup=1), and its day marker does
// not record a granted bump — removing the existence term fails this.
func TestFeedback_HardDeletedMemoryConsumesNoBump(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "hard-deleted"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	gone := resp.Activations[0].ID
	_, err := le.eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: gone, Hard: true})
	require.NoError(t, err)
	le.label(ctx, vault, resp.QueryID, true, gone)
	require.Equal(t, uint8(1), labelFor(le, vault, gone).Superseded)
	mk, err := le.store.GetRecallDayMarker(context.Background(), le.store.ResolveVaultPrefix(vault), unixDay(time.Now()),
		mustParseULID(t, gone), le.eng.qidKey.CredHash("tok-a"), storage.RecallPurposeLabel)
	require.NoError(t, err)
	require.NotNil(t, mk)
	require.Zero(t, mk.BumpedAt, "a deleted memory never consumes the day's bump")
}

// TestFeedback_SubstitutedOldIDPointsAtHead (item 7): labelling the old id a
// row was substituted for is rejected, and the outcome names the head.
func TestFeedback_SubstitutedOldIDPointsAtHead(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "substituted"
	old, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the beehive entrance reducer stays on until april"})
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	head, err := le.eng.Evolve(ctx, vault, old.ID, "the hive entrance reducer comes off in march", "earlier spring", nil, "")
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	resp, err := le.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"beehive entrance reducer april"}, MaxResults: 5, Threshold: 0.001, IncludeWeak: true})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Activations)
	require.Equal(t, head.String(), resp.Activations[0].ID)
	out := le.labelOutcomes(ctx, vault, resp.QueryID, true, old.ID)
	require.Len(t, out, 1)
	require.Equal(t, FeedbackOutcomeRejected, out[0].Outcome)
	require.Equal(t, FeedbackReasonSubstituted, out[0].Reason)
	require.Equal(t, head.String(), out[0].HeadID)
}

// TestFeedback_ReadOnlyRowBeyondTagCapRejectedExplicitly (item 9): a
// read-only id tags at most 100 rows; a label for row 101+ (reachable only on
// transports that allow larger recalls) is rejected with an explicit reason,
// not as an ordinary miss.
func TestFeedback_ReadOnlyRowBeyondTagCapRejectedExplicitly(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "beyond-cap"
	le.seedN(ctx, vault, 105)
	ro, err := le.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"ridge trail drainage marker"}, MaxResults: 150, Threshold: 0.001, IncludeWeak: true, ReadOnly: true})
	require.NoError(t, err)
	require.Greater(t, len(ro.Activations), 100)
	out := le.labelOutcomes(ctx, vault, ro.QueryID, true, ro.Activations[100].ID, ro.Activations[99].ID)
	require.Equal(t, FeedbackReasonUntaggedRow, out[0].Reason)
	require.Equal(t, FeedbackOutcomeRecorded, out[1].Outcome)
}

// TestLabelStats_ReadsTheWindowNotTheHistory (item 5): status label counts
// come from the trailing window's day markers, not a scan of every label row
// the vault has ever stored.
func TestLabelStats_ReadsTheWindowNotTheHistory(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "stats-window"
	le.seed(ctx, vault)
	ws := le.store.ResolveVaultPrefix(vault)
	// 50 historical label rows (no markers in the window): not counted.
	for i := 0; i < 50; i++ {
		_, _, err := le.store.UpsertRecallLabel(context.Background(), ws, storage.NewULIDWithTime(time.Now().AddDate(0, 0, -30)), storage.NewULID(), [8]byte{1},
			storage.RecallLabel{Polarity: 1, LastAt: time.Now().UnixMilli(), Membership: storage.LabelMembershipVerified}, nil)
		require.NoError(t, err)
	}
	resp := le.recall(ctx, vault, false)
	le.label(ctx, vault, resp.QueryID, true, resp.Activations[0].ID)
	st, err := le.eng.LabelStats(context.Background(), vault)
	require.NoError(t, err)
	require.Equal(t, 1, st.Verified)
	require.Equal(t, 1, st.Positive)
}
