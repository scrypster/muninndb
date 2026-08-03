package migrate

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func v5TestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// legacyEntryKey is the pre-#726 replication log entry address.
func legacyEntryKey(seq uint64) []byte {
	k := make([]byte, 9)
	k[0] = prefix.Idempotency
	binary.BigEndian.PutUint64(k[1:], seq)
	return k
}

// legacyEntryValue is msgpack in the shape ReplicationLog.Append writes.
func legacyEntryValue(t *testing.T, seq uint64) []byte {
	t.Helper()
	v, err := msgpack.Marshal(&legacyLogEntry{
		Seq:         seq,
		Op:          3,
		Value:       bytes.Repeat([]byte("x"), 64),
		TimestampNS: 1,
	})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return v
}

func receiptValue(t *testing.T, id string) []byte {
	t.Helper()
	v, err := json.Marshal(legacyReceipt{EngramID: id, CreatedAt: 1})
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	return v
}

// seedLegacyStore builds a store in the pre-#726 layout: replication log
// entries and idempotency receipts interleaved under 0x19, plus the six
// replication metadata keys, plus one unrelated storage key that must be inert.
func seedLegacyStore(t *testing.T, db *pebble.DB, entries int) (receiptKeys [][]byte) {
	t.Helper()
	for seq := uint64(1); seq <= uint64(entries); seq++ {
		if err := db.Set(legacyEntryKey(seq), legacyEntryValue(t, seq), pebble.Sync); err != nil {
			t.Fatalf("seed entry %d: %v", seq, err)
		}
	}
	// Receipts whose SipHash lands inside AND outside the entry sequence range.
	for _, h := range []uint64{2, 5, uint64(entries) + 3, 1 << 40} {
		k := legacyEntryKey(h)
		if h <= uint64(entries) {
			// This address is occupied by an entry in the seeded range; place the
			// receipt just above the range instead so the fixture stays honest
			// about what a real store looks like (one key, one value).
			continue
		}
		if err := db.Set(k, receiptValue(t, "engram-01"), pebble.Sync); err != nil {
			t.Fatalf("seed receipt %d: %v", h, err)
		}
		receiptKeys = append(receiptKeys, k)
	}
	for _, m := range legacyReplicationKeys() {
		if err := db.Set(m.old, []byte("meta-"+string(m.new[2:])), pebble.Sync); err != nil {
			t.Fatalf("seed metadata: %v", err)
		}
	}
	if err := db.Set([]byte{prefix.Embedding, 0x01}, []byte("unrelated"), pebble.Sync); err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}
	return receiptKeys
}

func countPrefix(t *testing.T, db *pebble.DB, p []byte) int {
	t.Helper()
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: p, UpperBound: keys.PrefixUpperBound(p)})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		n++
	}
	return n
}

