package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Activate's phase 4.75 lazy archive restore had NO gate on the resolved
// actReq.ReadOnly (COG-11's single resolved decision). A read_only:true
// Activate could therefore mint a live 0x03/0x04/0x14 association row out of
// an archived (0x25) edge, contradicting COG-11's "a read must not mutate
// learning state" text verbatim.
//
// This reproduces the defect: an archived edge between two tag-linked
// engrams, a ReadOnly Activate that fuses the source engram as a candidate,
// and an assertion that the live weight index stays at zero across the
// read_only call — restoring only on a subsequent NON-read-only Activate.
//
// PRIVACY: vault/concept/content strings below are synthetic, authored here.
// ---------------------------------------------------------------------------

func TestActivate_ReadOnly_DoesNotRestoreArchivedEdge(t *testing.T) {
	eng, store, db := danglingEnv(t)
	ctx := context.Background()
	const vault = "readonly-archive-restore-probe"
	const sharedTag = "quarterly-notes"

	w := danglingWriter(t, eng, vault, sharedTag)
	target := w("archive rotation policy", "rotate the planning archive every quarter")
	source := w("seed neighbour", "planning notes that keep the tag alive")
	_, _ = target, source

	ws := store.VaultPrefix(vault)

	// Force the live edge into the 0x25 archive (mirrors the STO-12 dangling
	// test's own archiving setup).
	if _, err := store.DecayAssocWeights(ctx, ws, time.Nanosecond, 0.001, 0.05); err != nil {
		t.Fatalf("archiving decay: %v", err)
	}
	// Direction-agnostic check: the live weight-index (0x14) must be empty
	// once the edge is archived, and the archive namespace (0x25) non-empty.
	if n := countPrefix(t, db, ws, 0x14); n != 0 {
		t.Fatalf("precondition: edge is not archived, %d live weight-index row(s) remain", n)
	}
	if n := countPrefix(t, db, ws, 0x25); n == 0 {
		t.Fatal("precondition: no edge reached the 0x25 archive")
	}

	// A read_only:true Activate whose context matches BOTH engrams, so the
	// archived edge's endpoints are fused as candidates and phase 4.75 sees
	// them.
	readOnlyReq := &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"planning notes archive rotation quarterly"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   true,
	}
	if _, err := eng.Activate(withMode(auth.ModeFull), readOnlyReq); err != nil {
		t.Fatalf("read-only Activate: %v", err)
	}
	eng.WaitWriteTimeIdle()

	if n := countPrefix(t, db, ws, 0x14); n != 0 {
		t.Fatalf("COG-11 violated: a read_only:true Activate restored an archived "+
			"edge — %d live weight-index row(s) now present; phase 4.75 must gate "+
			"on the resolved actReq.ReadOnly", n)
	}

	// A NON-read-only Activate over the identical query is the correct place
	// for the lazy restore to fire.
	liveReq := *readOnlyReq
	liveReq.ReadOnly = false
	if _, err := eng.Activate(withMode(auth.ModeFull), &liveReq); err != nil {
		t.Fatalf("live Activate: %v", err)
	}
	eng.WaitWriteTimeIdle()

	if n := countPrefix(t, db, ws, 0x14); n == 0 {
		t.Fatal("a subsequent non-read-only Activate should have lazily restored the archived edge, no live weight-index rows present")
	}
}
