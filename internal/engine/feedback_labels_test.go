package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Label pipeline (slice 2a) engine tests. Every vault name, token, session id
// and memory text below is invented (trail maintenance), never real content.

// labelEnv is an engine over a store it can close and reopen in the same
// directory, with a chosen auth secret — the restart seam for query ids.
type labelEnv struct {
	t      *testing.T
	dir    string
	secret []byte
	db     *pebble.DB
	store  *storage.PebbleStore
	eng    *Engine
}

func newLabelEnv(t *testing.T, secret []byte) *labelEnv {
	t.Helper()
	le := &labelEnv{t: t, dir: t.TempDir(), secret: secret}
	le.open()
	t.Cleanup(le.close)
	return le
}

func (le *labelEnv) open() {
	le.t.Helper()
	db, err := storage.OpenPebble(le.dir, storage.DefaultOptions())
	require.NoError(le.t, err)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	le.db, le.store = db, store
	le.eng = NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder, QueryIDSecret: le.secret})
}

func (le *labelEnv) close() {
	if le.eng == nil {
		return
	}
	le.eng.Stop()
	le.store.Close()
	le.eng, le.store, le.db = nil, nil, nil
}

func (le *labelEnv) reopen(secret []byte) {
	le.close()
	le.secret = secret
	le.open()
}

// caller is a full-mode MCP-like caller with a credential token and session.
func caller(token, session string) context.Context {
	return auth.WithCallerIdentity(withMode(auth.ModeFull), token, session)
}

var trailFixture = []struct{ concept, content string }{
	{"culvert spacing", "drainage culverts on the ridge trail are spaced every forty meters on steep grades"},
	{"waterbar angle", "waterbars on the ridge trail drainage are cut at thirty degrees to shed runoff"},
	{"bridge decking", "the creek footbridge decking was replaced with treated planks last autumn"},
	{"switchback repair", "switchback retaining walls on the ridge trail need annual drainage inspection"},
}

// seed writes the trail fixture into vault under ctx and returns ids in order.
func (le *labelEnv) seed(ctx context.Context, vault string) []string {
	le.t.Helper()
	var ids []string
	for _, f := range trailFixture {
		r, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: f.concept, Content: f.content})
		require.NoError(le.t, err)
		ids = append(ids, r.ID)
	}
	awaitFTS(le.t, le.eng)
	return ids
}

func (le *labelEnv) recall(ctx context.Context, vault string, readOnly bool) *mbp.ActivateResponse {
	le.t.Helper()
	resp, err := le.eng.Activate(ctx, &mbp.ActivateRequest{
		Vault: vault, Context: []string{"ridge trail drainage"}, MaxResults: 10, Threshold: 0.001, IncludeWeak: true, ReadOnly: readOnly,
	})
	require.NoError(le.t, err)
	return resp
}

func (le *labelEnv) label(ctx context.Context, vault, qid string, useful bool, ids ...string) {
	le.t.Helper()
	require.NoError(le.t, le.eng.RecordFeedbackLabels(ctx, vault, []FeedbackGroup{{QueryID: qid, IDs: ids, Useful: useful}}, ""))
}

func (le *labelEnv) labels(vault string) []storage.RecallLabelRow {
	le.t.Helper()
	var out []storage.RecallLabelRow
	require.NoError(le.t, le.store.ScanRecallLabels(context.Background(), le.store.ResolveVaultPrefix(vault), storage.RecallPurposeLabel,
		func(r storage.RecallLabelRow) error { out = append(out, r); return nil }))
	return out
}

func (le *labelEnv) rejected(vault, reason string) int64 {
	le.t.Helper()
	st, err := le.eng.LabelStats(context.Background(), vault)
	require.NoError(le.t, err)
	return st.Rejected[reason]
}

