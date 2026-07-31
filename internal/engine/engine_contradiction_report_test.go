package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// seedContradictionPair writes two opposing engrams into vault "test" and
// returns the engine, the vault prefix, and the two IDs. Synthetic content only.
func seedContradictionPair(t *testing.T) (*Engine, [8]byte, storage.ULID, storage.ULID) {
	t.Helper()
	eng, cleanup := testEnv(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	a, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "widget-color",
		Content: "the test widget is blue",
	})
	if err != nil {
		t.Fatalf("write a: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "widget-color-revised",
		Content: "the test widget is green",
	})
	if err != nil {
		t.Fatalf("write b: %v", err)
	}
	idA, err := storage.ParseULID(a.ID)
	if err != nil {
		t.Fatalf("parse a: %v", err)
	}
	idB, err := storage.ParseULID(b.ID)
	if err != nil {
		t.Fatalf("parse b: %v", err)
	}
	return eng, eng.Store().ResolveVaultPrefix("test"), idA, idB
}

// TestContradictionReport_PendingIsNotNone is the core regression test for the
// defect three independent evaluators hit: an explicit contradicts link is
// durable the moment Link returns, but the 0x0A marker is written by a batch
// worker up to 30s later. Until then the read surface returned an empty list —
// indistinguishable from "this vault has no contradictions". An unknown state
// must never be reported as a known one (CLAUDE.md §2.1/§2.2).
//
// The engine under test has NO ContradictWorker wired, which is exactly the
// "the detector has not run yet" state, held open indefinitely.
func TestContradictionReport_PendingIsNotNone(t *testing.T) {
	eng, _, idA, idB := seedContradictionPair(t)
	ctx := context.Background()

	// Baseline: a vault with no contradiction of any kind.
	none, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("baseline report: %v", err)
	}
	if len(none.Pairs) != 0 || none.PendingCount != 0 || none.DetectedCount != 0 {
		t.Fatalf("baseline should be empty and unambiguous, got %+v", none)
	}

	linkedAt := time.Now()
	if _, err := eng.Link(ctx, &mbp.LinkRequest{
		Vault:    "test",
		SourceID: idB.String(),
		TargetID: idA.String(),
		RelType:  uint16(storage.RelContradicts),
	}); err != nil {
		t.Fatalf("link: %v", err)
	}

	// IMMEDIATELY after Link — no sleep, no worker flush.
	got, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if got.PendingCount != 1 {
		t.Errorf("PendingCount = %d, want 1 (declared link awaiting detection)", got.PendingCount)
	}
	if len(got.Pairs) != 1 {
		t.Fatalf("Pairs = %d, want 1; an empty list here is indistinguishable from 'no contradictions'", len(got.Pairs))
	}
	p := got.Pairs[0]
	if p.Status != ContradictionPending {
		t.Errorf("Status = %q, want %q", p.Status, ContradictionPending)
	}
	if p.DeclaredAt.Before(linkedAt.Add(-time.Second)) || p.DeclaredAt.IsZero() {
		t.Errorf("DeclaredAt = %v, want ~%v", p.DeclaredAt, linkedAt)
	}
	if !p.DetectedAt.IsZero() {
		t.Errorf("DetectedAt = %v, want zero while pending", p.DetectedAt)
	}
	// And the pair must name the two engrams, in canonical order.
	wantA, wantB := idA, idB
	if storage.CompareULIDs(wantA, wantB) > 0 {
		wantA, wantB = wantB, wantA
	}
	if p.IDa != wantA.String() || p.IDb != wantB.String() {
		t.Errorf("pair = (%s,%s), want (%s,%s)", p.IDa, p.IDb, wantA, wantB)
	}
	if !got.ScanComplete {
		t.Errorf("ScanComplete = false on a two-engram vault")
	}
}