// TestRelocateReplicationPrefix_MovesMetadataDropsEntriesKeepsReceipts is the
// #726 migration, end to end.
func TestRelocateReplicationPrefix_MovesMetadataDropsEntriesKeepsReceipts(t *testing.T) {
	db := v5TestDB(t)
	const entries = 200
	receiptKeys := seedLegacyStore(t, db, entries)

	if got := countPrefix(t, db, []byte{prefix.Idempotency}); got != entries+len(receiptKeys)+6 {
		t.Fatalf("fixture: 0x19 holds %d keys; want %d", got, entries+len(receiptKeys)+6)
	}

	if err := RelocateReplicationPrefix(db); err != nil {
		t.Fatalf("RelocateReplicationPrefix: %v", err)
	}

	// Every legacy log entry is gone.
	for seq := uint64(1); seq <= entries; seq++ {
		if _, closer, err := db.Get(legacyEntryKey(seq)); err == nil {
			closer.Close()
			t.Fatalf("legacy log entry seq=%d survived the migration", seq)
		}
	}
	// Every receipt survives byte-for-byte.
	want := receiptValue(t, "engram-01")
	for _, k := range receiptKeys {
		got, closer, err := db.Get(k)
		if err != nil {
			t.Fatalf("idempotency receipt at % x was DELETED by the migration: %v", k, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("receipt at % x corrupted: got %q want %q", k, got, want)
		}
		closer.Close()
	}
	// Metadata moved, values preserved, old addresses cleared.
	for _, m := range legacyReplicationKeys() {
		if _, closer, err := db.Get(m.old); err == nil {
			closer.Close()
			t.Errorf("legacy metadata key % x still present after migration", m.old)
		}
		got, closer, err := db.Get(m.new)
		if err != nil {
			t.Fatalf("metadata not relocated to % x: %v", m.new, err)
		}
		if w := "meta-" + string(m.new[2:]); string(got) != w {
			t.Errorf("metadata at % x: got %q want %q", m.new, got, w)
		}
		closer.Close()
	}
	// Unrelated storage untouched.
	if _, closer, err := db.Get([]byte{prefix.Embedding, 0x01}); err != nil {
		t.Errorf("unrelated storage key removed: %v", err)
	} else {
		closer.Close()
	}
	// 0x19 now holds receipts only.
	if got := countPrefix(t, db, []byte{prefix.Idempotency}); got != len(receiptKeys) {
		t.Errorf("0x19 holds %d keys after migration; want %d (receipts only)", got, len(receiptKeys))
	}
}

// TestRelocateReplicationPrefix_IsIdempotentAndCrashSafe: a crash leaves the
// migration version unstamped, so Up() re-runs against a partly-migrated store.
// The re-run must not clobber the relocated metadata with a stale legacy copy,
// and must converge.
func TestRelocateReplicationPrefix_IsIdempotentAndCrashSafe(t *testing.T) {
	db := v5TestDB(t)
	seedLegacyStore(t, db, 20)

	if err := RelocateReplicationPrefix(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Simulate post-migration progress: the node ran, the seq counter advanced.
	newCounter := legacyReplicationKeys()[0].new
	if err := db.Set(newCounter, []byte("advanced"), pebble.Sync); err != nil {
		t.Fatalf("advance counter: %v", err)
	}
	// Simulate a crash that left ONE legacy metadata key behind.
	if err := db.Set(legacyReplicationKeys()[0].old, []byte("stale"), pebble.Sync); err != nil {
		t.Fatalf("reseed stale: %v", err)
	}

	if err := RelocateReplicationPrefix(db); err != nil {
		t.Fatalf("second run: %v", err)
	}

	got, closer, err := db.Get(newCounter)
	if err != nil {
		t.Fatalf("counter missing after re-run: %v", err)
	}
	if string(got) != "advanced" {
		t.Errorf("re-run overwrote live state with the stale legacy copy: got %q want %q", got, "advanced")
	}
	closer.Close()
	if _, closer, err := db.Get(legacyReplicationKeys()[0].old); err == nil {
		closer.Close()
		t.Errorf("re-run left the stale legacy key in place")
	}
}

// TestIsLegacyReplicationLogEntry_RefusesEverythingItCannotProve pins the
// destructive predicate. Anything it accepts is deleted forever.
func TestIsLegacyReplicationLogEntry_RefusesEverythingItCannotProve(t *testing.T) {
	entryVal := legacyEntryValue(t, 42)
	cases := []struct {
		name string
		key  []byte
		val  []byte
		want bool
	}{
		{"real legacy entry", legacyEntryKey(42), entryVal, true},
		{"entry value under the WRONG sequence", legacyEntryKey(43), entryVal, false},
		{"idempotency receipt", legacyEntryKey(42), receiptValue(t, "engram-01"), false},
		{"receipt with empty engram id", legacyEntryKey(42), receiptValue(t, ""), false},
		{"seq zero", legacyEntryKey(0), legacyEntryValue(t, 0), false},
		{"legacy seq counter (0xFF*8)", legacyReplicationKeys()[0].old, []byte{0, 0, 0, 0, 0, 0, 0, 9}, false},
		{"legacy last_applied (10 bytes)", legacyReplicationKeys()[1].old, []byte("x"), false},
		{"wrong prefix byte", append([]byte{prefix.Embedding}, legacyEntryKey(42)[1:]...), entryVal, false},
		{"garbage value", legacyEntryKey(42), []byte{0xC1, 0xC1, 0xC1}, false},
		{"empty value", legacyEntryKey(42), nil, false},
	}
	for _, tc := range cases {
		if got := IsLegacyReplicationLogEntry(tc.key, tc.val); got != tc.want {
			t.Errorf("%s: IsLegacyReplicationLogEntry = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestRegisterMigrations_IncludesV5 keeps the runner and the migration in sync —
// v3 shipped referenced only from tests and never ran in production.
func TestRegisterMigrations_IncludesV5(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration v5 (#726 replication prefix relocation) is not registered")
	}
	if got := MaxRegisteredVersion(); got != 5 {
		t.Errorf("MaxRegisteredVersion() = %d; want 5 — the refuse-newer downgrade guard keys off this", got)
	}
}

// TestRunner_RefusesDowngradeAfterV5 is the downgrade story for #726: a store
// migrated to v5 has no seq counter at the legacy address, so an older binary
// would restart the replication log at sequence 1 and re-issue numbers its
// followers already applied. The refuse-newer guard must stop it.
func TestRunner_RefusesDowngradeAfterV5(t *testing.T) {
	db := v5TestDB(t)
	seedLegacyStore(t, db, 5)

	r := NewRunner(db)
	RegisterMigrations(r)
	if _, err := r.Run(); err != nil {
		t.Fatalf("migrate to v5: %v", err)
	}

	// An older binary: only migrations up to v4 registered.
	old := NewRunner(db)
	old.Register(Migration{Version: 1, Up: func(*pebble.DB) error { return nil }})
	old.Register(Migration{Version: 4, Up: func(*pebble.DB) error { return nil }})
	if _, err := old.Run(); err == nil {
		t.Fatalf("a pre-v5 binary started against a v5 store — it would restart the replication log at seq 1")
	}
}