func (le *labelEnv) accessCount(vault, id string) uint32 {
	le.t.Helper()
	r, err := le.eng.Read(context.Background(), &mbp.ReadRequest{Vault: vault, ID: id, ReadOnly: true})
	require.NoError(le.t, err)
	return r.AccessCount
}

// snapshotKV returns every key and value in the store, concatenated with
// length framing, for byte-identical before/after comparison.
func (le *labelEnv) snapshotKV() []byte {
	le.t.Helper()
	le.eng.WaitWriteTimeIdle()
	le.store.DrainProvenance()
	le.store.FlushCounters() // a seed write's coalesced vault count must not land mid-comparison
	require.NoError(le.t, le.db.Flush())
	iter, err := le.db.NewIter(nil)
	require.NoError(le.t, err)
	defer iter.Close()
	var buf bytes.Buffer
	for iter.First(); iter.Valid(); iter.Next() {
		buf.WriteString("K")
		buf.Write(iter.Key())
		buf.WriteString("V")
		buf.Write(iter.Value())
	}
	return buf.Bytes()
}

var testSecret = []byte("label-pipeline-test-secret-0001")

// ---------------------------------------------------------------- ids ----

// TestRecall_QueryIDIsULIDOnZeroRowsAndFailedEventWrite: a normal recall's
// query id is a real ULID on the zero-row path and when the event write
// fails — never the boot counter `q-%016x`, which restarts at 1 on every
// boot and so is not a key.
func TestRecall_QueryIDIsULIDOnZeroRowsAndFailedEventWrite(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")

	empty, err := le.eng.Activate(ctx, &mbp.ActivateRequest{Vault: "zero-rows", Context: []string{"nothing stored here"}, MaxResults: 5, Threshold: 0.99})
	require.NoError(t, err)
	require.Empty(t, empty.Activations)
	_, perr := storage.ParseULID(empty.QueryID)
	require.NoError(t, perr, "zero-row recall query_id %q must be a ULID", empty.QueryID)

	le.seed(ctx, "failed-write")
	le.eng.recallEventWriteHook = func(context.Context, [8]byte, storage.ULID, *storage.RecallEvent) error {
		return errors.New("injected sink failure")
	}
	resp := le.recall(ctx, "failed-write", false)
	require.NotEmpty(t, resp.Activations)
	_, perr = storage.ParseULID(resp.QueryID)
	require.NoError(t, perr, "failed-write recall query_id %q must be a ULID", resp.QueryID)

	// Labelling either id is accepted uniformly and lands as unknown_event.
	le.label(ctx, "failed-write", resp.QueryID, true, resp.Activations[0].ID)
	require.Equal(t, int64(1), le.rejected("failed-write", labelRejectUnknownEvent))
	require.Empty(t, le.labels("failed-write"))
}

// TestRecall_ReadOnlyQueryIDSurvivesRestart: an ro1. id minted before a
// restart verifies after it (same persisted secret), and a normal id minted
// before a restart still joins its event after it.
func TestRecall_ReadOnlyQueryIDSurvivesRestart(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "restart"
	le.seed(ctx, vault)

	ro := le.recall(ctx, vault, true)
	require.True(t, strings.HasPrefix(ro.QueryID, "ro1."), "read-only id %q", ro.QueryID)
	normal := le.recall(ctx, vault, false)
	roMem, normMem := ro.Activations[0].ID, normal.Activations[1].ID

	le.reopen(testSecret)

	le.label(ctx, vault, ro.QueryID, true, roMem)
	le.label(ctx, vault, normal.QueryID, true, normMem)
	require.Zero(t, le.rejected(vault, labelRejectBadMAC))
	require.Zero(t, le.rejected(vault, labelRejectUnknownEvent))
	rows := le.labels(vault)
	require.Len(t, rows, 2)
	byMem := map[string]storage.RecallLabel{}
	for _, r := range rows {
		byMem[r.EngramID.String()] = r.Label
	}
	require.Equal(t, storage.LabelMembershipUnverified, byMem[roMem].Membership)
	require.Equal(t, storage.LabelMembershipVerified, byMem[normMem].Membership)
	require.Equal(t, uint16(2), byMem[normMem].Rank, "joined to the pre-restart event's entries")
}

