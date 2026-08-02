package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
)

// INDEPENDENTLY DERIVED legacy key bytes. The pre-fix WeightComplement computed
// uint32(1.0 * float32(MaxUint32)); float32(MaxUint32) rounds up to 2^32, the
// out-of-range uint32 conversion produced 0, and MaxUint32-0 is 0xFFFFFFFF.
// Nothing below calls keys.WeightComplement — a fixture derived from the code
// under test proves only that the code agrees with itself, which is exactly the
// trap the #757 review caught.
var testLegacyComplement = [4]byte{0xFF, 0xFF, 0xFF, 0xFF}

// testCorrectFullWeightComplement is the position a POST-fix 1.0 edge occupies:
// MaxUint32 - MaxUint32 = 0. Also hardcoded, for the same reason.
var testCorrectFullWeightComplement = [4]byte{0x00, 0x00, 0x00, 0x00}

func rawAssocKey(pfx byte, ws [8]byte, first [16]byte, complement [4]byte, second [16]byte) []byte {
	key := make([]byte, 45)
	key[0] = pfx
	copy(key[1:9], ws[:])
	copy(key[9:25], first[:])
	copy(key[25:29], complement[:])
	copy(key[29:45], second[:])
	return key
}

func rawWeightIndexKey(ws [8]byte, a, b [16]byte) []byte {
	key := make([]byte, 41)
	key[0] = prefix.AssocWeightIndex
	copy(key[1:9], ws[:])
	copy(key[9:25], a[:])
	copy(key[25:41], b[:])
	return key
}

// seedLegacyEdge lays down the exact on-disk shape a PRE-FIX weight-1.0 write
// left behind: 0x03/0x04 keys at the legacy complement, and a 0x14 index
// carrying the true weight (the index always stored raw float bits correctly).
//
// Both endpoints get a real 0x01 record: flushChunk re-validates them (STO-12),
// because a legacy row whose endpoint is gone must be skipped rather than
// relocated into a live, correctly-positioned dangling edge. A fixture without
// engrams would exercise the skip path on every case and prove nothing about
// the relocation this file is testing. TestSTO12_LegacyFullWeightRepairNever-
// CreatesADanglingEdge covers the endpoint-less shape deliberately.
func seedLegacyEdge(t *testing.T, store *PebbleStore, ws [8]byte, src, dst [16]byte, val []byte, indexWeight float32) {
	t.Helper()
	seedEndpoints(t, store, ws, ULID(src), ULID(dst))
	batch := store.db.NewBatch()
	defer batch.Close()
	_ = batch.Set(rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst), val, nil)
	_ = batch.Set(rawAssocKey(prefix.AssocRev, ws, dst, testLegacyComplement, src), val, nil)
	var wi [4]byte
	binary.BigEndian.PutUint32(wi[:], math.Float32bits(indexWeight))
	_ = batch.Set(rawWeightIndexKey(ws, src, dst), wi[:], nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		t.Fatalf("seed legacy edge: %v", err)
	}
}

func mustGet(t *testing.T, store *PebbleStore, key []byte) ([]byte, bool) {
	t.Helper()
	val, closer, err := store.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	out := append([]byte(nil), val...)
	_ = closer.Close()
	return out, true
}

