package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// The `associated` channel (COG-35 follow-up). Content-first order stops
// association-only rows (entity-boost injections that match none of the
// query's words) from displacing matches, which also dropped them from recall
// entirely once the matches filled the limit. They now get a separate,
// bounded, labelled block: at most 3, compact, ~600 bytes, not counted
// against `limit`, never a match. Fixtures are invented (trail maintenance).
// ---------------------------------------------------------------------------

func (c *cfEnv) activate(vault, query string, limit int, readOnly bool, mut func(*mbp.ActivateRequest)) *mbp.ActivateResponse {
	c.t.Helper()
	req := &mbp.ActivateRequest{Vault: vault, Context: []string{query}, MaxResults: limit, ReadOnly: readOnly}
	if mut != nil {
		mut(req)
	}
	// As MCP/REST/MBP call it: a surface that returns the associated block.
	resp, err := c.eng.Activate(mbp.WithAssociatedRendered(context.Background()), req)
	if err != nil {
		c.t.Fatal(err)
	}
	if !readOnly {
		c.eng.activation.WaitLogIdle()
	}
	return resp
}

func assocIndex(items []mbp.AssociatedItem, id storage.ULID) int {
	for i, a := range items {
		if a.ID == id.String() {
			return i
		}
	}
	return -1
}

// TestAssociated_EntityOnlyTargetMovesToTheAssociatedBlock — the memory that
// shares the top match's rare entities and matches none of the query's words
// is not in `memories` at limit 10 (ten better matches exist), and IS in
// `associated`, naming the entity and the returned row it is associated with.
func TestAssociated_EntityOnlyTargetMovesToTheAssociatedBlock(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc"
	f := cfBuild(c, vault)
	resp := c.activate(vault, cfQuery, 10, true, nil)
	for _, a := range resp.Activations {
		if a.ID == f.injectee.String() {
			t.Fatalf("premise: the association-only memory must not make the limit=10 cut")
		}
	}
	i := assocIndex(resp.Associated, f.injectee)
	if i < 0 {
		t.Fatalf("the association-only memory is in neither memories nor associated: %+v", resp.Associated)
	}
	a := resp.Associated[i]
	if !strings.Contains(a.Why, f.strong[0].String()) || !(strings.Contains(a.Why, "Kestrel Saddle") || strings.Contains(a.Why, "Harrowmoor Spur")) {
		t.Errorf("why must name the shared entity and the returned row it came through: %q", a.Why)
	}
	if a.Basis != "associated_not_matched" || a.Strength <= 0 || a.Concept == "" {
		t.Errorf("item shape: %+v", a)
	}
	for _, m := range resp.Activations {
		if assocIndex(resp.Associated, mustULID(t, m.ID)) >= 0 {
			t.Errorf("row %s is in both memories and associated", m.ID)
		}
	}
	if off := c.activate(vault, cfQuery, 10, true, func(r *mbp.ActivateRequest) { r.ExcludeAssociated = true }); off.Associated != nil {
		t.Errorf("exclude_associated must remove the block, got %+v", off.Associated)
	}
}

func mustULID(t *testing.T, s string) storage.ULID {
	t.Helper()
	id, err := storage.ParseULID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAssociated_AbsentWhenNothingIsAssociationOnly — no association-only
// rows means no `associated` key at all (nil, omitted from JSON), not [].
func TestAssociated_AbsentWhenNothingIsAssociationOnly(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-none"
	c.write(vault, "culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two", time.Hour)
	c.write(vault, "riprap supply", "riprap stone supply", 2*time.Hour)
	resp := c.activate(vault, cfQuery, 10, true, nil)
	if len(resp.Activations) == 0 {
		t.Fatal("premise: rows expected")
	}
	b, _ := json.Marshal(resp)
	if resp.Associated != nil || strings.Contains(string(b), `"associated"`) {
		t.Errorf("a response with no association-only rows must carry no associated key: %s", b)
	}
}

// TestAssociated_CapAndByteBudget — many association-only memories with long
// concepts: at most 3 items, and the JSON block stays within 600 bytes.
func TestAssociated_CapAndByteBudget(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-cap"
	f := cfBuild(c, vault)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	for i := 0; i < 8; i++ {
		// Each shares its own rare entity with the top match (df 2).
		place := fmt.Sprintf("Wrenfold Notch %d", i)
		c.write(vault, fmt.Sprintf("saddle permit desk notes volume %d with a deliberately long concept for byte budgeting", i),
			"permits issued at the saddle desk", time.Duration(i+1)*time.Minute, place)
		if err := c.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: place, Type: "place", Source: "inline"}, "inline"); err != nil {
			t.Fatal(err)
		}
		if err := c.store.WriteEntityEngramLink(ctx, ws, f.strong[0], place); err != nil {
			t.Fatal(err)
		}
	}
	resp := c.activate(vault, cfQuery, 10, true, nil)
	if len(resp.Associated) == 0 {
		t.Fatal("premise: association-only rows exist")
	}
	if len(resp.Associated) > 3 {
		t.Errorf("associated carries %d items, cap is 3", len(resp.Associated))
	}
	b, _ := json.Marshal(resp.Associated)
	t.Logf("associated: %d items, %d bytes", len(resp.Associated), len(b))
	if len(resp.Associated) < 2 {
		t.Errorf("premise: eight admissible association-only memories exist; the block should be filled to its byte budget, got %d", len(resp.Associated))
	}
	if len(b) > 600 {
		t.Errorf("associated block is %d bytes, cap is 600: %s", len(b), b)
	}
	for i := 1; i < len(resp.Associated); i++ {
		if resp.Associated[i].Strength > resp.Associated[i-1].Strength {
			t.Errorf("associated not strongest-first: %+v", resp.Associated)
		}
	}
}