// TestContradictionReport_ConceptsAndDetectedAt covers defects (2) and (3):
// concept_a/concept_b came back as empty strings and detected_at as the Go
// zero time, even though both engrams have concepts and the flag has a
// well-defined moment.
func TestContradictionReport_ConceptsAndDetectedAt(t *testing.T) {
	eng, ws, idA, idB := seedContradictionPair(t)
	ctx := context.Background()

	before := time.Now().Add(-time.Second)
	if _, err := eng.Store().FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("flag: %v", err)
	}
	after := time.Now().Add(time.Second)

	got, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got.Pairs) != 1 {
		t.Fatalf("Pairs = %d, want 1", len(got.Pairs))
	}
	p := got.Pairs[0]
	if p.Status != ContradictionDetected {
		t.Errorf("Status = %q, want %q", p.Status, ContradictionDetected)
	}
	if got.DetectedCount != 1 || got.PendingCount != 0 {
		t.Errorf("counts = detected %d / pending %d, want 1/0", got.DetectedCount, got.PendingCount)
	}
	if p.ConceptA == "" || p.ConceptB == "" {
		t.Fatalf("concepts must be populated, got (%q,%q)", p.ConceptA, p.ConceptB)
	}
	concepts := map[string]bool{p.ConceptA: true, p.ConceptB: true}
	if !concepts["widget-color"] || !concepts["widget-color-revised"] {
		t.Errorf("concepts = (%q,%q), want the two seeded concepts", p.ConceptA, p.ConceptB)
	}
	if p.DetectedAt.IsZero() {
		t.Fatalf("DetectedAt is the zero time")
	}
	if p.DetectedAt.Before(before) || p.DetectedAt.After(after) {
		t.Errorf("DetectedAt = %v, want within [%v,%v]", p.DetectedAt, before, after)
	}
}

// TestContradictionReport_DetectedAtIsFirstFlag pins that re-observing an
// already-known contradiction does not move its detection timestamp. The
// worker re-writes the marker on every batch; detected_at must record when the
// contradiction was FIRST known, not when it was last re-seen.
func TestContradictionReport_DetectedAtIsFirstFlag(t *testing.T) {
	eng, ws, idA, idB := seedContradictionPair(t)
	ctx := context.Background()

	if _, err := eng.Store().FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("flag: %v", err)
	}
	first, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	firstAt := first.Pairs[0].DetectedAt

	time.Sleep(5 * time.Millisecond)
	// Re-flag in the opposite argument order — the worker canonicalises, so this
	// must hit the same marker.
	newly, err := eng.Store().FlagContradiction(ctx, ws, idB, idA)
	if err != nil {
		t.Fatalf("reflag: %v", err)
	}
	if newly {
		t.Errorf("second flag reported newlyFlagged=true; the penalty idempotency guard depends on this being false")
	}
	second, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("report 2: %v", err)
	}
	if !second.Pairs[0].DetectedAt.Equal(firstAt) {
		t.Errorf("DetectedAt moved on re-flag: %v -> %v", firstAt, second.Pairs[0].DetectedAt)
	}
}

// TestContradictionReport_DetectionSupersedesPending proves a pair does not
// appear twice once the detector catches up with the declared link.
func TestContradictionReport_DetectionSupersedesPending(t *testing.T) {
	eng, ws, idA, idB := seedContradictionPair(t)
	ctx := context.Background()

	if _, err := eng.Link(ctx, &mbp.LinkRequest{
		Vault:    "test",
		SourceID: idB.String(),
		TargetID: idA.String(),
		RelType:  uint16(storage.RelContradicts),
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := eng.Store().FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("flag: %v", err)
	}

	got, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got.Pairs) != 1 {
		t.Fatalf("Pairs = %d, want 1 (no duplicate declared/detected entry)", len(got.Pairs))
	}
	if got.Pairs[0].Status != ContradictionDetected {
		t.Errorf("Status = %q, want %q", got.Pairs[0].Status, ContradictionDetected)
	}
	if got.PendingCount != 0 {
		t.Errorf("PendingCount = %d, want 0", got.PendingCount)
	}
	// The declared link's own timestamp survives alongside the detection time.
	if got.Pairs[0].DeclaredAt.IsZero() {
		t.Errorf("DeclaredAt is zero for a pair that was explicitly linked")
	}
}