// Test 1: the canonical damage. A pre-fix full-weight edge with a MODERN value
// (peakWeight 1.0 — the dominant case per the #756 correction, the one decay
// clamps rather than deletes) must end up at the true 1.0 position with its
// value bytes carried verbatim, the legacy position gone, and the 0x14 index
// untouched.
//
// RED without the repair: the fwd/rev keys stay at the legacy position and the
// edge keeps reading as weight 0.
func TestRepairLegacyFullWeightAssocKeys_RelocatesModernValueEdge(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x21}

	src := [16]byte{1, 2, 3}
	dst := [16]byte{4, 5, 6}
	createdAt := time.Unix(1_700_000_000, 0)
	val := encodeAssocValue(RelSupports, 0.875, createdAt, 12345, 1.0, 7)

	seedLegacyEdge(t, store, ws, src, dst, val[:], 1.0)

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}

	// Legacy position is gone, in BOTH directions.
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst)); ok {
		t.Error("legacy fwd key survived the repair")
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocRev, ws, dst, testLegacyComplement, src)); ok {
		t.Error("legacy rev key survived the repair")
	}

	// Exactly one fwd/rev pair, at the true 1.0 position, bytes verbatim.
	gotFwd, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst))
	if !ok {
		t.Fatal("no fwd key at the true 1.0 position")
	}
	if !bytes.Equal(gotFwd, val[:]) {
		t.Errorf("fwd value bytes changed: got %x want %x", gotFwd, val[:])
	}
	gotRev, ok := mustGet(t, store, rawAssocKey(prefix.AssocRev, ws, dst, testCorrectFullWeightComplement, src))
	if !ok {
		t.Fatal("no rev key at the true 1.0 position")
	}
	if !bytes.Equal(gotRev, val[:]) {
		t.Errorf("rev value bytes changed: got %x want %x", gotRev, val[:])
	}

	// Decoded metadata survives intact — relType/confidence/createdAt/peak.
	relType, conf, gotCreated, lastAct, peak, coAct, _ := decodeAssocValue(gotFwd)
	if relType != RelSupports || conf != 0.875 || peak != 1.0 || coAct != 7 || lastAct != 12345 {
		t.Errorf("metadata drifted: relType=%v conf=%v peak=%v coAct=%v lastAct=%v", relType, conf, peak, coAct, lastAct)
	}
	if !gotCreated.Equal(createdAt) {
		t.Errorf("createdAt drifted: got %v want %v", gotCreated, createdAt)
	}

	// The 0x14 index was already correct and must NOT have been rewritten.
	wi, ok := mustGet(t, store, rawWeightIndexKey(ws, src, dst))
	if !ok {
		t.Fatal("weight index deleted by the repair")
	}
	if got := math.Float32frombits(binary.BigEndian.Uint32(wi)); got != 1.0 {
		t.Errorf("weight index = %v, want 1.0 untouched", got)
	}

	// The relocated edge reads at full weight through the normal read path.
	assocs, err := store.GetAssociations(ctx, ws, []ULID{ULID(src)}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(assocs[ULID(src)]) != 1 {
		t.Fatalf("pair has %d edges, want exactly 1", len(assocs[ULID(src)]))
	}
	if w := assocs[ULID(src)][0].Weight; w != 1.0 {
		t.Errorf("edge reads weight %v after repair, want 1.0", w)
	}

	// Idempotent: a second pass finds nothing.
	again, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if again != 0 {
		t.Errorf("second pass repaired %d, want 0", again)
	}
}

// Test 3: the LEGACY-VALUE era (18-byte value whose peakWeight decodes as 0 —
// the case decay deletes outright). The repair must carry the value verbatim
// and relocate identically; it reads the 0x14 index, never the value, so the
// value era is irrelevant to the decision.
func TestRepairLegacyFullWeightAssocKeys_RelocatesLegacyValueEdge(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x22}

	src := [16]byte{9, 9, 9}
	dst := [16]byte{8, 8, 8}

	// 18-byte era value: relType | confidence | createdAt | lastActivated.
	// Non-zero so it is not mistaken for the all-zero pre-fix blank.
	val := make([]byte, 18)
	binary.BigEndian.PutUint16(val[0:2], uint16(RelRelatesTo))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(1.0))
	binary.BigEndian.PutUint64(val[6:14], uint64(time.Unix(1_600_000_000, 0).UnixNano()))
	binary.BigEndian.PutUint32(val[14:18], uint32(999))

	seedLegacyEdge(t, store, ws, src, dst, val, 1.0)

	// Precondition: this era decodes peakWeight 0 — the shape that makes decay
	// delete the edge outright rather than clamp it.
	if _, _, _, _, peak, _, _ := decodeAssocValue(val); peak != 0 {
		t.Fatalf("fixture premise void: 18-byte value decodes peak %v, want 0", peak)
	}

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}

	got, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst))
	if !ok {
		t.Fatal("no fwd key at the true 1.0 position")
	}
	if !bytes.Equal(got, val) {
		t.Errorf("value bytes not carried verbatim: got %x want %x", got, val)
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst)); ok {
		t.Error("legacy fwd key survived the repair")
	}
}

