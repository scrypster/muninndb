package storage

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestCloneVaultData_NeverAccessedFollowsWriteEngramConvention pins that a
// cloned engram's "never accessed in this vault" state is spelled the SAME way
// every other writer in the product spells it: LastAccess == CreatedAt (the
// normalization WriteEngram/WriteEngramBatch/BatchWriter apply).
//
// Clone previously wrote time.Time{} straight through erf.Encode, which stores
// uint64(time.Time{}.UnixNano()) and decodes as 1754-08-30 — a value whose
// IsZero() is false, so every downstream guard waved it through (#810).
//
// This asserts on the RAW BYTES, not the decoded value, because the decode-side
// sentinel mapping (part 2 of #810) would otherwise mask a clone that still
// writes the garbage: decode would turn it into the zero time and this test
// would look green while the on-disk record was still wrong.
func TestCloneVaultData_NeverAccessedFollowsWriteEngramConvention(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("src-la-conv")
	wsTarget := store.VaultPrefix("dst-la-conv")

	created := time.Now().Add(-72 * time.Hour)
	accessed := time.Now().Add(-1 * time.Hour)
	id, err := store.WriteEngram(ctx, wsSource, &Engram{
		Concept:     "clone last-access convention",
		Content:     "body",
		AccessCount: 17,
		CreatedAt:   created,
		UpdatedAt:   created,
		LastAccess:  accessed,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if err := store.WriteVaultName(wsTarget, "dst-la-conv"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if _, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil); err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}

	raw, err := Get(store.db, keys.EngramKey(wsTarget, [16]byte(id)))
	if err != nil {
		t.Fatalf("get raw cloned engram: %v", err)
	}
	if raw == nil {
		t.Fatal("cloned engram not found in target vault")
	}

	rawLastAccess := int64(binary.BigEndian.Uint64(raw[erf.OffsetLastAccess : erf.OffsetLastAccess+8]))
	rawCreatedAt := int64(binary.BigEndian.Uint64(raw[erf.OffsetCreatedAt : erf.OffsetCreatedAt+8]))

	zeroTimeNanos := time.Time{}.UnixNano()
	if rawLastAccess == zeroTimeNanos {
		t.Fatalf("clone wrote the zero-time sentinel to LastAccess on disk (raw=%d, reads back as %v)",
			rawLastAccess, time.Unix(0, rawLastAccess).UTC())
	}
	if rawLastAccess != rawCreatedAt {
		t.Errorf("cloned LastAccess raw = %d (%v), want it to equal CreatedAt raw = %d (%v) — the product-wide \"never accessed\" convention",
			rawLastAccess, time.Unix(0, rawLastAccess).UTC(), rawCreatedAt, time.Unix(0, rawCreatedAt).UTC())
	}

	// And the decoded view agrees, with AccessCount still reset.
	got, err := store.GetEngram(ctx, wsTarget, id)
	if err != nil {
		t.Fatalf("GetEngram target: %v", err)
	}
	if got.AccessCount != 0 {
		t.Errorf("AccessCount = %d after clone, want 0", got.AccessCount)
	}
	if !got.LastAccess.Equal(got.CreatedAt) {
		t.Errorf("decoded LastAccess = %v, want == CreatedAt = %v", got.LastAccess.UTC(), got.CreatedAt.UTC())
	}
	if got.LastAccess.Year() < 2000 {
		t.Errorf("decoded LastAccess = %v — a pre-2000 year is the #810 signature", got.LastAccess.UTC())
	}
	// The source's own access metadata must be untouched.
	src, err := store.GetEngram(ctx, wsSource, id)
	if err != nil {
		t.Fatalf("GetEngram source: %v", err)
	}
	if src.AccessCount != 17 {
		t.Errorf("source AccessCount = %d, want 17 (clone must not mutate the source)", src.AccessCount)
	}
}

// TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal pins the honest scope
// of normalizeEngramTimes, which an earlier version of its doc comment got
// wrong by claiming "every writer of a 0x01 record must funnel through this".
//
// Six writers do not, and correctly so: mutateEngram (behind UpdateEngramState
// and SupersedeEngram), SoftDelete, UpdateTags, UpdateConfidence,
// UpdateConfidenceWithContradiction and UpdateDigest are read-modify-write. They
// create no NEW corruption — but they decode, mutate one field and re-encode, so
// a zero LastAccess that decode just repaired is written straight back out as
// the sentinel. They preserve; they do not heal.
//
// The consequence is load-bearing elsewhere and is why this is pinned rather
// than only commented: a vault cloned before #810 never self-heals through
// ordinary writes, so the #811 index-rebuild ordering trap noted at
// WriteLastAccessEntry stays live indefinitely instead of decaying away, and the
// decode-side repair can never be retired as "eventually unnecessary".
//
// If this test fails because an RMW path started normalizing, that is a fine
// thing to do — but update normalizeEngramTimes' doc, the #811 note and this
// test together, because three other statements depend on it.
func TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("rmw-sentinel")

	created := time.Now().Add(-48 * time.Hour)
	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:    "read-modify-write does not heal",
		Content:    "body",
		CreatedAt:  created,
		UpdatedAt:  created,
		LastAccess: created,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Put the record into the pre-#810 state: the sentinel on disk, in both the
	// 0x01 record and its 0x02 metadata slice, with a valid CRC32.
	key := keys.EngramKey(ws, [16]byte(id))
	raw, err := Get(store.db, key)
	if err != nil || raw == nil {
		t.Fatalf("get raw engram: %v", err)
	}
	sentinel := uint64(time.Time{}.UnixNano())
	binary.BigEndian.PutUint64(raw[erf.OffsetLastAccess:erf.OffsetLastAccess+8], sentinel)
	binary.BigEndian.PutUint32(raw[len(raw)-erf.TrailerSize:], erf.ComputeCRC32(raw[:len(raw)-erf.TrailerSize]))
	if err := store.db.Set(key, raw, pebble.Sync); err != nil {
		t.Fatalf("set patched 0x01: %v", err)
	}
	if err := store.db.Set(keys.MetaKey(ws, [16]byte(id)), erf.MetaKeySlice(raw), pebble.Sync); err != nil {
		t.Fatalf("set patched 0x02: %v", err)
	}

	// An ordinary write against that record.
	if err := store.SoftDelete(ctx, ws, id); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	after, err := Get(store.db, key)
	if err != nil || after == nil {
		t.Fatalf("get raw engram after SoftDelete: %v", err)
	}
	gotRaw := int64(binary.BigEndian.Uint64(after[erf.OffsetLastAccess : erf.OffsetLastAccess+8]))
	if gotRaw != int64(sentinel) {
		t.Fatalf("after SoftDelete the on-disk LastAccess raw = %d (%v), want the sentinel %d — an RMW writer "+
			"started healing timestamps; see this test's doc for the three statements that depend on it not doing so",
			gotRaw, time.Unix(0, gotRaw).UTC(), int64(sentinel))
	}
	t.Logf("PERPETUATED as designed: after SoftDelete the on-disk LastAccess is still the sentinel raw=%d (%v)",
		gotRaw, time.Unix(0, gotRaw).UTC())

	// The decode-side repair is what keeps that byte pattern harmless.
	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if !got.LastAccess.IsZero() {
		t.Errorf("decoded LastAccess = %v, want the zero time — the decode-side repair is the only thing "+
			"standing between this record and a 740,000-day age", got.LastAccess.UTC())
	}
}
