package replication

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/storage/migrate"
)

// legacyIdempotencyValue is a byte-for-byte stand-in for what
// storage.WriteIdempotency puts at keys.IdempotencyKey(opID).
func legacyIdempotencyValue(t *testing.T, engramID string) []byte {
	t.Helper()
	v, err := json.Marshal(struct {
		EngramID  string `json:"engram_id"`
		CreatedAt int64  `json:"created_at"`
	}{EngramID: engramID, CreatedAt: 1})
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	return v
}

// TestReplicationKeys_NeverCollideWithIdempotency is the #726 invariant
// (STO-14): no key this package writes may live under prefix.Idempotency, and
// the log-entry sub-range must contain nothing but log entries.
//
// Before #726 every one of these keys began with a raw 0x19 —
// prefix.Idempotency — and the entry keys were `0x19|seq_be64(8)`, exactly the
// shape of `0x19|siphash(op_id)(8)`.
func TestReplicationKeys_NeverCollideWithIdempotency(t *testing.T) {
	all := map[string][]byte{
		"entry(1)":       replicationEntryKey(1),
		"entry(max)":     replicationEntryKey(^uint64(0)),
		"seqCounter":     seqCounterKey(),
		"lastApplied":    lastAppliedKey(),
		"schemaVersion":  schemaVersionKey(),
		"clusterEpoch":   clusterEpochKey(),
		"nodeRole":       nodeRoleKey(),
		"snapComplete":   snapCompleteKey,
		"entryRangeLow":  entryRangeLower(),
		"entryRangeHigh": entryRangeUpper(),
	}
	for name, k := range all {
		if len(k) == 0 {
			t.Fatalf("%s: empty key", name)
		}
		if k[0] == prefix.Idempotency {
			t.Errorf("%s = % x starts with prefix.Idempotency (0x%02X) — #726 collision",
				name, k, prefix.Idempotency)
		}
		if name != "entryRangeHigh" && k[0] != prefix.Replication {
			t.Errorf("%s = % x does not start with prefix.Replication (0x%02X)",
				name, k, prefix.Replication)
		}
	}

	// Every metadata key must sort OUTSIDE the entry range, so a prune whose
	// bounds are entry keys can never reach one.
	lo, hi := entryRangeLower(), entryRangeUpper()
	for _, name := range []string{"seqCounter", "lastApplied", "schemaVersion", "clusterEpoch", "nodeRole", "snapComplete"} {
		k := all[name]
		if bytes.Compare(k, lo) >= 0 && bytes.Compare(k, hi) < 0 {
			t.Errorf("metadata key %s = % x lies inside the entry range [% x, % x) — a Prune could delete it",
				name, k, lo, hi)
		}
	}

	// The entry range's upper bound must come from the shared helper, not a
	// hand-rolled increment (STO-11; three copies of that bug so far).
	if want := keys.PrefixUpperBound(lo); !bytes.Equal(hi, want) {
		t.Errorf("entryRangeUpper() = % x; want keys.PrefixUpperBound(% x) = % x", hi, lo, want)
	}
}

