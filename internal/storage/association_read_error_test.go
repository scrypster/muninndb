package storage

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	seedEndpoints(t, store, ws, src, dst)
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

// failReadsForSource returns a readFault scoped to ONE pair's namespace: reads
// of forward-association keys under (ws, src) fail, everything else succeeds.
// Prefix-wide faults cannot express the case that matters for a multi-vault
// batch — one unreadable pair among readable ones.
func failReadsForSource(ws [8]byte, src ULID) func([]byte) error {
	scoped := make([]byte, 0, 25)
	scoped = append(scoped, prefix.AssocFwd)
	scoped = append(scoped, ws[:]...)
	scoped = append(scoped, src[:]...)
	return func(key []byte) error {
		if bytes.HasPrefix(key, scoped) {
			return errInjectedRead
		}
		return nil
	}
}

// TestUpdateAssocWeightBatch_OneUnreadablePairDoesNotStopTheRest pins the
// blast-radius decision for a permanently unreadable pair.
//
// processBatch aggregates up to 50 CoActivationEvents spanning MULTIPLE VAULTS
// into one AssocWeightUpdate slice. A corrupt Pebble block is a permanent,
// KEY-SCOPED fault on an otherwise fully writable DB — a point read of the bad
// key returns a checksum error while a subsequent batch.Commit succeeds — so a
// pair that fails to read fails to read on every flush window that contains it.
// And a frequently co-activated pair is in the index precisely BECAUSE it is
// frequently co-activated, so it recurs.
//
// Aborting the whole batch therefore converts one edge's unreadable metadata
// into a standing Hebbian-learning outage for every innocent vault sharing the
// flush window. The contract's "either all succeed or none do" is about the
// atomicity of what gets WRITTEN — nothing half-written, no fabricated tuple
// ever persisted — not a promise to write nothing when one INPUT is unreadable.
// So: skip the unreadable pair, apply the rest, and report loudly enough that
// the caller can tell which updates did not land (principle #2, degrade
// loudly-but-gracefully; a silent skip would be the same silently-wrong class
// one layer up).
func TestUpdateAssocWeightBatch_OneUnreadablePairDoesNotStopTheRest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsA := store.VaultPrefix("batch-partial-innocent-before")
	wsB := store.VaultPrefix("batch-partial-unreadable")
	wsC := store.VaultPrefix("batch-partial-innocent-after")

	aSrc, aDst := NewULID(), NewULID()
	bSrc, bDst := NewULID(), NewULID()
	cSrc, cDst := NewULID(), NewULID()
	seedEdge(t, store, wsA, aSrc, aDst, 0.5)
	bOrig := seedEdge(t, store, wsB, bSrc, bDst, 0.5)
	seedEdge(t, store, wsC, cSrc, cDst, 0.5)

	store.readFault = failReadsForSource(wsB, bSrc)

	err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: wsA, Src: [16]byte(aSrc), Dst: [16]byte(aDst), Weight: 0.42, CountDelta: 1},
		{WS: wsB, Src: [16]byte(bSrc), Dst: [16]byte(bDst), Weight: 0.30, CountDelta: 1},
		{WS: wsC, Src: [16]byte(cSrc), Dst: [16]byte(cDst), Weight: 0.37, CountDelta: 1},
	})

	store.readFault = nil

	// The failure is reported, not swallowed.
	if err == nil {
		t.Fatal("UpdateAssocWeightBatch: want an error naming the skipped pair, got nil — a silent skip is silently-wrong")
	}
	if !errors.Is(err, errInjectedRead) {
		t.Errorf("UpdateAssocWeightBatch: want the injected read failure wrapped, got %v", err)
	}
	// The error names the pair the caller lost, so an operator can act on it.
	if !strings.Contains(err.Error(), bSrc.String()) || !strings.Contains(err.Error(), bDst.String()) {
		t.Errorf("UpdateAssocWeightBatch error must name the skipped pair %s->%s, got: %v", bSrc, bDst, err)
	}

	// The innocent vaults' updates landed — both the one ordered BEFORE the bad
	// pair and the one ordered AFTER it.
	if w, gerr := store.GetAssocWeight(ctx, wsA, aSrc, aDst); gerr != nil || w < 0.419 || w > 0.421 {
		t.Errorf("innocent vault ordered BEFORE the unreadable pair: weight %v, err %v; want 0.42 — "+
			"one unreadable pair must not cost an unrelated vault its learning", w, gerr)
	}
	if w, gerr := store.GetAssocWeight(ctx, wsC, cSrc, cDst); gerr != nil || w < 0.369 || w > 0.371 {
		t.Errorf("innocent vault ordered AFTER the unreadable pair: weight %v, err %v; want 0.37", w, gerr)
	}

	// The unreadable pair is untouched: nothing fabricated was written over it.
	cur, gerr := Get(store.db, keys.AssocFwdKey(wsB, [16]byte(bSrc), 0.5, [16]byte(bDst)))
	if gerr != nil {
		t.Fatalf("re-read skipped pair: %v", gerr)
	}
	if !bytes.Equal(cur, bOrig) {
		t.Errorf("skipped pair rewritten: got %x, want %x", cur, bOrig)
	}
	if n := countLiveEdges(t, store, wsB, bSrc, bDst); n != 1 {
		t.Errorf("skipped pair live forward edges: got %d, want 1", n)
	}

	// The caller can identify exactly which updates did not land.
	var partial interface{ SkippedUpdates() []int }
	if !errors.As(err, &partial) {
		t.Fatalf("UpdateAssocWeightBatch: want an error exposing SkippedUpdates() []int, got %T: %v", err, err)
	}
	if got := partial.SkippedUpdates(); len(got) != 1 || got[0] != 1 {
		t.Errorf("SkippedUpdates: got %v, want [1] (the middle update)", got)
	}
}

// TestUpdateAssocWeightBatch_AllSkipped_AppendsNoReplicationEntry pins that a
// batch which wrote nothing replicates nothing.
//
// Newly reachable with this change: before it, every update was written
// unconditionally, so an all-skipped batch could not occur. replicateBatch's
// own `len(repr) == 0` guard does not catch it — an empty Pebble batch's Repr()
// is still a 12-byte header, not zero bytes — so a zero-op OpBatch would be
// shipped to every follower each flush window. Harmless to apply, but it is log
// volume and a divergence-investigation red herring for a batch that did
// nothing.
func TestUpdateAssocWeightBatch_AllSkipped_AppendsNoReplicationEntry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("all-skipped-replication")

	src, dst := NewULID(), NewULID()
	seedEdge(t, store, ws, src, dst, 0.5)

	var appends int
	store.repLogAppend = func(uint8, []byte, []byte) error {
		appends++
		return nil
	}

	// Fail the 0x14 weight-index read so the only update in the batch is skipped.
	store.readFault = failReadsWithPrefix(prefix.AssocWeightIndex)

	err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: ws, Src: [16]byte(src), Dst: [16]byte(dst), Weight: 0.7, CountDelta: 1},
	})
	if err == nil {
		t.Fatal("UpdateAssocWeightBatch: want the skip reported, got nil")
	}
	var skipErr *AssocBatchSkipError
	if !errors.As(err, &skipErr) || len(skipErr.Skipped) != 1 || skipErr.Applied != 0 {
		t.Fatalf("want every update skipped and none applied, got %v", err)
	}

	if appends != 0 {
		t.Errorf("a batch that applied 0 updates appended %d replication entr(ies) — an empty "+
			"Pebble batch's Repr() is a 12-byte header, so the len(repr)==0 guard does not fire", appends)
	}
}