// Test 2: a GENUINE weight-0-ish edge sits at the same key position and must be
// left completely alone. This is the false-positive guard: the whole repair
// rests on the 0x14 index being the disambiguator, so an index that is not
// exactly 1.0 must veto.
func TestRepairLegacyFullWeightAssocKeys_LeavesGenuineZeroWeightEdge(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x23}

	// (a) index reads exactly 0 — a decayed-to-nothing edge.
	srcZero := [16]byte{0x10}
	dstZero := [16]byte{0x11}
	valZero := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 0.9, 3)
	seedLegacyEdge(t, store, ws, srcZero, dstZero, valZero[:], 0)

	// (b) index reads a small but non-zero value — an honestly decayed edge
	// whose key position happens to round to the same complement.
	srcSmall := [16]byte{0x20}
	dstSmall := [16]byte{0x21}
	valSmall := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 0.9, 3)
	seedLegacyEdge(t, store, ws, srcSmall, dstSmall, valSmall[:], 0.05)

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired = %d, want 0 — the repair moved an edge it had no evidence for", repaired)
	}

	for _, c := range []struct {
		name     string
		src, dst [16]byte
	}{{"index-zero", srcZero, dstZero}, {"index-0.05", srcSmall, dstSmall}} {
		if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, c.src, testLegacyComplement, c.dst)); !ok {
			t.Errorf("%s: fwd key was removed", c.name)
		}
		if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocRev, ws, c.dst, testLegacyComplement, c.src)); !ok {
			t.Errorf("%s: rev key was removed", c.name)
		}
		if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, c.src, testCorrectFullWeightComplement, c.dst)); ok {
			t.Errorf("%s: repair invented a key at the 1.0 position", c.name)
		}
	}
}

// Test 5: a MULTI-KEY pair. WriteAssociation performs no cleanup on rewrite, so
// one pair can hold several 0x03 keys at different weights. Here the pair holds
// a key at the legacy position AND a key at 0.7, with the index reading 0.7 —
// the index is the pair's single source of truth and it does not say 1.0, so
// the repair must leave EVERYTHING alone rather than assume the legacy key is
// the pair's only one.
func TestRepairLegacyFullWeightAssocKeys_MultiKeyPairIndexNotFullWeight(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x24}

	src := [16]byte{0x31}
	dst := [16]byte{0x32}
	legacyVal := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 1.0, 2)
	seedLegacyEdge(t, store, ws, src, dst, legacyVal[:], 0.7)

	// A second, correctly-placed key for the SAME pair at 0.7. Complement
	// derived independently: MaxUint32 - uint32(0.7 * 2^32) = 0x4CCCCCFF.
	weight07 := [4]byte{0x4C, 0xCC, 0xCC, 0xFF}
	val07 := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_100, 0), 2, 1.0, 3)
	batch := store.db.NewBatch()
	_ = batch.Set(rawAssocKey(prefix.AssocFwd, ws, src, weight07, dst), val07[:], nil)
	_ = batch.Set(rawAssocKey(prefix.AssocRev, ws, dst, weight07, src), val07[:], nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		t.Fatalf("seed 0.7 key: %v", err)
	}
	_ = batch.Close()

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired = %d, want 0 — the index reads 0.7, not 1.0", repaired)
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst)); !ok {
		t.Error("legacy-position key was removed on a pair whose index is not 1.0")
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, weight07, dst)); !ok {
		t.Error("the pair's 0.7 key was removed")
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst)); ok {
		t.Error("repair invented a 1.0-position key")
	}
}