// TestRecall_ReadOnlyQueryIDRejectedWithRotatedSecret: a rotated auth secret
// invalidates ro1. ids (bad_mac), uniformly.
func TestRecall_ReadOnlyQueryIDRejectedWithRotatedSecret(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "rotated"
	le.seed(ctx, vault)
	ro := le.recall(ctx, vault, true)

	le.reopen([]byte("a-different-secret-after-rotation"))
	le.label(ctx, vault, ro.QueryID, true, ro.Activations[0].ID)
	require.Equal(t, int64(1), le.rejected(vault, labelRejectBadMAC))
	require.Empty(t, le.labels(vault))
}

// TestRecall_ROIDFromOtherVaultFailsMAC: the MAC binds the vault.
func TestRecall_ROIDFromOtherVaultFailsMAC(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	le.seed(ctx, "vault-b")
	ids := le.seed(ctx, "vault-a")
	ro := le.recall(ctx, "vault-b", true)

	le.label(ctx, "vault-a", ro.QueryID, true, ids[0])
	require.Equal(t, int64(1), le.rejected("vault-a", labelRejectBadMAC))
	require.Empty(t, le.labels("vault-a"))
	require.Empty(t, le.labels("vault-b"))
}

// ------------------------------------------- membership / idempotency ----

// TestFeedback_IdNotInRecallRejectedSilently: a memory that exists but that
// the query did not return is rejected (not_in_recall); no row; uniform nil.
func TestFeedback_IdNotInRecallRejectedSilently(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "not-in-recall"
	le.seed(ctx, vault)
	outsider, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "kiln", Content: "pottery kiln firing schedule unrelated"})
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	resp := le.recall(ctx, vault, false)
	for _, a := range resp.Activations {
		require.NotEqual(t, outsider.ID, a.ID, "fixture: the outsider must not be returned")
	}
	before := testutil.ToFloat64(metrics.FeedbackRejectedTotal.WithLabelValues(labelRejectNotInRecall))
	le.label(ctx, vault, resp.QueryID, true, outsider.ID)
	require.Equal(t, before+1, testutil.ToFloat64(metrics.FeedbackRejectedTotal.WithLabelValues(labelRejectNotInRecall)))
	require.Equal(t, int64(1), le.rejected(vault, labelRejectNotInRecall))
	require.Empty(t, le.labels(vault))
	require.Zero(t, le.accessCount(vault, outsider.ID), "a rejected label is not a use")
}

// TestFeedback_UnknownEventRejectedSilently: a well-formed ULID that names no
// event is rejected (unknown_event).
func TestFeedback_UnknownEventRejectedSilently(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	ids := le.seed(ctx, "unknown-event")
	le.label(ctx, "unknown-event", storage.NewULID().String(), true, ids[0])
	require.Equal(t, int64(1), le.rejected("unknown-event", labelRejectUnknownEvent))
	require.Empty(t, le.labels("unknown-event"))
}

// TestFeedback_CrossVaultQueryIDRejected: an event from vault B posted to
// vault A is unknown_event — every lookup is keyed by the request's vault.
func TestFeedback_CrossVaultQueryIDRejected(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	le.seed(ctx, "cross-b")
	le.seed(ctx, "cross-a")
	respB := le.recall(ctx, "cross-b", false)
	le.label(ctx, "cross-a", respB.QueryID, true, respB.Activations[0].ID)
	require.Equal(t, int64(1), le.rejected("cross-a", labelRejectUnknownEvent))
	require.Empty(t, le.labels("cross-a"))
	require.Empty(t, le.labels("cross-b"))
}

