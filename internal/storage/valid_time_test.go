package storage

import (
	"context"
	"testing"
	"time"
)

// TestValidTime_PredicateSemantics pins the shared half-open [ValidFrom,
// ValidUntil) predicate every surface must use (COG-19 mitigation: one
// predicate, not inlined comparisons).
func TestValidTime_PredicateSemantics(t *testing.T) {
	base := time.Unix(0, 1700000000000000000).UTC()
	from := base
	until := base.Add(time.Hour)

	eng := &Engram{CreatedAt: base.Add(-time.Minute), ValidFrom: from, ValidUntil: until}

	if eng.ValidAt(from.Add(-time.Nanosecond)) {
		t.Error("ValidAt just before ValidFrom = true, want false")
	}
	if !eng.ValidAt(from) {
		t.Error("ValidAt(ValidFrom) = false, want true (closed lower bound)")
	}
	if !eng.ValidAt(until.Add(-time.Nanosecond)) {
		t.Error("ValidAt just before ValidUntil = false, want true")
	}
	if eng.ValidAt(until) {
		t.Error("ValidAt(ValidUntil) = true, want false (half-open upper bound)")
	}

	if eng.IsExpired(until.Add(-time.Nanosecond)) {
		t.Error("IsExpired before ValidUntil = true, want false")
	}
	if !eng.IsExpired(until) {
		t.Error("IsExpired at ValidUntil = false, want true (ValidUntil <= now)")
	}

	// Legacy defaults: zero ValidFrom falls back to CreatedAt, zero ValidUntil is open.
	legacy := &Engram{CreatedAt: base}
	if legacy.ValidAt(base.Add(-time.Second)) {
		t.Error("legacy ValidAt before CreatedAt = true, want false")
	}
	if !legacy.ValidAt(base.Add(100 * 24 * time.Hour)) {
		t.Error("legacy ValidAt far future = false, want true (open window)")
	}
	if legacy.IsExpired(base.Add(100 * 24 * time.Hour)) {
		t.Error("legacy IsExpired = true, want false (open window never expires)")
	}

	// Future ValidFrom: not valid yet, but NOT expired — default recall keeps it.
	future := &Engram{CreatedAt: base, ValidFrom: base.Add(time.Hour)}
	if future.ValidAt(base) {
		t.Error("future-ValidFrom ValidAt(now) = true, want false")
	}
	if future.IsExpired(base) {
		t.Error("future-ValidFrom IsExpired = true, want false")
	}
}

// TestValidTime_StoreRoundTrip verifies validity fields survive
// WriteEngram → GetEngram and appear in GetMetadata.
func TestValidTime_StoreRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("validtime-roundtrip")

	created := time.Unix(0, 1700000000000000000).UTC()
	from := created.Add(-2 * time.Hour)
	until := created.Add(2 * time.Hour)
	eng := &Engram{
		Concept:    "validity store roundtrip",
		Content:    "content",
		CreatedAt:  created,
		ValidFrom:  from,
		ValidUntil: until,
		Importance: 0.9,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if !got.ValidFrom.Equal(from) {
		t.Errorf("GetEngram ValidFrom = %v, want %v", got.ValidFrom, from)
	}
	if !got.ValidUntil.Equal(until) {
		t.Errorf("GetEngram ValidUntil = %v, want %v", got.ValidUntil, until)
	}
	if got.Importance != 0.9 {
		t.Errorf("GetEngram Importance = %v, want 0.9", got.Importance)
	}

	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || len(metas) == 0 || metas[0] == nil {
		t.Fatalf("GetMetadata: %v (metas=%v)", err, metas)
	}
	if !metas[0].ValidFrom.Equal(from) {
		t.Errorf("GetMetadata ValidFrom = %v, want %v", metas[0].ValidFrom, from)
	}
	if !metas[0].ValidUntil.Equal(until) {
		t.Errorf("GetMetadata ValidUntil = %v, want %v", metas[0].ValidUntil, until)
	}
}

