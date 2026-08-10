package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// COG-29 amendment — the vault-wide `unresolved_contradictions` block on the
// three MCP orientation surfaces.
//
// Every fixture here is invented and sits in a domain this project has no client
// in (trail maintenance and beekeeping). The two topics are deliberately
// disjoint so a query about one retrieves nothing about the other — that
// separation is the whole point of A1.

// newDebtServer wires a REAL engine behind the MCP server and returns the store
// too, so a declaration can be given a backdated timestamp on disk. The stub
// engine used elsewhere cannot see the two places a response-level field
// silently vanishes on this surface (the adapter, and handleRecall's hand-built
// map), which is exactly what these tests are for.
func newDebtServer(t *testing.T) (*engine.Engine, *MCPServer, *storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-mcp-debt-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder,
	})
	srv := newTestServerWith(NewEngineAdapter(eng, nil, nil))
	return eng, srv, store, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// declareBackdatedContradiction writes two rival facts and declares the conflict
// between them with an explicit on-disk declaration time. The edge is written
// through the store rather than muninn_link precisely so the timestamp is
// controlled — this is the same on-disk shape a declaration made by an earlier
// process has, which is the state the motivating incident was in.
func declareBackdatedContradiction(t *testing.T, eng *engine.Engine, store *storage.PebbleStore, vault, subject, factA, factB string, declaredAt time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: subject, Content: factA})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: subject + " revised", Content: factB})
	if err != nil {
		t.Fatal(err)
	}
	idA, err := storage.ParseULID(wa.ID)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := storage.ParseULID(wb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteAssociation(ctx, store.ResolveVaultPrefix(vault), idB, idA, &storage.Association{
		TargetID: idA, RelType: storage.RelContradicts, Weight: 0.8, Confidence: 1,
		CreatedAt: declaredAt,
	}); err != nil {
		t.Fatal(err)
	}
	eng.WaitWriteTimeIdle()
	return wa.ID, wb.ID
}

func callTool(t *testing.T, srv *MCPServer, name, args string) map[string]any {
	t.Helper()
	rpc := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":%q,"arguments":%s}}`, name, args)
	rec := postRPC(t, srv, rpc)
	if rec.Code != 200 {
		t.Fatalf("%s: HTTP %d: %s", name, rec.Code, rec.Body.String())
	}
	return extractInnerJSON(t, decodeResp(t, rec.Body.String()))
}