// TestFeedback_IdempotentPerQueryIDMemoryCredential: 1,000 identical posts →
// one label row, count 1,000, exactly one access bump.
func TestFeedback_IdempotentPerQueryIDMemoryCredential(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "idempotent"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem := resp.Activations[0].ID
	for i := 0; i < 1000; i++ {
		le.label(ctx, vault, resp.QueryID, true, mem)
	}
	rows := le.labels(vault)
	require.Len(t, rows, 1)
	require.Equal(t, uint32(1000), rows[0].Label.Count)
	require.Equal(t, uint32(1), le.accessCount(vault, mem), "1,000 posts are one use")
}

// TestFeedback_PolarityLastWriteWins.
func TestFeedback_PolarityLastWriteWins(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "polarity"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem := resp.Activations[0].ID
	le.label(ctx, vault, resp.QueryID, true, mem)
	le.label(ctx, vault, resp.QueryID, false, mem)
	rows := le.labels(vault)
	require.Len(t, rows, 1)
	require.Equal(t, uint8(0), rows[0].Label.Polarity)
	require.Equal(t, uint32(2), rows[0].Label.Count)
	require.Equal(t, uint32(1), le.accessCount(vault, mem), "the first positive was one use; the flip adds none")

	// A first NEGATIVE label never bumps.
	other := resp.Activations[1].ID
	le.label(ctx, vault, resp.QueryID, false, other)
	require.Zero(t, le.accessCount(vault, other))
}

// TestFeedback_DifferentCredentialsAreDistinctLabels.
func TestFeedback_DifferentCredentialsAreDistinctLabels(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "distinct-creds"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem := resp.Activations[0].ID
	le.label(ctx, vault, resp.QueryID, true, mem)
	le.label(caller("tok-b", "sess-b"), vault, resp.QueryID, true, mem)
	le.label(caller("tok-b", "sess-c"), vault, resp.QueryID, true, mem) // same credential, other session
	rows := le.labels(vault)
	require.Len(t, rows, 2, "one row per labelling credential")
	counts := []uint32{rows[0].Label.Count, rows[1].Label.Count}
	require.ElementsMatch(t, []uint32{1, 2}, counts)
}

// TestFeedback_LegacyFormCappedPerDay: 1,000 legacy posts → one access bump
// on one UTC day; a new day allows one more.
func TestFeedback_LegacyFormCappedPerDay(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	day := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	le.eng.labelNow = func() time.Time { return day }
	ctx := caller("tok-a", "sess-a")
	ids := le.seed(ctx, "legacy-cap")
	for i := 0; i < 1000; i++ {
		require.NoError(t, le.eng.RecordFeedback(ctx, "legacy-cap", ids[0], true))
	}
	le.eng.waitFireAndForgetIdle()
	require.Equal(t, uint32(1), le.accessCount("legacy-cap", ids[0]))
	day = day.Add(24 * time.Hour)
	require.NoError(t, le.eng.RecordFeedback(ctx, "legacy-cap", ids[0], true))
	le.eng.waitFireAndForgetIdle()
	require.Equal(t, uint32(2), le.accessCount("legacy-cap", ids[0]))
	require.Empty(t, le.labels("legacy-cap"), "the legacy form records no label")
}

// TestFeedback_StormCounterCrossesThresholds: the storm metric fires once
// at 10 and once at 100 posts of one (credential, memory, day).
func TestFeedback_StormCounterCrossesThresholds(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-storm", "sess-storm")
	const vault = "storm"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem := resp.Activations[0].ID
	at10 := testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("10"))
	at100 := testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("100"))
	for i := 0; i < 9; i++ {
		le.label(ctx, vault, resp.QueryID, true, mem)
	}
	require.Equal(t, at10, testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("10")))
	le.label(ctx, vault, resp.QueryID, true, mem)
	require.Equal(t, at10+1, testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("10")))
	for i := 0; i < 150; i++ {
		le.label(ctx, vault, resp.QueryID, true, mem)
	}
	require.Equal(t, at10+1, testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("10")), "fires once per crossing")
	require.Equal(t, at100+1, testutil.ToFloat64(metrics.FeedbackStormTotal.WithLabelValues("100")))
}

