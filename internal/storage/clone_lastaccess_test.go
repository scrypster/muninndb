package storage

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

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
