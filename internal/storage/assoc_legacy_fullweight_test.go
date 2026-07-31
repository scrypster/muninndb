package storage

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Pre-fix full-weight edges live at the WRONG key position, and an update must
// not leave them behind as duplicates.
//
// The old WeightComplement overflowed for weight exactly 1.0 (float32(MaxUint32)
// rounds up to 2^32; the uint32 conversion of 2^32 is implementation-defined
// and produced 0 on arm64), so a 1.0-weight association was written at the
// byte position of weight 0.0 and read back as weight 0. After the encode fix,
// a weight update recomputes the CORRECT old-key position, whose delete would
// miss the legacy location entirely — leaving a stale duplicate edge that
// still reads as 0. deleteLegacyFullWeightKeys closes that.
func TestUpdateAssocWeight_CleansLegacyFullWeightKey(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{7}

	a := writeConfPathAssocEngram(t, store, ws, "edge source")
	b := writeConfPathAssocEngram(t, store, ws, "edge target")

	// Simulate the PRE-FIX on-disk state by hand: the 0x14 weight index holds
	// the true 1.0 (it always stored raw float bits, correctly), while the
	// 0x03/0x04 keys sit at the weight-0.0 position (the overflow artifact).
	if err := store.UpdateAssocWeight(ctx, ws, a, b, 1.0, 1); err != nil {
		t.Fatalf("seed UpdateAssocWeight: %v", err)
	}
	// Move the fwd/rev keys to the legacy (buggy) position to recreate the
	// pre-fix layout: read the correctly-placed value, rewrite at weight-0.0
	// position, delete the correct position.
	fwdCorrect := keys.AssocFwdKey(ws, [16]byte(a), 1.0, [16]byte(b))
	val, closer, err := store.db.Get(fwdCorrect)
	if err != nil {
		t.Fatalf("read seeded fwd key: %v", err)
	}
	valCopy := append([]byte(nil), val...)
	_ = closer.Close()
	batch := store.db.NewBatch()
	_ = batch.Set(keys.AssocFwdKey(ws, [16]byte(a), 0.0, [16]byte(b)), valCopy, nil)
	_ = batch.Set(keys.AssocRevKey(ws, [16]byte(b), 0.0, [16]byte(a)), valCopy, nil)
	_ = batch.Delete(fwdCorrect, nil)
	_ = batch.Delete(keys.AssocRevKey(ws, [16]byte(b), 1.0, [16]byte(a)), nil)
	if err := batch.Commit(nil); err != nil {
		t.Fatalf("simulate legacy layout: %v", err)
	}

	// A post-fix weight update on the pair. The 0x14 index says oldWeight=1.0,
	// so the delete recomputes the CORRECT 1.0 position — the compat helper
	// must also clear the legacy 0.0 position or the stale edge survives.
	if err := store.UpdateAssocWeight(ctx, ws, a, b, 0.7, 1); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	assocs, err := store.GetAssociations(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	edges := assocs[a]
	count := 0
	for _, e := range edges {
		if e.TargetID == b {
			count++
			if e.Weight == 0 {
				t.Errorf("stale legacy full-weight key survived the update (edge reads weight 0)")
			}
		}
	}
	if count != 1 {
		t.Errorf("pair has %d edges after update, want exactly 1 — the legacy key position was not cleaned", count)
	}
}

func writeConfPathAssocEngram(t *testing.T, store *PebbleStore, ws [8]byte, content string) ULID {
	t.Helper()
	id, err := store.WriteEngram(context.Background(), ws, &Engram{
		Concept: "legacy-weight fixture", Content: content, State: StateActive, Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	return id
}

// The cross-era survival scenario from the adversarial review, pinned at the
// store level: a PRE-FIX interior-weight edge (0.8 — written with the legacy
// encoder, which is byte-identical to the fixed one on (0,1)) must survive one
// weight update with its metadata intact and exactly one key. The reviewer
// proved that the first (float64) version of the encoder fix failed this
// end-to-end: the recomputed delete missed the pre-fix key, the metadata read
// missed too, and the update wrote a duplicate edge with relType silently
// reset to 0.
func TestUpdateAssocWeight_PreFixInteriorWeightEdgeSurvives(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := [8]byte{8}

	a := writeConfPathAssocEngram(t, store, ws, "interior edge source")
	b := writeConfPathAssocEngram(t, store, ws, "interior edge target")

	// Written with the current encoder, THEN relocated to independently
	// hardcoded legacy bytes. The Tier-3 second pass proved the naive version
	// of this test (write + update, both with the current encoder) passes even
	// under the float64 byte-compat regression — writer and deleter are
	// self-consistent, so nothing is ever missed. Standing alone requires the
	// on-disk key to sit at bytes this test derives WITHOUT calling
	// WeightComplement: the legacy complement of 0.8 is
	// 0xFFFFFFFF - uint32(0.8 * 2^32) = 858993407 = 0x3333_32FF.
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, RelType: RelSupports, Weight: 0.8, Confidence: 1.0, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	// Independently-derived legacy bytes for 0.8 (no WeightComplement call).
	legacy08 := [4]byte{0x33, 0x33, 0x32, 0xFF}
	// The current encoder must place the key at exactly these bytes; if it
	// does not, the write above is already at a non-legacy position and this
	// test's premise (a PRE-FIX edge) is void — fail loudly rather than pass
	// vacuously.
	fwdKey := keys.AssocFwdKey(ws, [16]byte(a), 0.8, [16]byte(b))
	if got := [4]byte(fwdKey[25:29]); got != legacy08 {
		t.Fatalf("current encoder places weight 0.8 at complement %x, legacy bytes are %x — "+
			"byte-compatibility is broken and every assertion below would be self-consistent "+
			"rather than cross-era", got, legacy08)
	}

	if err := store.UpdateAssocWeight(ctx, ws, a, b, 0.85, 1); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	assocs, err := store.GetAssociations(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	count := 0
	for _, e := range assocs[a] {
		if e.TargetID == b {
			count++
			if e.RelType != RelSupports {
				t.Errorf("relType = %v after update, want RelSupports — metadata was reset, meaning "+
					"the recomputed key missed the stored one", e.RelType)
			}
		}
	}
	if count != 1 {
		t.Errorf("pair has %d edges after one update, want exactly 1 — the old key was not deleted "+
			"(encoder byte-compatibility broken)", count)
	}
}