// TestFeedback_BatchGroupsMixedVerifiedAndUnverified: one batch call carrying
// a normal-id group and a read-only-id group records one verified and one
// unverified label; a duplicate pair within the call is deduped.
func TestFeedback_BatchGroupsMixedVerifiedAndUnverified(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "batch"
	le.seed(ctx, vault)
	normal := le.recall(ctx, vault, false)
	ro := le.recall(ctx, vault, true)
	m1, m2 := normal.Activations[0].ID, ro.Activations[1].ID
	err := le.eng.RecordFeedbackLabels(caller("tok-drain", "sess-drain"), vault, []FeedbackGroup{
		{QueryID: normal.QueryID, IDs: []string{m1, m1}, Useful: true},
		{QueryID: ro.QueryID, IDs: []string{m2}, Useful: false},
	}, FeedbackChannelHandoff)
	require.NoError(t, err)
	rows := le.labels(vault)
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Equal(t, storage.LabelChannelHandoff, r.Label.Channel)
		require.Equal(t, uint32(1), r.Label.Count)
		switch r.EngramID.String() {
		case m1:
			require.Equal(t, storage.LabelMembershipVerified, r.Label.Membership)
		case m2:
			require.Equal(t, storage.LabelMembershipUnverified, r.Label.Membership)
			require.Equal(t, uint16(0), r.Label.Rank, "read-only rank is unknown")
			require.Equal(t, storage.RecallSourceUnknown, r.Label.Source)
			require.Equal(t, uint8(0), r.Label.Polarity)
		default:
			t.Fatalf("unexpected label for %s", r.EngramID)
		}
	}
	h, err := le.store.GetRecallROHeader(context.Background(), le.store.ResolveVaultPrefix(vault), mustParseULID(t, ro.QueryID[4:30]), storage.RecallPurposeLabel)
	require.NoError(t, err)
	require.NotNil(t, h, "the first label for a read-only id writes the header")
	require.Equal(t, uint8(1), h.Unverified)
}

// ---------------------------------------------------- recorded fields ----

// TestFeedback_RecordsRankSizeSourceFromEvent: rank, set size and source come
// from the event's entries; recall credential/session hashes from the event;
// the labeller's session hash is its own.
func TestFeedback_RecordsRankSizeSourceFromEvent(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	recaller := caller("tok-recall", "sess-recall")
	const vault = "fields"
	le.seed(recaller, vault)
	resp := le.recall(recaller, vault, false)
	require.GreaterOrEqual(t, len(resp.Activations), 3)
	mem := resp.Activations[2].ID
	le.label(caller("tok-other", "sess-other"), vault, resp.QueryID, true, mem)

	ev, err := le.store.GetRecallEvent(context.Background(), le.store.ResolveVaultPrefix(vault), mustParseULID(t, resp.QueryID), storage.RecallPurposeCalibration)
	require.NoError(t, err)
	require.Equal(t, storage.RecallEventVersionLabels, ev.Version)
	rows := le.labels(vault)
	require.Len(t, rows, 1)
	l := rows[0].Label
	require.Equal(t, uint16(3), l.Rank)
	require.Equal(t, uint16(len(resp.Activations)), l.SetSize)
	require.Equal(t, ev.Entries[2].Source, l.Source)
	k := le.eng.qidKey
	rc, rs := k.CredHash("tok-recall"), k.SessionHash("sess-recall")
	ls := k.SessionHash("sess-other")
	require.Equal(t, rc[:], l.RecallCred)
	require.Equal(t, rs[:], l.RecallSess)
	require.Equal(t, ls[:], l.LabelSess)
	lcOther := k.CredHash("tok-other")
	require.Equal(t, lcOther, rows[0].LabelCred, "the row is keyed by the LABELLING credential")
	require.Equal(t, storage.LabelChannelDirect, l.Channel)
	require.NotZero(t, l.FirstAt)
	require.Equal(t, l.FirstAt, l.LastAt)
	// Score components ride the event.
	require.NotZero(t, ev.Entries[0].CM)
	require.NotZero(t, ev.Entries[0].Abs)
}

