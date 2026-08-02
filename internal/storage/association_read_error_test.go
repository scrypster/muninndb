package storage

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// errInjectedRead is the synthetic Pebble read failure used by these tests.
var errInjectedRead = errors.New("injected pebble read failure")

// failReadsWithPrefix returns a readFault that fails only reads whose key
// starts with the given prefix byte. Failing ONE namespace is the point: when
// the 0x14 weight index still reads fine but the 0x03 metadata read fails, the
// write path believes it knows the old weight (so it deletes the old keys) and
// believes the edge has no metadata (so it writes defaults) — the maximum-damage
// interleaving, and the one a whole-DB outage would never isolate.
func failReadsWithPrefix(b byte) func([]byte) error {
	return func(key []byte) error {
		if len(key) > 0 && key[0] == b {
			return errInjectedRead
		}
		return nil
	}
}

// seedEdge writes a directional edge with distinctive metadata and returns the
// raw forward-key value bytes so a test can prove they were not rewritten.
func seedEdge(t *testing.T, store *PebbleStore, ws [8]byte, src, dst ULID, weight float32) []byte {
	t.Helper()
	ctx := context.Background()
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        weight,
		RelType:       RelSupersedes,
		Confidence:    0.9,
		CreatedAt:     time.Now().Add(-72 * time.Hour),
		LastActivated: int32(time.Now().Add(-24 * time.Hour).Unix()),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	val, err := Get(store.db, keys.AssocFwdKey(ws, [16]byte(src), weight, [16]byte(dst)))
	if err != nil {
		t.Fatalf("read seeded fwd key: %v", err)
	}
	if val == nil {
		t.Fatal("seeded fwd key missing")
	}
	return val
}

// liveEdge returns the decoded metadata of whatever forward key currently holds
// the pair, scanning the 0x03 namespace directly so it sees a rewrite at a NEW
// weight position (which the 0x14-index path would happily follow).
func liveEdge(t *testing.T, store *PebbleStore, ws [8]byte, src, dst ULID) (relType RelType, peak float32, createdAt time.Time, found bool) {
	t.Helper()
	scan := make([]byte, 0, 25)
	scan = append(scan, prefix.AssocFwd)
	scan = append(scan, ws[:]...)
	scan = append(scan, src[:]...)
	iter, err := PrefixIterator(store.db, scan)
	if err != nil {
		t.Fatalf("prefix iterator: %v", err)
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 45 || !bytes.Equal(k[29:45], dst[:]) {
			continue
		}
		rt, _, ca, _, pw, _, _ := decodeAssocValue(iter.Value())
		return rt, pw, ca, true
	}
	return 0, 0, time.Time{}, false
}

// TestUpdateAssocWeightBatch_ReadFailureDoesNotOverwriteEdge is the primary RED
// test for the laundering defect. A transient failure of the 0x03 metadata read
// must NOT cause the batch to re-encode a live edge from fabricated zero values:
// relType would be reclassified from a directional relation to RelType 0 (the
// Hebbian co-activation type), and peakWeight — the anchor of the COG-27 decay
// ceiling (ceiling = peakWeight * 2^(-dt/halfLife)) — would collapse to the new
// weight, permanently lowering the edge's ceiling on the next decay pass.
func TestUpdateAssocWeightBatch_ReadFailureDoesNotOverwriteEdge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-read-fail-batch")

	src, dst := NewULID(), NewULID()
	const seedWeight = float32(0.9)
	origVal := seedEdge(t, store, ws, src, dst, seedWeight)

	store.readFault = failReadsWithPrefix(prefix.AssocFwd)

	err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS:         ws,
		Src:        [16]byte(src),
		Dst:        [16]byte(dst),
		Weight:     0.2,
		CountDelta: 1,
	}})
	if err == nil {
		t.Error("UpdateAssocWeightBatch: want error when the metadata read fails, got nil")
	} else if !errors.Is(err, errInjectedRead) {
		t.Errorf("UpdateAssocWeightBatch: want wrapped injected read failure, got %v", err)
	}

	store.readFault = nil

	relType, peak, createdAt, found := liveEdge(t, store, ws, src, dst)
	if !found {
		t.Fatal("edge vanished: the failed read destroyed it")
	}
	if relType != RelSupersedes {
		t.Errorf("relType: got %v, want %v (RelSupersedes) — a failed read reclassified a directional edge", relType, RelSupersedes)
	}
	if peak < seedWeight-0.001 {
		t.Errorf("peakWeight: got %v, want >= %v — the COG-27 decay ceiling anchor was reset by a failed read", peak, seedWeight)
	}
	if createdAt.IsZero() {
		t.Error("createdAt: got zero time, want the seeded creation time")
	}

	// Byte-level: nothing about the stored edge may have changed at all.
	cur, gerr := Get(store.db, keys.AssocFwdKey(ws, [16]byte(src), seedWeight, [16]byte(dst)))
	if gerr != nil {
		t.Fatalf("re-read fwd key: %v", gerr)
	}
	if !bytes.Equal(cur, origVal) {
		t.Errorf("forward value rewritten on a failed read: got %x, want %x", cur, origVal)
	}
}

