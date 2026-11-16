package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Labels on the COG-35 `associated` channel. A label's set size is the number
// of MATCHED rows the recall served in `memories` — independent of
// include_associated — and a label on an associated item says so (its own
// source value, rank 0: it has no matched-row rank), for normal and read-only
// query ids alike. Fixtures are invented (trail maintenance).

const assocTestSecret = "trail-crew-test-secret-0123456789"

// labelFor posts one useful label and returns the outcome and the stored row.
func assocLabelFor(t *testing.T, c *cfEnv, vault, qid string, id storage.ULID) (string, storage.RecallLabel) {
	t.Helper()
	ctx := context.Background()
	out, err := c.eng.RecordFeedbackLabelsDetailed(ctx, vault, []FeedbackGroup{{QueryID: qid, IDs: []string{id.String()}, Useful: true}},
		FeedbackChannelDirect, FeedbackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("outcomes: %+v", out)
	}
	_, p := auth.ClassifyQueryID(qid)
	ev := mustULID(t, p.ULID)
	var got storage.RecallLabel
	found := false
	if err := c.store.ScanRecallLabels(ctx, c.store.ResolveVaultPrefix(vault), storage.RecallPurposeLabel, func(r storage.RecallLabelRow) error {
		if r.EventID == ev && r.EngramID == id {
			got, found = r.Label, true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out[0].Outcome == FeedbackOutcomeRecorded && !found {
		t.Fatalf("label recorded but no row stored")
	}
	return out[0].Outcome, got
}

func labelIsAssociated(l storage.RecallLabel) bool {
	return l.Source != storage.RecallSourceUnknown && l.Source&storage.RecallSourceAssociated != 0
}

// TestAssociatedLabel_SetSizeCountsMatchedRowsOnly — the same query with the
// same ten `memories` records a matched row's label with the same set size
// whether the associated block was on or off. RED before: k=11 on, k=10 off.
func TestAssociatedLabel_SetSizeCountsMatchedRowsOnly(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-setsize"
	f := cfBuild(c, vault)
	on := c.activate(vault, cfQuery, 10, false, nil)
	off := c.activate(vault, cfQuery, 10, false, func(r *mbp.ActivateRequest) { r.ExcludeAssociated = true })
	if len(on.Associated) == 0 || len(on.Activations) != len(off.Activations) {
		t.Fatalf("premise: same memories (%d/%d), associated block on (%d items)", len(on.Activations), len(off.Activations), len(on.Associated))
	}
	_ = f
	top := mustULID(t, on.Activations[0].ID)
	_, lOn := assocLabelFor(t, c, vault, on.QueryID, top)
	_, lOff := assocLabelFor(t, c, vault, off.QueryID, top)
	want := uint16(len(on.Activations))
	if lOn.SetSize != want || lOff.SetSize != want {
		t.Errorf("set size must be the matched rows served (%d): associated on k=%d, off k=%d", want, lOn.SetSize, lOff.SetSize)
	}
	if lOn.Rank != 1 || labelIsAssociated(lOn) {
		t.Errorf("matched row label: rank=%d source=%08b", lOn.Rank, lOn.Source)
	}
}

// TestAssociatedLabel_NormalIDMarksTheAssociatedItem — a label on an
// associated item via a normal query id carries the associated source and no
// matched-row rank. RED before: rank 11 (after the ten matched rows).
func TestAssociatedLabel_NormalIDMarksTheAssociatedItem(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-normal-kind"
	f := cfBuild(c, vault)
	resp := c.activate(vault, cfQuery, 10, false, nil)
	if assocIndex(resp.Associated, f.injectee) < 0 {
		t.Fatalf("premise: injectee in associated: %+v", resp.Associated)
	}
	outcome, l := assocLabelFor(t, c, vault, resp.QueryID, f.injectee)
	if outcome != FeedbackOutcomeRecorded {
		t.Fatalf("label for a served associated item: %s", outcome)
	}
	if !labelIsAssociated(l) || l.Rank != 0 || l.SetSize != uint16(len(resp.Activations)) {
		t.Errorf("associated item label: rank=%d k=%d source=%08b, want rank 0, k=%d, associated source",
			l.Rank, l.SetSize, l.Source, len(resp.Activations))
	}
}

// TestAssociatedLabel_ReadOnlyIDMarksTheAssociatedItem — the same for a
// read-only id: the id itself must distinguish an associated item from a
// matched row. RED before: rank 11, k=11, source unknown — indistinguishable
// from a matched row at rank 11.
func TestAssociatedLabel_ReadOnlyIDMarksTheAssociatedItem(t *testing.T) {
	c := newCFEnv(t)
	c.eng.qidKey = auth.NewQueryIDKey([]byte(assocTestSecret))
	const vault = "trail-assoc-ro-kind"
	f := cfBuild(c, vault)
	resp := c.activate(vault, cfQuery, 10, true, nil)
	if assocIndex(resp.Associated, f.injectee) < 0 {
		t.Fatalf("premise: injectee in associated: %+v", resp.Associated)
	}
	outcome, l := assocLabelFor(t, c, vault, resp.QueryID, f.injectee)
	if outcome != FeedbackOutcomeRecorded {
		t.Fatalf("label for a served associated item: %s", outcome)
	}
	k := uint16(len(resp.Activations))
	if !labelIsAssociated(l) || l.Rank != 0 || l.SetSize != k {
		t.Errorf("read-only associated item label: rank=%d k=%d source=%08b, want rank 0, k=%d, associated source", l.Rank, l.SetSize, l.Source, k)
	}
	_, m := assocLabelFor(t, c, vault, resp.QueryID, mustULID(t, resp.Activations[0].ID))
	if labelIsAssociated(m) || m.Rank != 1 || m.SetSize != k {
		t.Errorf("read-only matched row label: rank=%d k=%d source=%08b, want rank 1, k=%d", m.Rank, m.SetSize, m.Source, k)
	}
}

// TestAssociatedLabel_LegacyReadOnlyIDStaysAcceptedAndHonest — a read-only id
// in the layout issued before the associated section existed (ro1: one tag
// list, matched rows then associated items) is still accepted. It cannot say
// where the matched rows end, so: a tag in the first max(1, rows-3) positions
// is a matched row with that rank; a later tag (which may be an associated
// item) gets rank 0 (unknown); the set size is 0 (unknown); the source stays
// unknown. Never rejected, never a wrong rank or set size.
// RED before: the associated item was recorded at rank 11, k=11.
func TestAssociatedLabel_LegacyReadOnlyIDStaysAcceptedAndHonest(t *testing.T) {
	c := newCFEnv(t)
	c.eng.qidKey = auth.NewQueryIDKey([]byte(assocTestSecret))
	const vault = "trail-assoc-ro-legacy"
	f := cfBuild(c, vault)
	resp := c.activate(vault, cfQuery, 10, true, nil)
	if assocIndex(resp.Associated, f.injectee) < 0 || len(resp.Activations) != 10 {
		t.Fatalf("premise: ten matched rows and the injectee in associated: %d, %+v", len(resp.Activations), resp.Associated)
	}
	var rows [][16]byte
	for _, a := range resp.Activations {
		rows = append(rows, [16]byte(mustULID(t, a.ID)))
	}
	for _, a := range resp.Associated {
		rows = append(rows, [16]byte(mustULID(t, a.ID)))
	}
	ev := storage.NewULID()
	legacy := mintLegacyReadOnlyID(c.eng.qidKey, vault, ev, rows)
	if kind, _ := auth.ClassifyQueryID(legacy); kind != auth.QueryIDReadOnly || legacy[:4] != "ro1." {
		t.Fatalf("premise: a legacy ro1. id, got %q", legacy)
	}
	tail := mustULID(t, resp.Activations[9].ID) // matched rank 10: within the ambiguous tail
	for _, tc := range []struct {
		name     string
		id       storage.ULID
		wantRank uint16
	}{{"matched rank 1", mustULID(t, resp.Activations[0].ID), 1}, {"matched rank 10 (ambiguous tail)", tail, 0}, {"associated item", f.injectee, 0}} {
		outcome, l := assocLabelFor(t, c, vault, legacy, tc.id)
		if outcome != FeedbackOutcomeRecorded {
			t.Errorf("%s: legacy id label %s, want recorded", tc.name, outcome)
			continue
		}
		if l.Rank != tc.wantRank || l.SetSize != 0 || l.Source != storage.RecallSourceUnknown || l.Membership != storage.LabelMembershipToken {
			t.Errorf("%s: legacy id label rank=%d k=%d source=%08b membership=%d, want rank %d, k 0 (unknown), source unknown, token",
				tc.name, l.Rank, l.SetSize, l.Source, l.Membership, tc.wantRank)
		}
	}
}

// TestAssociated_UnmarkedSurfaceGetsNoBlockAndRecordsNone — the structural
// half of the served-set rule: a caller that does not declare it returns the
// associated block (gRPC, the embedded Go library, any future surface) gets
// none, and its recall records no associated entry — so an id the caller
// never received can never be recorded as served, whoever forgets what.
func TestAssociated_UnmarkedSurfaceGetsNoBlockAndRecordsNone(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-unmarked"
	f := cfBuild(c, vault)
	if rendered := c.activate(vault, cfQuery, 10, true, nil); assocIndex(rendered.Associated, f.injectee) < 0 {
		t.Fatalf("premise: a rendering surface gets the injectee in associated")
	}
	resp, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: vault, Context: []string{cfQuery}, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	c.eng.activation.WaitLogIdle()
	if resp.Associated != nil || resp.AssociatedOmitted != 0 {
		t.Errorf("an unmarked caller received an associated block: %+v (omitted %d)", resp.Associated, resp.AssociatedOmitted)
	}
	outcome, _ := assocLabelFor(t, c, vault, resp.QueryID, f.injectee)
	if outcome != FeedbackOutcomeRejected {
		t.Errorf("a label for an id the unmarked caller never received was %s, want rejected", outcome)
	}
}