// TestFeedback_SelfLabellingFromProvenanceSession: a memory written in the
// recall's session is self=1; one written in another session is self=2; one
// written with no session (a non-MCP transport) is self=0.
func TestFeedback_SelfLabellingFromProvenanceSession(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	const vault = "self-label"
	mine, err := le.eng.Write(caller("tok-a", "sess-mine"), &mbp.WriteRequest{Vault: vault, Concept: "culvert", Content: "ridge trail drainage culvert inventory"})
	require.NoError(t, err)
	theirs, err := le.eng.Write(caller("tok-a", "sess-theirs"), &mbp.WriteRequest{Vault: vault, Concept: "waterbar", Content: "ridge trail drainage waterbar inventory"})
	require.NoError(t, err)
	anon, err := le.eng.Write(withMode(auth.ModeFull), &mbp.WriteRequest{Vault: vault, Concept: "switchback", Content: "ridge trail drainage switchback inventory"})
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	le.store.DrainProvenance()

	ctx := caller("tok-a", "sess-mine")
	resp := le.recall(ctx, vault, false)
	require.Len(t, resp.Activations, 3)
	le.label(ctx, vault, resp.QueryID, true, mine.ID, theirs.ID, anon.ID)
	got := map[string]uint8{}
	for _, r := range le.labels(vault) {
		got[r.EngramID.String()] = r.Label.Self
	}
	require.Equal(t, storage.LabelSelfSame, got[mine.ID])
	require.Equal(t, storage.LabelSelfOther, got[theirs.ID])
	require.Equal(t, storage.LabelSelfUnknown, got[anon.ID])
}

// TestFeedback_SupersededFlag: a memory evolved (superseded) between the
// recall and the label is recorded sup=1; a live one sup=0.
func TestFeedback_SupersededFlag(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "superseded"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	old, live := resp.Activations[0].ID, resp.Activations[1].ID
	_, err := le.eng.Evolve(ctx, vault, old, "drainage culverts are now spaced every thirty meters", "re-surveyed", nil, "")
	require.NoError(t, err)
	le.label(ctx, vault, resp.QueryID, true, old, live)
	got := map[string]uint8{}
	for _, r := range le.labels(vault) {
		got[r.EngramID.String()] = r.Label.Superseded
	}
	require.Equal(t, uint8(1), got[old])
	require.Equal(t, uint8(0), got[live])
}