func callToolText(t *testing.T, srv *MCPServer, name, args string) string {
	t.Helper()
	rpc := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":%q,"arguments":%s}}`, name, args)
	rec := postRPC(t, srv, rpc)
	if rec.Code != 200 {
		t.Fatalf("%s: HTTP %d: %s", name, rec.Code, rec.Body.String())
	}
	resp := decodeResp(t, rec.Body.String())
	if resp.Error != nil {
		t.Fatalf("%s: JSON-RPC error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}
	wrapper, _ := resp.Result.(map[string]any)
	contents, _ := wrapper["content"].([]any)
	if len(contents) == 0 {
		t.Fatalf("%s: empty content", name)
	}
	item, _ := contents[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

// TestContradictionDebt_SurfacedAtSessionStartWhenNeitherSideIsRetrieved is A1,
// the production-incident reproduction.
//
// A conflict is declared on topic X and left for 26 hours. The agent then
// orients on an unrelated topic Y. The pre-existing paths are asserted SILENT
// first — neither conflicted memory is in the results, and the response-level
// `conflict` block is nil — which is exactly the incident: the demote is waiting
// to be charged and no surface ever says so. The debt block must speak anyway,
// on all three orientation surfaces.
func TestContradictionDebt_SurfacedAtSessionStartWhenNeitherSideIsRetrieved(t *testing.T) {
	eng, srv, store, cleanup := newDebtServer(t)
	defer cleanup()
	ctx := context.Background()

	declaredAt := time.Now().Add(-26 * time.Hour)
	a, b := declareBackdatedContradiction(t, eng, store, "default", "trestle bridge decking width",
		"the trestle bridge decking is 1.2 metres wide",
		"the trestle bridge decking is 1.6 metres wide", declaredAt)

	// Unrelated, newer work on a disjoint topic — what the agent is actually
	// doing when it comes back the next day.
	for i := 0; i < 6; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default",
			Concept: fmt.Sprintf("apiary hive %d inspection", i),
			Content: fmt.Sprintf("hive %d had capped brood on frame %d during the apiary inspection", i, i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	eng.WaitWriteTimeIdle()

	// CONTROL, and the thing that makes the rest of this test mean something: a
	// query that DOES retrieve both sides gets the pre-existing COG-29 conflict
	// block. The conflict is live, the machinery works, and the demote is
	// waiting — so any silence below is about the QUERY, not about the fixture.
	control := callTool(t, srv, "muninn_recall",
		`{"vault":"default","context":["trestle bridge decking"],"limit":5,"threshold":0.001}`)
	if control["conflict"] == nil {
		raw, _ := json.Marshal(control)
		t.Fatalf("control: a query that retrieves both sides carries no conflict block — "+
			"the fixture never armed COG-29 at all: %s", raw)
	}
	if _, present := control["unresolved_contradictions"]; present {
		t.Error("control: default-mode recall must not carry the debt block (increment 1 defers the hot path)")
	}

	// --- muninn_recall(mode="recent") — the shared-vault session-start call ---
	got := callTool(t, srv, "muninn_recall",
		`{"vault":"default","context":["apiary inspection","session start"],"mode":"recent","limit":4,"threshold":0.001}`)

	memories, _ := got["memories"].([]any)
	for _, raw := range memories {
		m, _ := raw.(map[string]any)
		if id, _ := m["id"].(string); id == a || id == b {
			t.Fatalf("precondition failed: the orientation query retrieved a conflicted memory (%s) — "+
				"this fixture must reproduce the incident, where NEITHER side is retrieved", id)
		}
	}
	if got["conflict"] != nil {
		t.Fatalf("precondition failed: the pre-existing COG-29 conflict block fired (%v); "+
			"the incident is precisely that it does NOT", got["conflict"])
	}

	assertDebtBlock(t, "muninn_recall(mode=recent)", got, a, b, 24)

	// --- muninn_where_left_off ---
	assertDebtBlock(t, "muninn_where_left_off",
		callTool(t, srv, "muninn_where_left_off", `{"vault":"default"}`), a, b, 24)

	// --- muninn_guide (prose, same derivation) ---
	guide := callToolText(t, srv, "muninn_guide", `{"vault":"default"}`)
	if !strings.Contains(guide, "1 unresolved declared contradiction") {
		t.Errorf("muninn_guide does not report the vault's outstanding debt:\n%s", guide)
	}
	if !strings.Contains(guide, a) || !strings.Contains(guide, b) {
		t.Errorf("muninn_guide does not name the conflicting pair (%s, %s)", a, b)
	}
	if !strings.Contains(guide, "muninn_evolve") || !strings.Contains(guide, "not_true_since") {
		t.Error("muninn_guide's debt paragraph does not name the resolution actions")
	}
}

// assertDebtBlock checks the shared wire shape on a JSON orientation response.
func assertDebtBlock(t *testing.T, surface string, resp map[string]any, a, b string, minAgeHours float64) {
	t.Helper()
	block, ok := resp["unresolved_contradictions"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(resp)
		t.Errorf("%s carries no unresolved_contradictions block: %s", surface, raw)
		return
	}
	if c, _ := block["count"].(float64); c != 1 {
		t.Errorf("%s: count = %v, want 1", surface, block["count"])
	}
	if age, _ := block["oldest_age_hours"].(float64); age < minAgeHours {
		t.Errorf("%s: oldest_age_hours = %v, want >= %v", surface, block["oldest_age_hours"], minAgeHours)
	}
	if act, _ := block["action"].(string); !strings.Contains(act, "muninn_evolve") ||
		!strings.Contains(act, "not_true_since") || !strings.Contains(act, "supersedes") {
		t.Errorf("%s: action does not name all three resolution verbs: %q", surface, act)
	}
	if sc, _ := block["scan_complete"].(bool); !sc {
		t.Errorf("%s: scan_complete = false on a tiny vault", surface)
	}
	pairs, _ := block["pairs"].([]any)
	if len(pairs) != 1 {
		t.Errorf("%s: pairs = %v, want exactly one", surface, block["pairs"])
		return
	}
	p, _ := pairs[0].(map[string]any)
	ida, _ := p["id_a"].(string)
	idb, _ := p["id_b"].(string)
	if !(ida == a && idb == b) && !(ida == b && idb == a) {
		t.Errorf("%s: pair = (%s,%s), want the fixture pair (%s,%s)", surface, ida, idb, a, b)
	}
	if ca, _ := p["concept_a"].(string); ca == "" {
		t.Errorf("%s: pair concept_a is empty", surface)
	}
	if age, _ := p["age_hours"].(float64); age < minAgeHours {
		t.Errorf("%s: pair age_hours = %v, want >= %v", surface, p["age_hours"], minAgeHours)
	}
	if _, present := block["scope_note"]; present {
		t.Errorf("%s: scope_note must be absent on a single-user vault", surface)
	}
}

// TestContradictionDebt_CleanVaultAddsZeroBytes is A2. On a vault with no
// declared contradiction, no orientation surface may carry the key at all —
// absent, not an empty object. A standing empty object on every orientation call
// is the wallpaper #609 died of.
func TestContradictionDebt_CleanVaultAddsZeroBytes(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default",
			Concept: fmt.Sprintf("trail segment %d surface", i),
			Content: fmt.Sprintf("segment %d of the ridge trail is crushed limestone", i)}); err != nil {
			t.Fatal(err)
		}
	}
	eng.WaitWriteTimeIdle()

	recall := callTool(t, srv, "muninn_recall",
		`{"vault":"default","context":["ridge trail surface"],"mode":"recent","limit":4,"threshold":0.001}`)
	if _, present := recall["unresolved_contradictions"]; present {
		t.Errorf("muninn_recall carries the key on a clean vault: %v", recall["unresolved_contradictions"])
	}
	wlo := callTool(t, srv, "muninn_where_left_off", `{"vault":"default"}`)
	if _, present := wlo["unresolved_contradictions"]; present {
		t.Errorf("muninn_where_left_off carries the key on a clean vault: %v", wlo["unresolved_contradictions"])
	}
	guide := callToolText(t, srv, "muninn_guide", `{"vault":"default"}`)
	if strings.Contains(guide, "unresolved declared contradiction") {
		t.Error("muninn_guide reports debt on a clean vault")
	}
}

// TestContradictionDebt_DefaultModeRecallIsUntouched pins the deferral: only
// mode="recent" carries the block. The hot path stays exactly as it was until
// the field measurement returns a number.
func TestContradictionDebt_DefaultModeRecallIsUntouched(t *testing.T) {
	eng, srv, store, cleanup := newDebtServer(t)
	defer cleanup()

	declareBackdatedContradiction(t, eng, store, "default", "waterbar spacing",
		"waterbars on the ridge trail sit 8 metres apart",
		"waterbars on the ridge trail sit 12 metres apart", time.Now().Add(-26*time.Hour))

	// Same query, no mode: the block must be absent.
	plain := callTool(t, srv, "muninn_recall",
		`{"vault":"default","context":["waterbar spacing"],"limit":4,"threshold":0.001}`)
	if _, present := plain["unresolved_contradictions"]; present {
		t.Errorf("default-mode recall carries the block; increment 1 defers the hot path: %v",
			plain["unresolved_contradictions"])
	}
	// ...and the same query WITH mode=recent does carry it, so this test is
	// asserting a scope, not a broken feature.
	recent := callTool(t, srv, "muninn_recall",
		`{"vault":"default","context":["waterbar spacing"],"mode":"recent","limit":4,"threshold":0.001}`)
	if _, present := recent["unresolved_contradictions"]; !present {
		t.Error("mode=recent recall carries no block, so the default-mode assertion above proves nothing")
	}
}

// TestContradictionDebtBlock_UnknownAgeIsAbsentNotEpoch — a legacy edge with no
// declaration timestamp must render as ABSENT plus an explicit flag, never as
// 1970 and never as a zero age. This is the rule engine_adapter.go already
// applies to the contradictions surface's own timestamps.
func TestContradictionDebtBlock_UnknownAgeIsAbsentNotEpoch(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	block := contradictionDebtBlock(&engine.ContradictionDebt{
		Count: 2, Truncated: false, ScanComplete: false,
		Pairs: []engine.ContradictionDebtPair{
			{IDa: "A", ConceptA: "hive 1 queen age", IDb: "B", ConceptB: "hive 1 queen age revised"},
			{IDa: "C", ConceptA: "hive 2 queen age", IDb: "D", ConceptB: "hive 2 queen age revised",
				DeclaredAt: now.Add(-3 * time.Hour)},
		},
	}, false, now)

	if _, present := block["oldest_declared_at"]; present {
		t.Errorf("oldest_declared_at present for an unknown oldest: %v", block["oldest_declared_at"])
	}
	if _, present := block["oldest_age_hours"]; present {
		t.Errorf("oldest_age_hours present for an unknown oldest: %v", block["oldest_age_hours"])
	}
	pairs, _ := block["pairs"].([]map[string]any)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %v", block["pairs"])
	}
	if pairs[0]["declared_at_unknown"] != true {
		t.Error("the undated pair is not flagged declared_at_unknown")
	}
	if _, present := pairs[0]["declared_at"]; present {
		t.Errorf("the undated pair carries declared_at = %v", pairs[0]["declared_at"])
	}
	if _, present := pairs[0]["age_hours"]; present {
		t.Errorf("the undated pair carries age_hours = %v — zero is not an age", pairs[0]["age_hours"])
	}
	if pairs[1]["age_hours"] != 3.0 {
		t.Errorf("the dated pair's age_hours = %v, want 3", pairs[1]["age_hours"])
	}
	if block["scan_complete"] != false {
		t.Error("scan_complete must propagate: a capped scan makes the count a lower bound")
	}
}

// debtStubEngine is a stub with a fixed debt readout and a configurable resolved
// plasticity, for the two config-driven cases. It exercises the real handler
// path without paying for a Pebble store.
type debtStubEngine struct {
	whereLeftOffEngine
	plasticity auth.ResolvedPlasticity
	calls      int
}

// WhereLeftOff returns a FIXED entry set: the byte-identity assertion below
// compares two responses, so a time.Now() in the fixture would make them differ
// for a reason that has nothing to do with the switch.
func (e *debtStubEngine) WhereLeftOff(_ context.Context, _ string, _ int, _ []string) ([]WhereLeftOffEntry, error) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return []WhereLeftOffEntry{
		{ID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Concept: "hive 4 winter feed", LastAccess: at, State: "active", Type: "task"},
	}, nil
}