// TestPrune_LeavesIdempotencyReceiptsAlone is the #726 defect, end to end.
//
// A receipt's key is `prefix.Idempotency|siphash(op_id)(8)`. Before the
// relocation, `Prune(untilSeq)` range-deleted `[0x19|be64(1),
// 0x19|be64(untilSeq+1))` — which is precisely the set of receipts whose
// SipHash is at or below the watermark. Rather than search for an op_id that
// hashes low (2^-44 odds at these watermarks), the test writes receipts AT
// those addresses, which is exactly what such an op_id would produce.
func TestPrune_LeavesIdempotencyReceiptsAlone(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	// Receipts whose SipHash lands at 3, 17 and 42 — all under the watermark.
	receipts := map[uint64][]byte{}
	for _, h := range []uint64{3, 17, 42} {
		k := make([]byte, 9)
		k[0] = prefix.Idempotency
		binary.BigEndian.PutUint64(k[1:], h)
		v := legacyIdempotencyValue(t, "engram-for-hash")
		if err := db.Set(k, v, pebble.Sync); err != nil {
			t.Fatalf("seed receipt %d: %v", h, err)
		}
		receipts[h] = v
	}

	log := NewReplicationLog(db)
	for i := 0; i < 60; i++ {
		if _, err := log.Append(OpSet, []byte("k"), []byte("v")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if err := log.Prune(57); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for h, want := range receipts {
		k := make([]byte, 9)
		k[0] = prefix.Idempotency
		binary.BigEndian.PutUint64(k[1:], h)
		got, closer, err := db.Get(k)
		if err != nil {
			t.Fatalf("idempotency receipt at siphash=%d was DELETED by Prune (#726): %v", h, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("receipt at siphash=%d corrupted: got %q want %q", h, got, want)
		}
		closer.Close()
	}

	// And the prune did its actual job.
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(entries) != 3 || entries[0].Seq != 58 {
		t.Fatalf("after Prune(57) of 60 entries: got %d entries starting at %d; want 3 starting at 58",
			len(entries), entries[0].Seq)
	}
}

// TestPrune_LeavesReplicationMetadataAlone is a FORWARD regression guard, not a
// reproduction of #726: under the old layout the seq counter sat at
// `0x19|0xFF*8`, i.e. sequence MaxUint64, which no reachable watermark could
// delete. (The structural half of that defect — metadata living inside the
// entry range at all — is what
// TestReplicationKeys_NeverCollideWithIdempotency asserts, and that one does go
// red on the pre-fix layout.)
//
// What this pins is the consequence if it ever became reachable: losing the seq
// counter restarts the log at 1 and re-issues sequence numbers followers have
// already applied, which an Applier skips as "already applied" — silent
// divergence rather than a visible failure.
func TestPrune_LeavesReplicationMetadataAlone(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	for i := 0; i < 10; i++ {
		if _, err := log.Append(OpSet, []byte("k"), []byte("v")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := db.Set(clusterEpochKey(), []byte{0, 0, 0, 0, 0, 0, 0, 7}, pebble.Sync); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}

	if err := log.Prune(9); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for name, k := range map[string][]byte{"seqCounter": seqCounterKey(), "clusterEpoch": clusterEpochKey()} {
		_, closer, err := db.Get(k)
		if err != nil {
			t.Fatalf("%s deleted by Prune: %v", name, err)
		}
		closer.Close()
	}
	// Sequence continuity survives a full prune.
	seq, err := log.Append(OpSet, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("append after prune: %v", err)
	}
	if seq != 11 {
		t.Errorf("seq after Prune(9) of a 10-entry log = %d; want 11 (the counter must not reset)", seq)
	}
}

// TestMigrationV5KeysMatchReplicationPackage pins migration v5's hard-coded
// destination addresses against this package's real constructors. The migration
// lives in internal/storage/migrate, which cannot import internal/replication
// (replication imports storage), so the two copies are bridged here rather than
// left to drift — a migration that writes to the wrong address silently loses
// the cluster's epoch and sequence counter.
func TestMigrationV5KeysMatchReplicationPackage(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	// Seed every legacy metadata key with a distinguishable value, run the
	// migration, and require the value to reappear under the live constructor.
	legacy := map[string][]byte{
		"seq_counter":   {prefix.Idempotency, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"last_applied":  append([]byte{prefix.Idempotency, 0x02}, "last_app"...),
		"schema_v":      append([]byte{prefix.Idempotency, 0x03}, "schema_v"...),
		"cluster_epoch": append([]byte{prefix.Idempotency, 0x03}, "cluster_epoch"...),
		"node_role":     append([]byte{prefix.Idempotency, 0x03}, "node_role"...),
		"snap_complete": append([]byte{prefix.Idempotency, 0x10}, "snap_complete"...),
	}
	live := map[string][]byte{
		"seq_counter":   seqCounterKey(),
		"last_applied":  lastAppliedKey(),
		"schema_v":      schemaVersionKey(),
		"cluster_epoch": clusterEpochKey(),
		"node_role":     nodeRoleKey(),
		"snap_complete": snapCompleteKey,
	}
	for name, k := range legacy {
		if err := db.Set(k, []byte("value-of-"+name), pebble.Sync); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if err := migrate.RelocateReplicationPrefix(db); err != nil {
		t.Fatalf("RelocateReplicationPrefix: %v", err)
	}

	for name, k := range live {
		got, closer, err := db.Get(k)
		if err != nil {
			t.Fatalf("%s: migration did not write to the live address % x: %v", name, k, err)
		}
		if want := "value-of-" + name; string(got) != want {
			t.Errorf("%s at % x: got %q want %q", name, k, got, want)
		}
		closer.Close()
	}
}

// TestMigrationV5RecognisesRealAppendOutput pins the migration's frozen msgpack
// decoder against what ReplicationLog.Append actually writes, and against what
// an idempotency receipt actually looks like. The migration deletes on the
// strength of this predicate, so a struct-shape drift here is data loss.
func TestMigrationV5RecognisesRealAppendOutput(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	seq, err := log.Append(OpBatch, nil, []byte("some batch repr"))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	val, closer, err := db.Get(replicationEntryKey(seq))
	if err != nil {
		t.Fatalf("read back entry: %v", err)
	}
	entryVal := append([]byte(nil), val...)
	closer.Close()

	// The legacy address for that same entry.
	legacyKey := make([]byte, 9)
	legacyKey[0] = prefix.Idempotency
	binary.BigEndian.PutUint64(legacyKey[1:], seq)

	if !migrate.IsLegacyReplicationLogEntry(legacyKey, entryVal) {
		t.Errorf("IsLegacyReplicationLogEntry rejected a real Append value — v5 would leave the log behind")
	}
	// A receipt at the same address must be refused.
	if migrate.IsLegacyReplicationLogEntry(legacyKey, legacyIdempotencyValue(t, "01J000000000000000000000")) {
		t.Errorf("IsLegacyReplicationLogEntry accepted an idempotency receipt — v5 would delete it")
	}
}