// TestRecallEvent_EntrySourceFlags: each source flag bit is set from its
// mechanism (unit), and an Activate-written event carries flags equal to
// the returned rows' (integration: a tag-filter-admitted row).
func TestRecallEvent_EntrySourceFlags(t *testing.T) {
	sub := storage.NewULID()
	cases := []struct {
		name string
		s    activation.ScoredEngram
		want uint8
	}{
		{"injected", activation.ScoredEngram{Admission: activation.AdmissionInjected}, 0},
		{"scored", activation.ScoredEngram{Admission: activation.AdmissionScored}, 1},
		{"tag-pool", activation.ScoredEngram{Admission: activation.AdmissionTagFilter}, 2},
		{"version-head", activation.ScoredEngram{Admission: activation.AdmissionScored, SubstitutedFor: sub}, 1 | storage.RecallSourceVersionHead},
		{"entity-boost", activation.ScoredEngram{Components: activation.ScoreComponents{EntityBoost: 0.1}}, storage.RecallSourceEntityBoost},
		{"pas", activation.ScoredEngram{Components: activation.ScoreComponents{TransitionBoost: 0.2}}, storage.RecallSourcePAS},
		{"hebbian", activation.ScoredEngram{Components: activation.ScoreComponents{HebbianBoost: 0.3}}, storage.RecallSourceHebbian},
	}
	for _, c := range cases {
		require.Equal(t, c.want, recallSourceFlags(&c.s), c.name)
	}

	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "source-flags"
	_, err := le.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "tool shed", Content: "the tool shed padlock code changed", Tags: []string{"shed"}})
	require.NoError(t, err)
	awaitFTS(t, le.eng)
	resp, err := le.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"unrelated mountain weather"}, MaxResults: 5,
		Filters: []mbp.Filter{{Field: "tags_all", Op: "all", Value: []string{"shed"}}}})
	require.NoError(t, err)
	require.Len(t, resp.Activations, 1)
	ev, err := le.store.GetRecallEvent(context.Background(), le.store.ResolveVaultPrefix(vault), mustParseULID(t, resp.QueryID), storage.RecallPurposeCalibration)
	require.NoError(t, err)
	require.Equal(t, uint8(activation.AdmissionTagFilter), ev.Entries[0].Source&storage.RecallSourceAdmissionMask, "tag-pool row records admission 2")
}

// --------------------------------------------------------- credentials ----

// TestFeedback_ObserveRefused: the engine refuses observe credentials for
// both forms (MCP refuses them at dispatch too — see the mcp package).
func TestFeedback_ObserveRefused(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ids := le.seed(caller("tok-a", "sess-a"), "observe")
	resp := le.recall(caller("tok-a", "sess-a"), "observe", false)
	obs := auth.WithCallerIdentity(withMode(auth.ModeObserve), "tok-obs", "s")
	err := le.eng.RecordFeedbackLabels(obs, "observe", []FeedbackGroup{{QueryID: resp.QueryID, IDs: ids[:1], Useful: true}}, "")
	require.ErrorIs(t, err, ErrObserveForbidden)
	require.ErrorIs(t, le.eng.RecordFeedback(obs, "observe", ids[0], true), ErrObserveForbidden)
	require.Empty(t, le.labels("observe"))
}

// TestFeedback_AppendAllowed: append credentials may label (SEC-15
// amendment) through both forms.
func TestFeedback_AppendAllowed(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	le.seed(caller("tok-a", "sess-a"), "append")
	resp := le.recall(caller("tok-a", "sess-a"), "append", false)
	ap := auth.WithCallerIdentity(withMode(auth.ModeAppend), "tok-app", "s")
	require.NoError(t, le.eng.RecordFeedbackLabels(ap, "append", []FeedbackGroup{{QueryID: resp.QueryID, IDs: []string{resp.Activations[0].ID}, Useful: true}}, ""))
	require.NoError(t, le.eng.RecordFeedback(ap, "append", resp.Activations[1].ID, true))
	require.Len(t, le.labels("append"), 1)
}

// -------------------------------------------------- read-only is pure ----

// TestLabels_ReadOnlyRecallWritesNothing: a full key-and-value iteration of
// the store before and after a read-only recall is byte-identical — the ro1.
// id is minted, not stored.
func TestLabels_ReadOnlyRecallWritesNothing(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	le.seed(ctx, "ro-recall")
	before := le.snapshotKV()
	for i := 0; i < 3; i++ {
		resp := le.recall(ctx, "ro-recall", true)
		require.True(t, strings.HasPrefix(resp.QueryID, "ro1."))
	}
	require.True(t, bytes.Equal(before, le.snapshotKV()), "a read-only recall must write nothing")
	// Observe credentials are read-only too.
	le.recall(auth.WithCallerIdentity(withMode(auth.ModeObserve), "tok-obs", "s"), "ro-recall", false)
	require.True(t, bytes.Equal(before, le.snapshotKV()), "an observe recall must write nothing")
}