func (e *debtStubEngine) GetVaultPlasticity(_ context.Context, _ string) (*auth.ResolvedPlasticity, error) {
	p := e.plasticity
	return &p, nil
}

func (e *debtStubEngine) ContradictionDebt(_ context.Context, _ string) (*engine.ContradictionDebt, error) {
	e.calls++
	return &engine.ContradictionDebt{
		Count:        1,
		Oldest:       time.Now().Add(-9 * time.Hour),
		ScanComplete: true,
		Pairs: []engine.ContradictionDebtPair{{
			IDa: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConceptA: "hive 4 queen age",
			IDb: "01ARZ3NDEKTSV4RRFFQ69G5FAW", ConceptB: "hive 4 queen age revised",
			DeclaredAt: time.Now().Add(-9 * time.Hour),
		}},
	}, nil
}

// TestContradictionDebt_MultiUserVaultCarriesTheScopeNote — in a shared vault
// the readout is vault-global, so an agent that scoped its recall to its own tag
// must be told these conflicts are not necessarily its own.
func TestContradictionDebt_MultiUserVaultCarriesTheScopeNote(t *testing.T) {
	p := auth.ResolvePlasticity(nil)
	p.MultiUser = true
	srv := newTestServerWith(&debtStubEngine{plasticity: p})

	block, ok := callTool(t, srv, "muninn_where_left_off", `{"vault":"default"}`)["unresolved_contradictions"].(map[string]any)
	if !ok {
		t.Fatal("no unresolved_contradictions block on a multi-user vault")
	}
	note, _ := block["scope_note"].(string)
	if !strings.Contains(note, "shared") || !strings.Contains(note, "ALL users") {
		t.Errorf("scope_note = %q, want the shared-vault caveat", note)
	}
}