// TestAssociated_HonoursTheInjectionContract — a leased, untrusted (on a vault
// that excludes untrusted), expired or superseded memory sharing the rare
// entity is never shown, exactly as entity-boost injection already refuses it.
func TestAssociated_HonoursTheInjectionContract(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-gate"
	tr := true
	if err := c.as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true, Plasticity: &auth.PlasticityConfig{ExcludeUntrusted: &tr}}); err != nil {
		t.Fatal(err)
	}
	f := cfBuild(c, vault)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	mk := func(concept string, mutate func(*storage.Engram)) storage.ULID {
		e := &storage.Engram{Concept: concept, Content: "permits for the saddle desk", Confidence: 1,
			CreatedAt: time.Now(), UpdatedAt: time.Now(), LastAccess: time.Now()}
		mutate(e)
		id, err := c.store.WriteEngram(ctx, ws, e)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"Kestrel Saddle", "Harrowmoor Spur"} {
			if err := c.store.WriteEntityEngramLink(ctx, ws, id, n); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	untrusted := mk("untrusted saddle note", func(e *storage.Engram) { e.Trust = storage.TrustUntrusted })
	expired := mk("expired saddle note", func(e *storage.Engram) { e.ValidUntil = time.Now().Add(-time.Hour) })
	superseded := mk("superseded saddle note", func(e *storage.Engram) {
		e.State = storage.StateSoftDeleted
		e.ValidUntil = time.Now().Add(-time.Hour)
	})
	leased := mk("leased saddle note", func(*storage.Engram) {})
	if _, err := c.eng.Claim(ctx, vault, leased.String(), "another-crew", 3600); err != nil {
		t.Fatal(err)
	}
	resp := c.activate(vault, cfQuery, 10, true, nil)
	if assocIndex(resp.Associated, f.injectee) < 0 {
		t.Fatalf("premise: the admissible association-only memory must be shown: %+v", resp.Associated)
	}
	for name, id := range map[string]storage.ULID{"untrusted": untrusted, "expired": expired, "superseded": superseded, "leased": leased} {
		if assocIndex(resp.Associated, id) >= 0 {
			t.Errorf("%s memory shown in associated", name)
		}
	}
}

// TestAssociated_LabelAcceptedWithSourceAssociated — an associated id is part
// of what the recall served: muninn_feedback accepts a label for it and the
// label records the associated source flag (so the label pipeline can learn
// whether association is ever useful). Read-only and normal recalls carry the
// same block.
func TestAssociated_LabelAcceptedWithSourceAssociated(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-assoc-label"
	f := cfBuild(c, vault)
	ro1 := c.activate(vault, cfQuery, 10, true, nil)
	ro2 := c.activate(vault, cfQuery, 10, true, nil)
	normal := c.activate(vault, cfQuery, 10, false, nil)
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if j(ro1.Associated) != j(ro2.Associated) || j(ro1.Associated) != j(normal.Associated) {
		t.Errorf("associated differs between identical recalls\nro1 %s\nro2 %s\nnormal %s", j(ro1.Associated), j(ro2.Associated), j(normal.Associated))
	}
	if assocIndex(normal.Associated, f.injectee) < 0 {
		t.Fatalf("premise: injectee in associated: %+v", normal.Associated)
	}
	out, err := c.eng.RecordFeedbackLabelsDetailed(context.Background(), vault,
		[]FeedbackGroup{{QueryID: normal.QueryID, IDs: []string{f.injectee.String()}, Useful: true}}, FeedbackChannelDirect, FeedbackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Outcome != FeedbackOutcomeRecorded {
		t.Fatalf("label for an associated id was not recorded: %+v", out)
	}
	ws := c.store.ResolveVaultPrefix(vault)
	found := false
	if err := c.store.ScanRecallLabels(context.Background(), ws, storage.RecallPurposeLabel, func(r storage.RecallLabelRow) error {
		if r.EngramID == f.injectee {
			found = true
			if r.Label.Source&storage.RecallSourceAssociated == 0 {
				t.Errorf("label source flags %08b lack the associated bit", r.Label.Source)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("no stored label for the associated id")
	}
}

// TestAssociated_NotOnRRFOrWeightedSum — the channel exists for content-first
// responses only. rrf skips the entity boost; weighted_sum still lets an
// injected row compete for `memories` by score, exactly as on the trunk (the
// differential golden pins those responses byte-for-byte).
func TestAssociated_NotOnRRFOrWeightedSum(t *testing.T) {
	c := newCFEnv(t)
	for _, fusion := range []string{"rrf", "weighted_sum"} {
		vault := "trail-assoc-" + fusion
		c.configure(vault, fusion)
		cfBuild(c, vault)
		if resp := c.activate(vault, cfQuery, 10, true, nil); resp.Associated != nil {
			t.Errorf("%s vault carries an associated block: %+v", fusion, resp.Associated)
		}
	}
}