// TestUpdateAssocWeight_ReadFailureDoesNotOverwriteEdge is the single-pair
// analogue: the same laundering lives in UpdateAssocWeight.
func TestUpdateAssocWeight_ReadFailureDoesNotOverwriteEdge(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-read-fail-single")

	src, dst := NewULID(), NewULID()
	const seedWeight = float32(0.85)
	origVal := seedEdge(t, store, ws, src, dst, seedWeight)

	store.readFault = failReadsWithPrefix(prefix.AssocFwd)

	err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.15, 1)
	if err == nil {
		t.Error("UpdateAssocWeight: want error when the metadata read fails, got nil")
	} else if !errors.Is(err, errInjectedRead) {
		t.Errorf("UpdateAssocWeight: want wrapped injected read failure, got %v", err)
	}

	store.readFault = nil

	relType, peak, _, found := liveEdge(t, store, ws, src, dst)
	if !found {
		t.Fatal("edge vanished: the failed read destroyed it")
	}
	if relType != RelSupersedes {
		t.Errorf("relType: got %v, want %v (RelSupersedes)", relType, RelSupersedes)
	}
	if peak < seedWeight-0.001 {
		t.Errorf("peakWeight: got %v, want >= %v", peak, seedWeight)
	}
	cur, gerr := Get(store.db, keys.AssocFwdKey(ws, [16]byte(src), seedWeight, [16]byte(dst)))
	if gerr != nil {
		t.Fatalf("re-read fwd key: %v", gerr)
	}
	if !bytes.Equal(cur, origVal) {
		t.Errorf("forward value rewritten on a failed read: got %x, want %x", cur, origVal)
	}
}

// TestGetAssocWeight_ReadErrorPropagates pins the weight-index half of the same
// class. Weight 0 means "no such edge" to every caller: the Hebbian worker
// re-seeds a cold-start 0.01 over a strong edge, and dream consolidation infers
// a fresh A→C over one that already exists. Absence and failure must differ.
func TestGetAssocWeight_ReadErrorPropagates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-read-fail-weight")

	src, dst := NewULID(), NewULID()
	seedEdge(t, store, ws, src, dst, 0.7)

	// Absence still reports 0 with no error.
	absent, err := store.GetAssocWeight(ctx, ws, src, NewULID())
	if err != nil {
		t.Fatalf("GetAssocWeight (absent): unexpected error %v", err)
	}
	if absent != 0 {
		t.Errorf("GetAssocWeight (absent): got %v, want 0", absent)
	}

	store.readFault = failReadsWithPrefix(prefix.AssocWeightIndex)
	defer func() { store.readFault = nil }()

	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err == nil {
		t.Fatalf("GetAssocWeight: want error on a failed read, got weight %v and nil error", w)
	}
	if !errors.Is(err, errInjectedRead) {
		t.Errorf("GetAssocWeight: want wrapped injected read failure, got %v", err)
	}
}

// TestGetAssocValueFull_ReadErrorPropagates pins the metadata reader itself.
func TestGetAssocValueFull_ReadErrorPropagates(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("assoc-read-fail-full")

	src, dst := NewULID(), NewULID()
	seedEdge(t, store, ws, src, dst, 0.6)

	// Absence: no edge, no error.
	_, _, _, _, _, _, _, err := store.getAssocValueFull(context.Background(), ws, src, NewULID())
	if err != nil {
		t.Fatalf("getAssocValueFull (absent): unexpected error %v", err)
	}

	store.readFault = failReadsWithPrefix(prefix.AssocFwd)
	defer func() { store.readFault = nil }()

	relType, _, _, _, peak, _, _, err := store.getAssocValueFull(context.Background(), ws, src, dst)
	if err == nil {
		t.Fatalf("getAssocValueFull: want error on a failed read, got relType=%v peak=%v and nil error", relType, peak)
	}
	if !errors.Is(err, errInjectedRead) {
		t.Errorf("getAssocValueFull: want wrapped injected read failure, got %v", err)
	}
}

// TestReadOrdinal_ReadErrorPropagates pins the same shape in ReadOrdinal, which
// reported found=false — indistinguishable from "no ordinal" — on a read error.
func TestReadOrdinal_ReadErrorPropagates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-read-fail")

	parent, child := NewULID(), NewULID()
	if err := store.WriteOrdinal(ctx, ws, parent, child, 7); err != nil {
		t.Fatalf("WriteOrdinal: %v", err)
	}

	// Corrupt the stored record to a short value: still "absent", not an error.
	key := keys.OrdinalKey(ws, [16]byte(parent), [16]byte(child))
	if err := store.db.Set(key, []byte{0x01}, nil); err != nil {
		t.Fatalf("Set short ordinal: %v", err)
	}
	if _, found, err := store.ReadOrdinal(ctx, ws, parent, child); err != nil || found {
		t.Errorf("ReadOrdinal (short record): got found=%v err=%v, want found=false err=nil", found, err)
	}

	store.readFault = failReadsWithPrefix(prefix.Ordinal)
	defer func() { store.readFault = nil }()

	ord, found, err := store.ReadOrdinal(ctx, ws, parent, child)
	if err == nil {
		t.Fatalf("ReadOrdinal: want error on a failed read, got ordinal=%v found=%v nil error", ord, found)
	}
	if !errors.Is(err, errInjectedRead) {
		t.Errorf("ReadOrdinal: want wrapped injected read failure, got %v", err)
	}
}

// TestAssocReadFaultSeamIsInert asserts the test-only seam is inert when nil,
// so it can never change production behaviour.
func TestAssocReadFaultSeamIsInert(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("assoc-seam-inert")
	src, dst := NewULID(), NewULID()
	seedEdge(t, store, ws, src, dst, 0.5)

	if store.readFault != nil {
		t.Fatal("readFault must default to nil")
	}
	w, err := store.GetAssocWeight(context.Background(), ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if math.Abs(float64(w)-0.5) > 0.001 {
		t.Errorf("weight through the seam: got %v, want 0.5", w)
	}
}