// TestContradictionDebt_PlasticityOffSuppressesOnlyTheBlock is the §11 Q1
// control arm. With the per-vault switch off the block disappears and nothing
// else about the response changes — and the derivation is never even called, so
// the switch is a real cost gate, not a render-then-discard.
func TestContradictionDebt_PlasticityOffSuppressesOnlyTheBlock(t *testing.T) {
	off := false
	stubOff := &debtStubEngine{plasticity: auth.ResolvePlasticity(&auth.PlasticityConfig{ContradictionDebt: &off})}
	stubOn := &debtStubEngine{plasticity: auth.ResolvePlasticity(nil)}

	withBlock := callTool(t, newTestServerWith(stubOn), "muninn_where_left_off", `{"vault":"default"}`)
	if _, present := withBlock["unresolved_contradictions"]; !present {
		t.Fatal("control arm: the block is absent with the switch ON, so the OFF assertion proves nothing")
	}
	if stubOn.calls == 0 {
		t.Error("control arm: the derivation was never called with the switch ON")
	}

	withoutBlock := callTool(t, newTestServerWith(stubOff), "muninn_where_left_off", `{"vault":"default"}`)
	if _, present := withoutBlock["unresolved_contradictions"]; present {
		t.Errorf("the per-vault switch is off and the block was delivered anyway: %v",
			withoutBlock["unresolved_contradictions"])
	}
	if stubOff.calls != 0 {
		t.Errorf("the derivation ran %d time(s) with the switch off — the switch must gate the WORK, not just the render", stubOff.calls)
	}

	// Everything else is byte-identical: the switch suppresses the receipt and
	// nothing else.
	delete(withBlock, "unresolved_contradictions")
	on, _ := json.Marshal(withBlock)
	offJSON, _ := json.Marshal(withoutBlock)
	if string(on) != string(offJSON) {
		t.Errorf("the switch changed something other than the block:\n on:  %s\n off: %s", on, offJSON)
	}
}