// TestLabels_ReadOnlyReadWritesNothing: likewise for a read-only read.
func TestLabels_ReadOnlyReadWritesNothing(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	ids := le.seed(ctx, "ro-read")
	before := le.snapshotKV()
	_, err := le.eng.Read(ctx, &mbp.ReadRequest{Vault: "ro-read", ID: ids[0], ReadOnly: true})
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, le.snapshotKV()), "a read-only read must write nothing")
}

// TestLabels_ROHeaderWrittenOnlyByFeedback: the read-only header exists only
// after the first label for that id, never after the recall.
func TestLabels_ROHeaderWrittenOnlyByFeedback(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "ro-header"
	le.seed(ctx, vault)
	ro := le.recall(ctx, vault, true)
	ws := le.store.ResolveVaultPrefix(vault)
	eid := mustParseULID(t, ro.QueryID[4:30])
	h, err := le.store.GetRecallROHeader(context.Background(), ws, eid, storage.RecallPurposeLabel)
	require.NoError(t, err)
	require.Nil(t, h, "the recall wrote no header")
	le.label(ctx, vault, ro.QueryID, true, ro.Activations[0].ID)
	h, err = le.store.GetRecallROHeader(context.Background(), ws, eid, storage.RecallPurposeLabel)
	require.NoError(t, err)
	require.NotNil(t, h)
	rc := le.eng.qidKey.CredHash("tok-a")
	require.Equal(t, rc[:], h.RecallCred)
	require.Equal(t, int64(ulidTimeMs(eid)), h.At)
}

// ------------------------------------------------ no ranking change ----

// TestLabels_RecallOrderUnchanged: labels change nothing in ranking — the
// ids, scores and order of a read-only recall are identical before and after
// heavy labelling (positives on the last row, negatives on the first).
func TestLabels_RecallOrderUnchanged(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "order-unchanged"
	le.seed(ctx, vault)
	before := le.recall(ctx, vault, true)
	normal := le.recall(ctx, vault, false)
	n := len(normal.Activations)
	for i := 0; i < 50; i++ {
		le.label(caller("tok-a", "s"), vault, normal.QueryID, true, normal.Activations[n-1].ID)
		le.label(caller("tok-b", "s"), vault, normal.QueryID, false, normal.Activations[0].ID)
		le.label(caller("tok-c", "s"), vault, before.QueryID, true, before.Activations[n-1].ID)
	}
	// The access bump of the first positive label is the one recorded effect;
	// under ACT-R it does not reach ranking (COG-7), which this pins too.
	after := le.recall(ctx, vault, true)
	require.Equal(t, len(before.Activations), len(after.Activations))
	for i := range before.Activations {
		require.Equal(t, before.Activations[i].ID, after.Activations[i].ID, "row %d", i)
		require.Equal(t, before.Activations[i].Score, after.Activations[i].Score, "row %d", i)
	}
}

// TestFeedback_ConcurrentSameQueryMemory (-race): simultaneous labels on the
// same (query, memory) from one credential give one row, the exact count,
// and exactly one access bump.
func TestFeedback_ConcurrentSameQueryMemory(t *testing.T) {
	le := newLabelEnv(t, testSecret)
	ctx := caller("tok-a", "sess-a")
	const vault = "concurrent"
	le.seed(ctx, vault)
	resp := le.recall(ctx, vault, false)
	mem := resp.Activations[0].ID
	const workers, per = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := le.eng.RecordFeedbackLabels(ctx, vault, []FeedbackGroup{{QueryID: resp.QueryID, IDs: []string{mem}, Useful: true}}, ""); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	rows := le.labels(vault)
	require.Len(t, rows, 1)
	require.Equal(t, uint32(workers*per), rows[0].Label.Count)
	require.Equal(t, uint32(1), le.accessCount(vault, mem))
}