// A multi-key pair where the index DOES read 1.0 and a correctly-placed 1.0 key
// already exists. The correctly-placed key is newer (only the post-fix encoder
// can write it), so the repair must delete the legacy position WITHOUT clobbering
// the survivor's value bytes.
func TestRepairLegacyFullWeightAssocKeys_KeepsExistingCorrectPositionValue(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x25}

	src := [16]byte{0x41}
	dst := [16]byte{0x42}
	stale := encodeAssocValue(RelSupports, 0.5, time.Unix(1_600_000_000, 0), 1, 1.0, 1)
	fresh := encodeAssocValue(RelSupports, 0.9, time.Unix(1_700_000_000, 0), 9, 1.0, 42)
	seedLegacyEdge(t, store, ws, src, dst, stale[:], 1.0)

	batch := store.db.NewBatch()
	_ = batch.Set(rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst), fresh[:], nil)
	_ = batch.Set(rawAssocKey(prefix.AssocRev, ws, dst, testCorrectFullWeightComplement, src), fresh[:], nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		t.Fatalf("seed correct-position key: %v", err)
	}
	_ = batch.Close()

	if _, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws); err != nil {
		t.Fatalf("repair: %v", err)
	}

	got, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst))
	if !ok {
		t.Fatal("correct-position key was deleted")
	}
	if !bytes.Equal(got, fresh[:]) {
		t.Errorf("repair clobbered the newer correctly-placed value with the legacy one")
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst)); ok {
		t.Error("legacy duplicate survived")
	}
}

// The watermark round-trips and defaults to 0 for a vault that has never been
// swept — the read a boot uses to decide whether to scan at all.
func TestAssocWeightRepairMark_RoundTrip(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x26}

	mark, err := store.GetAssocWeightRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("get mark: %v", err)
	}
	if mark != 0 {
		t.Fatalf("unswept vault reports mark %d, want 0", mark)
	}
	if err := store.SetAssocWeightRepairMark(ctx, ws, 1); err != nil {
		t.Fatalf("set mark: %v", err)
	}
	mark, err = store.GetAssocWeightRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("get mark: %v", err)
	}
	if mark != 1 {
		t.Errorf("mark = %d, want 1", mark)
	}
}

// The repair is key-level idempotent: re-applying its exact writes over
// already-repaired state converges to one edge rather than duplicating or
// erroring. That property is what makes a purely LOCAL per-node repair sound
// (the #681 precedent) — nothing is shipped to followers, so every node must
// reach the same state by running the same deterministic pass. It also guards a
// re-scan of a vault whose 0x2E watermark was cleared or version-bumped.
func TestRepairLegacyFullWeightAssocKeys_DoubleApplyIsANoOp(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x27}

	src := [16]byte{0x51}
	dst := [16]byte{0x52}
	val := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 1.0, 1)
	seedLegacyEdge(t, store, ws, src, dst, val[:], 1.0)

	if _, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws); err != nil {
		t.Fatalf("first repair: %v", err)
	}
	// Re-apply the repair's exact batch shape by hand: delete the
	// (already-absent) legacy keys and re-set the (already-present) correct ones.
	batch := store.db.NewBatch()
	_ = batch.Delete(rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst), nil)
	_ = batch.Delete(rawAssocKey(prefix.AssocRev, ws, dst, testLegacyComplement, src), nil)
	_ = batch.Set(rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst), val[:], nil)
	_ = batch.Set(rawAssocKey(prefix.AssocRev, ws, dst, testCorrectFullWeightComplement, src), val[:], nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		t.Fatalf("replay repair batch: %v", err)
	}
	_ = batch.Close()

	assocs, err := store.GetAssociations(ctx, ws, []ULID{ULID(src)}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if n := len(assocs[ULID(src)]); n != 1 {
		t.Errorf("pair has %d edges after double-apply, want exactly 1", n)
	}
}