// failingDebtEngine's debt derivation always errors — a persistent store fault.
type failingDebtEngine struct{ debtStubEngine }

func (e *failingDebtEngine) ContradictionDebt(_ context.Context, _ string) (*engine.ContradictionDebt, error) {
	return nil, errors.New("synthetic store fault")
}

// TestContradictionDebt_DerivationFailureIsSaidOutLoud — F7. Failing open must
// not mean failing SILENT. Emitting nothing on a derivation error makes the
// vault look debt-free, which restores the motivating incident with the
// confidence penalty already charged: the agent is told nothing while both facts
// stay demoted. The response carries an honest marker instead — no count, no
// pairs, no age, because an invented zero is the silently-wrong class.
func TestContradictionDebt_DerivationFailureIsSaidOutLoud(t *testing.T) {
	srv := newTestServerWith(&failingDebtEngine{debtStubEngine{plasticity: auth.ResolvePlasticity(nil)}})

	resp := callTool(t, srv, "muninn_where_left_off", `{"vault":"default"}`)
	block, ok := resp["unresolved_contradictions"].(map[string]any)
	if !ok {
		raw, _ := json.Marshal(resp)
		t.Fatalf("a failed derivation emitted NOTHING — indistinguishable from a debt-free vault: %s", raw)
	}
	if block["unavailable"] != true {
		t.Errorf("unavailable = %v, want true", block["unavailable"])
	}
	if note, _ := block["note"].(string); !strings.Contains(note, "UNKNOWN") {
		t.Errorf("note = %q, want it to say the state is UNKNOWN, not zero", note)
	}
	for _, forbidden := range []string{"count", "pairs", "oldest_age_hours", "showing"} {
		if _, present := block[forbidden]; present {
			t.Errorf("the unavailable marker carries %q = %v — it must invent no numbers", forbidden, block[forbidden])
		}
	}

	// The guide says the same thing in prose.
	guide := callToolText(t, srv, "muninn_guide", `{"vault":"default"}`)
	if !strings.Contains(guide, "could not be read") {
		t.Errorf("muninn_guide is silent about the failed derivation:\n%s", guide)
	}
}

// TestContradictionDebt_LowerBoundIsSaidOnBothRenders — F6. `scan_complete:
// false` next to a confident `count` does not tell a JSON caller the count is a
// floor; the prose render already said so in words. Both renders carry the SAME
// sentence, from one const.
func TestContradictionDebt_LowerBoundIsSaidOnBothRenders(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	capped := &engine.ContradictionDebt{
		Count: 4, Truncated: true, ScanComplete: false,
		Oldest: now.Add(-5 * time.Hour),
		Pairs: []engine.ContradictionDebtPair{{
			IDa: "A", ConceptA: "hive 7 queen age", IDb: "B", ConceptB: "hive 7 queen age revised",
			DeclaredAt: now.Add(-5 * time.Hour),
		}},
	}

	block := contradictionDebtBlock(capped, false, now)
	note, _ := block["note"].(string)
	if !strings.Contains(note, "LOWER BOUND") {
		t.Errorf("JSON block note = %q, want the lower-bound sentence", note)
	}
	prose := contradictionDebtGuideSection(capped, false, now)
	if !strings.Contains(prose, "LOWER BOUND") {
		t.Errorf("prose render does not say LOWER BOUND:\n%s", prose)
	}
	if !strings.Contains(prose, note) {
		t.Errorf("the two renders do not carry the SAME sentence:\n json:  %q\n prose: %s", note, prose)
	}

	// ...and a COMPLETE scan carries no note at all, so the field means
	// something when it appears.
	complete := *capped
	complete.ScanComplete = true
	if _, present := contradictionDebtBlock(&complete, false, now)["note"]; present {
		t.Error("a complete scan carries a lower-bound note")
	}
}
