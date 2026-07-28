package consolidation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

func eng(concept, content string) *storage.Engram {
	return &storage.Engram{Concept: concept, Content: content}
}

// TestDedup_SeparationGuard_KeepsDivergingNearDuplicate is the end-to-end wiring
// proof: two memories with IDENTICAL embeddings that differ only in a number
// ("$99" vs "$149") must BOTH survive dedup — neither archived — because they are
// distinct facts, not duplicates. Before the fix, the higher-scored one is kept and
// the other is silently archived (destroyed).
func TestDedup_SeparationGuard_KeepsDivergingNearDuplicate(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()
	ctx := context.Background()
	vault := "dedup_sep"
	ws := store.ResolveVaultPrefix(vault)
	embed := []float32{1, 0, 0, 0} // identical vectors → cosine 1.0

	id99 := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "pricing", Content: "Founder pricing is $99 per month",
		Confidence: 0.9, Relevance: 0.9, Stability: 30, Embedding: embed,
	})
	id149 := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "pricing", Content: "Founder pricing is $149 per month",
		Confidence: 0.5, Relevance: 0.5, Stability: 30, Embedding: embed,
	})

	w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase2Dedup(ctx, store, ws, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.MergedEngrams != 0 {
		t.Errorf("MergedEngrams = %d, want 0 (diverging facts must not merge)", report.MergedEngrams)
	}
	if report.DedupSeparated != 1 {
		t.Errorf("DedupSeparated = %d, want 1", report.DedupSeparated)
	}
	for _, id := range []storage.ULID{id99, id149} {
		got, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == storage.StateArchived {
			t.Errorf("engram %s was archived — a distinct fact was destroyed", got.Content)
		}
	}
}

// TestDedup_SeparationGuard_NoTagContamination pins the refute-pass blocking bug:
// in a mixed cluster (survivor + a true duplicate + a numeric-diverging member),
// the survivor must absorb the TRUE duplicate's tag but NEVER the separated
// member's tag — a fact the guard declared distinct must not have its provenance
// cross-labeled onto the survivor.
func TestDedup_SeparationGuard_NoTagContamination(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()
	ctx := context.Background()
	vault := "dedup_tagcontam"
	ws := store.ResolveVaultPrefix(vault)
	embed := []float32{1, 0, 0, 0}

	repID := writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "pricing", Content: "Founder pricing is $99 per month",
		Confidence: 0.9, Relevance: 0.9, Stability: 30, Embedding: embed, Tags: []string{"rep_tag"},
	})
	// True duplicate of the survivor (same number) — its tag SHOULD be absorbed.
	writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "pricing", Content: "Founder pricing is $99 per month",
		Confidence: 0.5, Relevance: 0.5, Stability: 30, Embedding: embed, Tags: []string{"truedup_tag"},
	})
	// Numeric-diverging member — its tag MUST NOT leak onto the survivor.
	writeEngramWithEmbedding(t, ctx, store, db, ws, &storage.Engram{
		Concept: "pricing", Content: "Founder pricing is $149 per month",
		Confidence: 0.4, Relevance: 0.4, Stability: 30, Embedding: embed, Tags: []string{"diverging_tag"},
	})

	w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100}
	if err := w.runPhase2Dedup(ctx, store, ws, &ConsolidationReport{}, vault); err != nil {
		t.Fatal(err)
	}

	rep, err := store.GetEngram(ctx, ws, repID)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	for _, tg := range rep.Tags {
		tags[tg] = true
	}
	if !tags["truedup_tag"] {
		t.Error("survivor should absorb the TRUE duplicate's tag")
	}
	if tags["diverging_tag"] {
		t.Errorf("CONTAMINATION: survivor absorbed the separated $149 member's tag; tags=%v", rep.Tags)
	}
}

// TestDivergesOnLoadBearingToken is the pattern-separation core: memories that
// differ in a number, date, or negation must be judged DISTINCT (guard=true) even
// though they'd embed nearly identically; true paraphrases must be judged mergeable
// (guard=false). Bias: refuse-to-merge over destroy — but never so trigger-happy it
// blocks genuine duplicates.
func TestDivergesOnLoadBearingToken(t *testing.T) {
	cases := []struct {
		name        string
		a, b        *storage.Engram
		wantDiverge bool
	}{
		// The strategy's exact destruction cases — MUST be kept apart.
		{"pricing number differs", eng("pricing", "Founder pricing is $99 per month"),
			eng("pricing", "Founder pricing is $149 per month"), true},
		{"runway number differs", eng("runway", "Runway was 8 months in May"),
			eng("runway", "Runway is 11 months after the bridge"), true},
		{"cac number differs", eng("cac", "CAC ceiling is 80 dollars"),
			eng("cac", "CAC ceiling raised to 120 dollars"), true},
		{"negation asymmetry", eng("policy", "We ship on Fridays"),
			eng("policy", "We do not ship on Fridays"), true},
		{"correction marker", eng("vendor", "The contract with Acme is active"),
			eng("vendor", "The contract with Acme was cancelled"), true},
		{"year differs", eng("launch", "Launch planned for 2025"),
			eng("launch", "Launch planned for 2026"), true},
		{"month differs", eng("review", "Board review in March"),
			eng("review", "Board review in April"), true},

		// True duplicates / benign paraphrases — MUST remain mergeable.
		{"same number reworded", eng("pricing", "Founder pricing is $99 per month"),
			eng("pricing", "The founder plan costs $99/mo"), false},
		{"pure paraphrase no numbers", eng("dana", "Dana owns brand and design"),
			eng("dana", "Dana is responsible for branding and design"), false},
		{"US thousands separator equivalent", eng("arr", "ARR crossed 1,000,000"),
			eng("arr", "ARR crossed 1000000"), false},
		{"identical", eng("x", "the sky is blue"), eng("x", "the sky is blue"), false},

		// Conservative normalization — ambiguous formats are treated as DISTINCT
		// (safe over-fire) so a real numeric difference can never collapse.
		{"trailing-zero decimal kept distinct (safe)", eng("rate", "Conversion is 2.50 percent"),
			eng("rate", "Conversion is 2.5 percent"), true},
		{"european thousands not collapsed to 1", eng("cost", "Cost is 1.000 dollars"),
			eng("cost", "Cost is 1 dollar"), true},
		{"european decimal not collapsed to 15", eng("margin", "Margin is 1,5 percent"),
			eng("margin", "Margin is 15 percent"), true},

		// Documented BLIND SPOTS — the guard does NOT catch these yet, so it reports
		// diverge=false (they would merge). Pinned so a future broadening is a
		// deliberate change to these expectations, not a silent one.
		{"BLIND: same number different currency", eng("price", "It costs $99"),
			eng("price", "It costs 99 euros"), false},
		{"BLIND: entity swap", eng("office", "The office is in Boston"),
			eng("office", "The office is in NYC"), false},
		{"BLIND: spelled-out numbers", eng("runway", "Runway is eight months"),
			eng("runway", "Runway is nine months"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := divergesOnLoadBearingToken(tc.a, tc.b); got != tc.wantDiverge {
				t.Errorf("diverge(%q | %q) = %v, want %v", tc.a.Content, tc.b.Content, got, tc.wantDiverge)
			}
			// Symmetry: the guard must not depend on argument order.
			if got := divergesOnLoadBearingToken(tc.b, tc.a); got != tc.wantDiverge {
				t.Errorf("diverge is asymmetric for %q | %q", tc.a.Content, tc.b.Content)
			}
		})
	}
}