// countFwdKeysForSrc counts every 0x03 key the vault holds for src, regardless
// of weight position — the direct measure of "is there a duplicate edge", which
// GetAssociations would hide by returning only the first (highest-weight) one.
func countFwdKeysForSrc(t *testing.T, store *PebbleStore, ws [8]byte, src [16]byte) [][]byte {
	t.Helper()
	scanPrefix := make([]byte, 25)
	scanPrefix[0] = prefix.AssocFwd
	copy(scanPrefix[1:9], ws[:])
	copy(scanPrefix[9:25], src[:])
	iter, err := PrefixIterator(store.db, scanPrefix)
	if err != nil {
		t.Fatalf("prefix iterator: %v", err)
	}
	defer iter.Close()
	var found [][]byte
	for iter.First(); iter.Valid(); iter.Next() {
		found = append(found, append([]byte(nil), iter.Key()...))
	}
	return found
}

// B1 (review finding): a live write that supersedes a captured pair between the
// scan's snapshot and the chunk commit must NOT be undone by the repair.
//
// The interleaving: the scan captures the damaged pair from its fixed snapshot.
// Before the chunk commits, Hebbian UpdateAssocWeight runs for real — it reads
// 1.0 from the 0x14 index, deletes the correct AND the legacy key positions, and
// rewrites the pair at 0.8, index 0.8. The repair must notice (the legacy key is
// gone) and emit nothing at all for that pair.
//
// RED against the pre-fix logic, which re-validated only `keyExists(fwdCorrect)`:
// the 1.0 position is empty after the live write, so it wrote the STALE captured
// value back at 1.0 — a permanent phantom full-weight duplicate that sorts first
// in GetAssociations while the index says 0.8, and that nothing ever deletes.
//
// The interleaving is forced through the pre-flush hook rather than by racing a
// multi-second scan against a timer: same window, deterministic, sub-millisecond.
func TestRepairLegacyFullWeightAssocKeys_DoesNotResurrectSupersededPair(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x28}

	src := [16]byte{0x61}
	dst := [16]byte{0x62}
	// STO-12: the concurrent writer below is UpdateAssocWeight, which now
	// refuses a pair whose endpoints have no 0x01 record. Give the pair real
	// engrams so the window this test forces is the repair-vs-live-write one it
	// is about, not an endpoint refusal.
	seedEndpoints(t, store, ws, ULID(src), ULID(dst))
	stale := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 1.0, 1)
	seedLegacyEdge(t, store, ws, src, dst, stale[:], 1.0)

	// The live write lands in the capture→commit window, exactly once.
	fired := false
	assocWeightRepairPreFlushHook = func() {
		if fired {
			return
		}
		fired = true
		if err := store.UpdateAssocWeight(ctx, ws, ULID(src), ULID(dst), 0.8, 1); err != nil {
			t.Errorf("concurrent UpdateAssocWeight: %v", err)
		}
	}
	defer func() { assocWeightRepairPreFlushHook = nil }()

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !fired {
		t.Fatal("pre-flush hook never fired — the test did not exercise the window")
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 — the pair was superseded, nothing was relocated", repaired)
	}

	fwdKeys := countFwdKeysForSrc(t, store, ws, src)
	if len(fwdKeys) != 1 {
		t.Fatalf("pair holds %d forward keys, want exactly 1 (the live 0.8 edge); a second key is the resurrected phantom", len(fwdKeys))
	}
	if bytes.Equal(fwdKeys[0][25:29], testCorrectFullWeightComplement[:]) {
		t.Error("the surviving key sits at the 1.0 position — the repair resurrected the superseded full-weight edge")
	}
	w, err := store.GetAssocWeight(ctx, ws, ULID(src), ULID(dst))
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if w != 0.8 {
		t.Errorf("index weight = %v, want 0.8 (the live write's value)", w)
	}
}

