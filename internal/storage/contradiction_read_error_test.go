package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ---------------------------------------------------------------------------
// #804 — the 0x0A idempotency token must not be fabricated from a failed read.
//
// The token is what makes the contradiction confidence penalty fire ONCE. A
// failed read previously skipped the `err == nil` branch, so the pair was
// reported NEWLY flagged and the marker was restamped with `now`. Re-penalising
// compounds a Bayesian update on BOTH engrams (1.0 -> 0.975 -> 0.797 -> 0.313
// -> 0.0709) and BayesianUpdate is not invertible.
// ---------------------------------------------------------------------------

// seedContradictionMarker writes the canonical 0x0A pair directly with a fixed
// detection time, so a restamp is detectable without depending on clock
// resolution. Returns the canonical (low, high) ULIDs.
func seedContradictionMarker(t *testing.T, store *PebbleStore, ws [8]byte, a, b ULID, detectedAt time.Time) (ULID, ULID) {
	t.Helper()
	if CompareULIDs(a, b) > 0 {
		a, b = b, a
	}
	aB, bB := [16]byte(a), [16]byte(b)
	if err := store.db.Set(keys.ContradictionKey(ws, 0, 0, aB), encodeContradictionValue(bB, detectedAt), pebble.Sync); err != nil {
		t.Fatalf("seed 0x0A a: %v", err)
	}
	if err := store.db.Set(keys.ContradictionKey(ws, 0, 0, bB), encodeContradictionValue(aB, detectedAt), pebble.Sync); err != nil {
		t.Fatalf("seed 0x0A b: %v", err)
	}
	return a, b
}

// markerDetectedAt reads the stored detection time for the canonical marker.
func markerDetectedAt(t *testing.T, store *PebbleStore, ws [8]byte, a ULID) time.Time {
	t.Helper()
	val, err := Get(store.db, keys.ContradictionKey(ws, 0, 0, [16]byte(a)))
	if err != nil {
		t.Fatalf("read 0x0A: %v", err)
	}
	if val == nil {
		t.Fatal("0x0A marker missing")
	}
	_, detectedAt, ok := decodeContradictionValue(val)
	if !ok {
		t.Fatal("0x0A value too short to decode")
	}
	return detectedAt
}

// TestFlagContradiction_ReadFaultIsNotLaunderedIntoTheToken pins the fail-CLOSED
// policy: an unreadable token surfaces as an error and writes nothing, rather
// than becoming "newly flagged" with a fresh detectedAt.
func TestFlagContradiction_ReadFaultIsNotLaunderedIntoTheToken(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-flag-read-fault")

	detected := time.Unix(1700000000, 0)
	a, b := seedContradictionMarker(t, store, ws, NewULID(), NewULID(), detected)

	// Healthy control: the pair is already known, so the penalty is suppressed
	// and the original stamp survives.
	newly, err := store.FlagContradiction(ctx, ws, a, b)
	if err != nil {
		t.Fatalf("healthy FlagContradiction: %v", err)
	}
	if newly {
		t.Fatal("fixture broken: a re-flag of a seeded marker reported newlyFlagged=true")
	}
	if got := markerDetectedAt(t, store, ws, a); !got.Equal(detected) {
		t.Fatalf("fixture broken: healthy re-flag restamped detectedAt to %v", got)
	}

	// Now the token read fails and nothing else does.
	store.readFault = failReadsWithPrefix(prefix.Contradiction)
	newly, err = store.FlagContradiction(ctx, ws, a, b)
	store.readFault = nil

	if newly {
		t.Error("a failed token read reported the pair as NEWLY flagged — the confidence penalty would fire again on an already-known contradiction")
	}
	if err == nil {
		t.Error("a failed token read returned a nil error, so the caller cannot tell newness was never established")
	}
	if got := markerDetectedAt(t, store, ws, a); !got.Equal(detected) {
		t.Errorf("detectedAt was restamped over a live marker under a read fault: got %v, want %v", got, detected)
	}
}

// TestUpdateConfidenceWithContradiction_ReadFaultDoesNotRestampTheMarker is the
// same laundering one file over (engram.go), and lands on the OPPOSITE side of
// the fail-open/fail-closed line: the confidence delta is the caller's primary
// request and refusing the whole batch would drop it. So the confidence write
// proceeds and only the marker rewrite is skipped.
func TestUpdateConfidenceWithContradiction_ReadFaultDoesNotRestampTheMarker(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-conf-read-fault")

	id := NewULID()
	other := NewULID()
	if _, err := store.WriteEngram(ctx, ws, &Engram{ID: id, Concept: "claim", Content: "the ledger balances", Confidence: 1.0}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	detected := time.Unix(1700000000, 0)
	canonLow, _ := seedContradictionMarker(t, store, ws, id, other, detected)

	store.readFault = failReadsWithPrefix(prefix.Contradiction)
	prior, newConf, err := store.UpdateConfidenceWithContradiction(ctx, ws, id, -0.1, other, true)
	store.readFault = nil

	if err != nil {
		t.Fatalf("UpdateConfidenceWithContradiction must not refuse the confidence write on a 0x0A read fault: %v", err)
	}
	if prior != 1.0 || newConf > 0.91 || newConf < 0.89 {
		t.Errorf("confidence delta was not applied: prior=%v new=%v", prior, newConf)
	}
	if got := markerDetectedAt(t, store, ws, canonLow); !got.Equal(detected) {
		t.Errorf("detectedAt was restamped over a live marker under a read fault: got %v, want %v", got, detected)
	}
}
