package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestFlagContradiction_RecordsDetectionTime pins that the 0x0A marker carries
// a durable detection timestamp. Before this, the value was the bare partner
// ULID and detected_at surfaced everywhere as the Go zero time.
func TestFlagContradiction_RecordsDetectionTime(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-detected-at")

	a, b := NewULID(), NewULID()
	before := time.Now().Add(-time.Second)
	if _, err := store.FlagContradiction(ctx, ws, a, b); err != nil {
		t.Fatalf("flag: %v", err)
	}
	after := time.Now().Add(time.Second)

	recs, err := store.GetContradictionRecords(ctx, ws)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].DetectedAt.IsZero() {
		t.Fatalf("DetectedAt is the zero time")
	}
	if recs[0].DetectedAt.Before(before) || recs[0].DetectedAt.After(after) {
		t.Errorf("DetectedAt = %v, want within [%v,%v]", recs[0].DetectedAt, before, after)
	}
}

// TestFlagContradiction_ReFlagKeepsFirstDetectionTime — the batch worker
// re-writes the marker on every observation. detected_at must record when the
// contradiction first became known.
func TestFlagContradiction_ReFlagKeepsFirstDetectionTime(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-first-flag")

	a, b := NewULID(), NewULID()
	newly, err := store.FlagContradiction(ctx, ws, a, b)
	if err != nil {
		t.Fatalf("flag: %v", err)
	}
	if !newly {
		t.Fatalf("first flag should report newlyFlagged=true")
	}
	recs, _ := store.GetContradictionRecords(ctx, ws)
	first := recs[0].DetectedAt

	time.Sleep(5 * time.Millisecond)
	newly, err = store.FlagContradiction(ctx, ws, b, a)
	if err != nil {
		t.Fatalf("reflag: %v", err)
	}
	if newly {
		t.Fatalf("second flag reported newlyFlagged=true — the confidence-penalty idempotency guard depends on false")
	}
	recs, _ = store.GetContradictionRecords(ctx, ws)
	if !recs[0].DetectedAt.Equal(first) {
		t.Errorf("DetectedAt moved on re-flag: %v -> %v", first, recs[0].DetectedAt)
	}
}

// TestGetContradictionRecords_LegacyValue proves a marker written before the
// value carried a timestamp (bare 16-byte partner ULID) still yields the pair,
// with an UNKNOWN detection time rather than a fabricated one.
func TestGetContradictionRecords_LegacyValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-legacy")

	a, b := NewULID(), NewULID()
	if CompareULIDs(a, b) > 0 {
		a, b = b, a
	}
	if err := store.db.Set(keys.ContradictionKey(ws, 0, 0, [16]byte(a)), b[:], nil); err != nil {
		t.Fatalf("legacy set: %v", err)
	}
	if err := store.db.Set(keys.ContradictionKey(ws, 0, 0, [16]byte(b)), a[:], nil); err != nil {
		t.Fatalf("legacy set rev: %v", err)
	}

	recs, err := store.GetContradictionRecords(ctx, ws)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if !recs[0].DetectedAt.IsZero() {
		t.Errorf("legacy marker must report an unknown detection time, got %v", recs[0].DetectedAt)
	}
	// The legacy pair must still be visible through the old accessor.
	pairs, err := store.GetContradictions(ctx, ws)
	if err != nil {
		t.Fatalf("pairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("GetContradictions = %d, want 1", len(pairs))
	}
}

// TestDeclaredContradictions_FindsUnflaggedLinks is the storage half of the
// "pending is not none" fix: an explicit contradicts association is durable
// immediately, so the read path can report it as awaiting detection instead of
// returning an empty list.
func TestDeclaredContradictions_FindsUnflaggedLinks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-declared")

	a, b, c := NewULID(), NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b, c)
	created := time.Now().Truncate(time.Millisecond)

	// One contradicts edge and one ordinary edge — only the first counts.
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, RelType: RelContradicts, Weight: 0.9, Confidence: 1, CreatedAt: created,
	}); err != nil {
		t.Fatalf("write contradicts: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, a, c, &Association{
		TargetID: c, RelType: RelSupports, Weight: 0.9, Confidence: 1, CreatedAt: created,
	}); err != nil {
		t.Fatalf("write supports: %v", err)
	}

	res, err := store.DeclaredContradictions(ctx, ws, 0)
	if err != nil {
		t.Fatalf("declared: %v", err)
	}
	if !res.Complete {
		t.Errorf("Complete = false on a 2-edge vault")
	}
	if len(res.Records) != 1 {
		t.Fatalf("declared = %d, want 1 (only the contradicts edge)", len(res.Records))
	}
	wantA, wantB := a, b
	if CompareULIDs(wantA, wantB) > 0 {
		wantA, wantB = wantB, wantA
	}
	if res.Records[0].A != wantA || res.Records[0].B != wantB {
		t.Errorf("pair = (%s,%s), want (%s,%s)", res.Records[0].A, res.Records[0].B, wantA, wantB)
	}
	if !res.Records[0].DeclaredAt.Equal(created) {
		t.Errorf("DeclaredAt = %v, want %v", res.Records[0].DeclaredAt, created)
	}
}