// flushChunk's `indexed != 1.0` RE-READ had no discriminating test: deleting it
// outright left the whole internal/storage suite green.
//
// Only the SCAN loop's copy of that check was covered, and the two are not the
// same check. The scan's runs against a fixed iterator snapshot; the flush-time
// one exists solely to catch a weight change that lands in the capture→commit
// window. The nearest existing test
// (TestRepairLegacyFullWeightAssocKeys_DoesNotResurrectSupersededPair) drives
// the same hook but changes the legacy key's PRESENCE, so it exercises the
// `!present` branch above and returns before the index is ever re-read.
//
// This is the window the re-read is for: the legacy key is still there (so the
// pair still looks repairable) but the 0x14 index no longer says exactly 1.0.
// A concurrent decay or Hebbian update that rewrites the index without moving
// the key produces exactly that state, and the disambiguator is gone — the pair
// is no longer provably a pre-fix full-weight edge, so the repair must not move
// it.
//
// Deterministic via the pre-flush hook, not a timing race.
func TestRepairLegacyFullWeightAssocKeys_ReReadsTheIndexAtFlushTime(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x2A}

	src := [16]byte{0x81}
	dst := [16]byte{0x82}
	val := encodeAssocValue(RelSupports, 1.0, time.Unix(1_700_000_000, 0), 1, 1.0, 1)
	seedLegacyEdge(t, store, ws, src, dst, val[:], 1.0)

	fired := false
	assocWeightRepairPreFlushHook = func() {
		if fired {
			return
		}
		fired = true
		// Rewrite ONLY the 0x14 index. The legacy 0x03/0x04 keys stay exactly
		// where they are, so the `!present` short-circuit does not fire and the
		// index re-read is the only thing that can refuse this pair.
		var wi [4]byte
		binary.BigEndian.PutUint32(wi[:], math.Float32bits(0.55))
		if err := store.db.Set(rawWeightIndexKey(ws, src, dst), wi[:], pebble.NoSync); err != nil {
			t.Errorf("in-window index rewrite: %v", err)
		}
	}
	defer func() { assocWeightRepairPreFlushHook = nil }()

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !fired {
		t.Fatal("pre-flush hook never fired — the test did not exercise the window")
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 — the pair's index stopped being exactly 1.0 inside the "+
			"capture→commit window, so it is not provably a pre-fix full-weight edge", repaired)
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst)); ok {
		t.Error("the repair relocated a pair whose 0x14 index no longer said 1.0 at flush time — " +
			"the flush-time re-read did not run")
	}
	if _, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst)); !ok {
		t.Error("the repair deleted the legacy position of a pair it refused to move")
	}
}

// B1, second half: when the pair is NOT superseded but its legacy-position value
// bytes are rewritten in place inside the window, the repair must carry the
// FRESH bytes to the 1.0 position. Committing the captured copy would silently
// roll back whatever that write recorded.
//
// RED against the pre-fix logic, which wrote the snapshot's `d.val`.
func TestRepairLegacyFullWeightAssocKeys_CarriesFreshlyReadValueBytes(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{0x29}

	src := [16]byte{0x71}
	dst := [16]byte{0x72}
	captured := encodeAssocValue(RelSupports, 0.5, time.Unix(1_600_000_000, 0), 1, 1.0, 1)
	fresher := encodeAssocValue(RelSupports, 0.9, time.Unix(1_700_000_000, 0), 99, 1.0, 77)
	seedLegacyEdge(t, store, ws, src, dst, captured[:], 1.0)

	fired := false
	assocWeightRepairPreFlushHook = func() {
		if fired {
			return
		}
		fired = true
		// In-place rewrite at the legacy position; weight (and so key position
		// and 0x14 index) is unchanged, so the pair is still repairable.
		if err := store.db.Set(rawAssocKey(prefix.AssocFwd, ws, src, testLegacyComplement, dst), fresher[:], pebble.NoSync); err != nil {
			t.Errorf("in-window rewrite: %v", err)
		}
	}
	defer func() { assocWeightRepairPreFlushHook = nil }()

	repaired, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !fired {
		t.Fatal("pre-flush hook never fired — the test did not exercise the window")
	}
	if repaired != 1 {
		t.Errorf("repaired = %d, want 1", repaired)
	}
	got, ok := mustGet(t, store, rawAssocKey(prefix.AssocFwd, ws, src, testCorrectFullWeightComplement, dst))
	if !ok {
		t.Fatal("repair did not place the edge at the true 1.0 position")
	}
	if !bytes.Equal(got, fresher[:]) {
		t.Error("repair carried the captured snapshot value, clobbering the in-window rewrite")
	}
}