// TestStampValidUntil verifies the stamp primitive: closes an open window,
// respects onlyIfOpen against an already-closed window, and clears with the
// zero time (the restore path).
func TestStampValidUntil(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("validtime-stamp")

	id := writeTestEngram(t, store, ws, "stamp target", "stamp content")

	until := time.Now().Add(-time.Minute).UTC().Truncate(time.Nanosecond)
	stamped, err := store.StampValidUntil(ctx, ws, id, until, true)
	if err != nil {
		t.Fatalf("StampValidUntil: %v", err)
	}
	if !stamped {
		t.Fatal("StampValidUntil on open window: stamped = false, want true")
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if !got.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil = %v, want %v", got.ValidUntil, until)
	}
	// The record must NOT be deleted — invalidation is a stamp (COG-19).
	if got.State == StateSoftDeleted {
		t.Error("StampValidUntil soft-deleted the engram — invalidation must never delete")
	}

	// onlyIfOpen: a second stamp against the closed window is skipped.
	later := until.Add(time.Hour)
	stamped, err = store.StampValidUntil(ctx, ws, id, later, true)
	if err != nil {
		t.Fatalf("StampValidUntil (closed, onlyIfOpen): %v", err)
	}
	if stamped {
		t.Error("StampValidUntil onlyIfOpen on closed window: stamped = true, want false")
	}
	got, _ = store.GetEngram(ctx, ws, id)
	if !got.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil after skipped stamp = %v, want unchanged %v", got.ValidUntil, until)
	}

	// Overwrite (onlyIfOpen=false): explicit caller assertion wins.
	stamped, err = store.StampValidUntil(ctx, ws, id, later, false)
	if err != nil || !stamped {
		t.Fatalf("StampValidUntil overwrite: stamped=%v err=%v", stamped, err)
	}

	// Clear with the zero time — re-opens the window (restore).
	stamped, err = store.StampValidUntil(ctx, ws, id, time.Time{}, false)
	if err != nil || !stamped {
		t.Fatalf("StampValidUntil clear: stamped=%v err=%v", stamped, err)
	}
	got, _ = store.GetEngram(ctx, ws, id)
	if !got.ValidUntil.IsZero() {
		t.Errorf("ValidUntil after clear = %v, want zero (open)", got.ValidUntil)
	}
}

// TestUpdateMetadata_PreservesValidityStamp pins that the metadata
// read-modify-write path (decay, CAS, restore-meta) does not clobber a
// validity stamp: UpdateMetadata patches in place and must leave bytes 72-91 alone.
func TestUpdateMetadata_PreservesValidityStamp(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("validtime-updatemeta")

	id := writeTestEngram(t, store, ws, "meta preserve", "meta preserve content")
	until := time.Now().Add(time.Hour).UTC()
	if _, err := store.StampValidUntil(ctx, ws, id, until, true); err != nil {
		t.Fatalf("StampValidUntil: %v", err)
	}

	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || len(metas) == 0 || metas[0] == nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	updated := *metas[0]
	updated.Confidence = 0.42
	if err := store.UpdateMetadata(ctx, ws, id, &updated); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Confidence != 0.42 {
		t.Errorf("Confidence = %v, want 0.42", got.Confidence)
	}
	if !got.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil clobbered by UpdateMetadata: got %v, want %v", got.ValidUntil, until)
	}
}

// TestBatchSupersedeEngram verifies the atomic evolve-side primitive:
// soft-delete + ValidUntil stamp in one batched re-encode, and that an
// already-closed window is preserved (evolve of an already-expired fact
// must not destroy the earlier window end).
func TestBatchSupersedeEngram(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("validtime-supersede")

	id := writeTestEngram(t, store, ws, "supersede target", "supersede content")

	until := time.Now().UTC()
	batch := store.NewBatch()
	if err := batch.SupersedeEngram(ctx, ws, id, until); err != nil {
		t.Fatalf("SupersedeEngram: %v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	batch.Discard()

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.State != StateSoftDeleted {
		t.Errorf("State = %v, want StateSoftDeleted", got.State)
	}
	if !got.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil = %v, want %v", got.ValidUntil, until)
	}

	// Already-closed window: supersede keeps the earlier stamp.
	id2 := writeTestEngram(t, store, ws, "supersede closed", "supersede closed content")
	firstUntil := until.Add(-24 * time.Hour)
	if _, err := store.StampValidUntil(ctx, ws, id2, firstUntil, true); err != nil {
		t.Fatalf("StampValidUntil: %v", err)
	}
	batch2 := store.NewBatch()
	if err := batch2.SupersedeEngram(ctx, ws, id2, until); err != nil {
		t.Fatalf("SupersedeEngram (closed): %v", err)
	}
	if err := batch2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	batch2.Discard()
	got2, _ := store.GetEngram(ctx, ws, id2)
	if !got2.ValidUntil.Equal(firstUntil) {
		t.Errorf("ValidUntil = %v, want earlier stamp %v preserved", got2.ValidUntil, firstUntil)
	}
	if got2.State != StateSoftDeleted {
		t.Errorf("State = %v, want StateSoftDeleted", got2.State)
	}
}