// TestDeclaredContradictions_DedupesBothDirections — an agent that links A→B
// and B→A has declared ONE contradiction, not two.
func TestDeclaredContradictions_DedupesBothDirections(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-declared-dupe")

	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b)
	early := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	late := time.Now().Truncate(time.Millisecond)
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, RelType: RelContradicts, Weight: 0.9, Confidence: 1, CreatedAt: late,
	}); err != nil {
		t.Fatalf("write ab: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, b, a, &Association{
		TargetID: a, RelType: RelContradicts, Weight: 0.9, Confidence: 1, CreatedAt: early,
	}); err != nil {
		t.Fatalf("write ba: %v", err)
	}

	res, err := store.DeclaredContradictions(ctx, ws, 0)
	if err != nil {
		t.Fatalf("declared: %v", err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("declared = %d, want 1", len(res.Records))
	}
	if !res.Records[0].DeclaredAt.Equal(early) {
		t.Errorf("DeclaredAt = %v, want the EARLIER declaration %v", res.Records[0].DeclaredAt, early)
	}
}

// TestDeclaredContradictions_ScanCapIsReported — when the scan is truncated the
// caller must be told, so "no pending contradictions" is never asserted from a
// partial scan.
func TestDeclaredContradictions_ScanCapIsReported(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-declared-cap")

	src := NewULID()
	seedEndpoints(t, store, ws, src)
	for i := 0; i < 5; i++ {
		dst := NewULID()
		seedEndpoints(t, store, ws, dst)
		if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
			TargetID: dst, RelType: RelSupports, Weight: float32(i+1) / 10, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	res, err := store.DeclaredContradictions(ctx, ws, 2)
	if err != nil {
		t.Fatalf("declared: %v", err)
	}
	if res.Complete {
		t.Errorf("Complete = true despite a maxScan of 2 over 5 edges")
	}
	if res.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", res.Scanned)
	}
}

// BenchmarkDeclaredContradictions measures the forward-association scan that
// finds declared-but-undetected pairs. Association values carry the relation
// type, so this is O(edges) with no way to prefix-filter; the benchmark exists
// so the cost of muninn_contradictions is measured rather than assumed.
func BenchmarkDeclaredContradictions(b *testing.B) {
	dir := b.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	b.Cleanup(func() { store.Close() })

	ctx := context.Background()
	ws := store.VaultPrefix("bench-declared")
	const edges = 100_000
	for i := 0; i < edges; i++ {
		src, dst := NewULID(), NewULID()
		rel := RelSupports
		if i%1000 == 0 {
			rel = RelContradicts
		}
		if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
			TargetID: dst, RelType: rel, Weight: 0.5, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := store.DeclaredContradictions(ctx, ws, 0)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Records) == 0 {
			b.Fatal("expected declared pairs")
		}
	}
}

// vaultEndingIn0xFF derives, deterministically, a vault name whose 8-byte
// SipHash prefix ends in 0xFF. Roughly one vault name in 256 does.
func vaultEndingIn0xFF(t *testing.T) string {
	t.Helper()
	for i := 0; i < 100_000; i++ {
		name := fmt.Sprintf("ff-vault-%d", i)
		ws := keys.VaultPrefix(name)
		if ws[7] == 0xFF {
			return name
		}
	}
	t.Fatal("no vault name with a 0xFF-terminated prefix in 100k candidates")
	return ""
}

// TestContradictionScans_VaultPrefixEndingIn0xFF is the RED for the naive
// last-byte-increment upper bound. When a vault's 8-byte prefix ends in 0xFF,
// `upper[len-1]++` wraps to 0x00, producing an upper bound BELOW the lower
// bound: every scan returns empty, silently, forever. For contradictions that
// means COG-29 honesty is disabled for ~1/256 of all vaults — a silently-wrong
// answer, the project's worst failure class.
func TestContradictionScans_VaultPrefixEndingIn0xFF(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	name := vaultEndingIn0xFF(t)
	ws := store.VaultPrefix(name)
	if ws[7] != 0xFF {
		t.Fatalf("precondition: ws = %x does not end in 0xFF", ws)
	}

	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b)
	if _, err := store.FlagContradiction(ctx, ws, a, b); err != nil {
		t.Fatalf("flag: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, RelType: RelContradicts, Weight: 0.5, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write assoc: %v", err)
	}

	has, err := store.HasContradictionMarkers(ctx, ws)
	if err != nil {
		t.Fatalf("HasContradictionMarkers: %v", err)
	}
	if !has {
		t.Error("HasContradictionMarkers = false on a vault with a marker written — the recall-side honesty gate is permanently off for this vault")
	}

	recs, err := store.GetContradictionRecords(ctx, ws)
	if err != nil {
		t.Fatalf("GetContradictionRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("GetContradictionRecords = %d records, want 1", len(recs))
	}

	decl, err := store.DeclaredContradictions(ctx, ws, 0)
	if err != nil {
		t.Fatalf("DeclaredContradictions: %v", err)
	}
	if len(decl.Records) != 1 {
		t.Errorf("DeclaredContradictions = %d records, want 1", len(decl.Records))
	}
}
