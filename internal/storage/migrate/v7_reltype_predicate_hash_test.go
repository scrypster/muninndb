package migrate

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func v7TestDB(t *testing.T) *pebble.DB {
	return openTestDB(t)
}

// plantLegacyRelationship writes one pre-#894 record: a 42-byte key built by
// the migration's own legacy encoder plus a value encoded by the frozen copy.
// relTypeByte is what the deleted relTypeBytes map would have produced —
// 0x02 for a mapped predicate, 0xFF for anything outside the vocabulary.
func plantLegacyRelationship(t *testing.T, db *pebble.DB, ws [8]byte, engramID [16]byte,
	fromHash, toHash [8]byte, relTypeByte byte, rec v7RelationshipRecord) []byte {
	t.Helper()
	val, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	key := legacyRelationshipKey(ws, engramID, fromHash, relTypeByte, toHash)
	if err := db.Set(key, val, pebble.Sync); err != nil {
		t.Fatalf("plant legacy 0x21 key: %v", err)
	}
	return key
}

// countRelationshipKeys scans [0x21,0x22) and returns the number of keys at
// each length. Only 42 (legacy) and 49 (current) are expected in practice.
func countRelationshipKeys(t *testing.T, db *pebble.DB) map[int]int {
	t.Helper()
	counts := make(map[int]int)
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x21},
		UpperBound: []byte{0x22},
	})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	for valid := iter.First(); valid; valid = iter.Next() {
		counts[len(iter.Key())]++
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	return counts
}

// TestMigrationV7_RekeysPredicateByteToHash — the core rekey: a legacy 42-byte
// key must land at the 49-byte key with keys.PredicateHash(value.RelType) at
// bytes [33:41], the old key must be gone, and the value bytes must be
// byte-identical (verbatim, no re-encode).
func TestMigrationV7_RekeysPredicateByteToHash(t *testing.T) {
	db := v7TestDB(t)
	ws := [8]byte{'v', '7', 0, 0, 0, 0, 0, 1}
	engramID := [16]byte{0xE1}
	fromHash := keys.EntityNameHash("Aurora Platform")
	toHash := keys.EntityNameHash("Kepler Cache")

	rec := v7RelationshipRecord{
		FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
		RelType: "communicates_with", Weight: 0.9, Source: "plugin:enrich", UpdatedAt: 42,
	}
	legacyKey := plantLegacyRelationship(t, db, ws, engramID, fromHash, toHash, 0xFF, rec)

	// A second unmapped predicate about the SAME pair — under the legacy
	// layout both folded to 0xFF; whatever survived is rekeyed by its value.
	rec2 := v7RelationshipRecord{
		FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
		RelType: "deployed_on", Weight: 0.6, Source: "inline", UpdatedAt: 43,
	}
	// Different engram so the two legacy keys do not themselves collide.
	plantLegacyRelationship(t, db, ws, [16]byte{0xE2}, fromHash, toHash, 0xFF, rec2)

	if err := RekeyRelationshipPredicateHash(db); err != nil {
		t.Fatalf("v7 migration: %v", err)
	}

	counts := countRelationshipKeys(t, db)
	if counts[42] != 0 || counts[49] != 2 {
		t.Fatalf("post-v7 key lengths = %v; want zero 42-byte and two 49-byte keys", counts)
	}

	wantVal, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	wantKey := keys.RelationshipKey(ws, engramID, fromHash, keys.PredicateHash("communicates_with"), toHash)
	gotVal, closer, err := db.Get(wantKey)
	if err != nil {
		t.Fatalf("49-byte key %x not found: %v", wantKey, err)
	}
	defer closer.Close()
	if !bytes.Equal(gotVal, wantVal) {
		t.Fatalf("value was re-encoded: got %x want %x (must be verbatim)", gotVal, wantVal)
	}
	wantPred := keys.PredicateHash("communicates_with")
	if !bytes.Equal(wantKey[33:41], wantPred[:]) {
		t.Fatalf("predicate hash not at bytes [33:41]")
	}

	if _, closer, err := db.Get(legacyKey); err != pebble.ErrNotFound {
		closer.Close()
		t.Fatalf("legacy key %x still present after v7", legacyKey)
	}

	// The second record rekeys by ITS value's predicate, from the same 0xFF byte.
	wantKey2 := keys.RelationshipKey(ws, [16]byte{0xE2}, fromHash, keys.PredicateHash("deployed_on"), toHash)
	if _, closer, err := db.Get(wantKey2); err != nil {
		t.Fatalf("second 49-byte key not found at %x: %v", wantKey2, err)
	} else {
		closer.Close()
	}
}

// TestMigrationV7_IdempotentOnRerun — a completed pass re-run directly (or via
// force-rerun) finds no 42-byte keys and changes nothing.
func TestMigrationV7_IdempotentOnRerun(t *testing.T) {
	db := v7TestDB(t)
	ws := [8]byte{'v', '7', 0, 0, 0, 0, 0, 2}

	plantLegacyRelationship(t, db, ws, [16]byte{0xF1}, keys.EntityNameHash("Aurora Platform"),
		keys.EntityNameHash("Kepler Cache"), 0x02,
		v7RelationshipRecord{FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
			RelType: "uses", Weight: 0.8, Source: "inline", UpdatedAt: 7})

	if err := RekeyRelationshipPredicateHash(db); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	afterFirst := countRelationshipKeys(t, db)

	for i := 0; i < 3; i++ {
		if err := RekeyRelationshipPredicateHash(db); err != nil {
			t.Fatalf("re-run %d: %v", i+2, err)
		}
	}
	afterReruns := countRelationshipKeys(t, db)

	if afterFirst[49] != 1 || afterReruns[49] != 1 || afterReruns[42] != 0 {
		t.Fatalf("re-run changed the store: first=%v reruns=%v (want exactly one 49-byte key, zero 42-byte)", afterFirst, afterReruns)
	}
}

// TestMigrationV7_ConvergesTornState — a crash mid-pass leaves a 42/49
// mixture with the version unstamped; the next run must converge to all-49
// with zero duplicates and zero losses.
func TestMigrationV7_ConvergesTornState(t *testing.T) {
	db := v7TestDB(t)
	ws := [8]byte{'v', '7', 0, 0, 0, 0, 0, 3}
	fromHash := keys.EntityNameHash("Aurora Platform")
	toHash := keys.EntityNameHash("Kepler Cache")

	// relTypeByte mirrors what the deleted relTypeBytes map produced: mapped
	// predicates got their byte, everything else folded to 0xFF. The migration
	// never reads the byte — the value's string is authoritative — so the
	// mixture is deliberately realistic, not uniform.
	records := []struct {
		id          [16]byte
		relType     string
		relTypeByte byte
	}{
		{[16]byte{0xA1}, "uses", 0x02},
		{[16]byte{0xA2}, "belongs_to", 0xFF},
		{[16]byte{0xA3}, "integrates_with", 0xFF},
		{[16]byte{0xA4}, "supports", 0x09},
	}
	for i, w := range records {
		plantLegacyRelationship(t, db, ws, w.id, fromHash, toHash, w.relTypeByte,
			v7RelationshipRecord{FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
				RelType: w.relType, Weight: 0.5, Source: "inline", UpdatedAt: int64(i)})
	}

	// Simulate the torn state: two records already moved (49-byte keys present,
	// their legacy keys gone), two still legacy.
	for _, w := range records[:2] {
		legacyKey := legacyRelationshipKey(ws, w.id, fromHash, w.relTypeByte, toHash)
		val, closer, err := db.Get(legacyKey)
		if err != nil {
			t.Fatalf("read legacy for torn simulation: %v", err)
		}
		movedKey := keys.RelationshipKey(ws, w.id, fromHash, keys.PredicateHash(w.relType), toHash)
		if err := db.Set(movedKey, append([]byte(nil), val...), pebble.Sync); err != nil {
			t.Fatalf("plant moved copy: %v", err)
		}
		closer.Close()
		if err := db.Delete(legacyKey, pebble.Sync); err != nil {
			t.Fatalf("remove legacy copy: %v", err)
		}
	}

	if err := RekeyRelationshipPredicateHash(db); err != nil {
		t.Fatalf("convergence pass: %v", err)
	}

	counts := countRelationshipKeys(t, db)
	if counts[49] != len(records) || counts[42] != 0 {
		t.Fatalf("post-convergence lengths = %v; want %d 49-byte keys and zero 42-byte", counts, len(records))
	}
	for _, w := range records {
		k := keys.RelationshipKey(ws, w.id, fromHash, keys.PredicateHash(w.relType), toHash)
		if _, closer, err := db.Get(k); err != nil {
			t.Fatalf("record %d (%s) lost during convergence: %v", w.id[0], w.relType, err)
		} else {
			closer.Close()
		}
	}
}

// TestMigrationV7_FailsLoudOnUndecodableValue — a corrupted value at a 42-byte
// key must return an error and leave the version unstamped (v3's
// never-silently-orphan rule). Run through the real Runner with the store
// planted at version 6 so the stamping path is the production one.
func TestMigrationV7_FailsLoudOnUndecodableValue(t *testing.T) {
	db := v7TestDB(t)
	if err := writeMigrationVersion(db, 6); err != nil {
		t.Fatalf("write version 6: %v", err)
	}

	// A 42-byte key whose value is not msgpack.
	badKey := legacyRelationshipKey([8]byte{9}, [16]byte{8}, [8]byte{7}, 0xFF, [8]byte{6})
	if err := db.Set(badKey, []byte("not-msgpack-at-all"), pebble.Sync); err != nil {
		t.Fatalf("plant corrupted record: %v", err)
	}

	if err := RekeyRelationshipPredicateHash(db); err == nil {
		t.Fatal("undecodable value was silently skipped; want a loud error")
	}

	// The runner must refuse to stamp 7 past the failure.
	r := NewRunner(db)
	r.Register(Migration{Version: 7, Description: "v7", Up: RekeyRelationshipPredicateHash})
	if _, err := r.Run(); err == nil {
		t.Fatal("Runner.Run succeeded past a corrupt 0x21 record")
	}
	if v, err := readMigrationVersion(db); err != nil || v != 6 {
		t.Fatalf("stored version = (%d, %v); want (6, nil) — v7 must not be stamped on failure", v, err)
	}

	// The corrupted record must still be there, un-orphaned.
	if _, closer, err := db.Get(badKey); err != nil {
		t.Fatalf("corrupted record was deleted by the failed pass: %v", err)
	} else {
		closer.Close()
	}
}

// TestV7LegacyLayoutPinned pins the migration's hand-rolled legacy encoder to
// the historical byte layout by literal bytes — it cannot be built from the
// live constructor, which now emits the 49-byte shape, so the two-copy bridge
// here is an explicit pin (the TestV6DiscriminatorMatchesLiveEncoder pattern).
func TestV7LegacyLayoutPinned(t *testing.T) {
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	engramID := [16]byte{0xA, 0xB}
	fromHash := [8]byte{0xC}
	toHash := [8]byte{0xD}

	k := legacyRelationshipKey(ws, engramID, fromHash, 0x0B, toHash)
	if len(k) != 42 {
		t.Fatalf("legacy key length = %d, want 42", len(k))
	}
	if k[0] != 0x21 {
		t.Fatalf("prefix byte = %#x, want 0x21", k[0])
	}
	if !bytes.Equal(k[1:9], ws[:]) || !bytes.Equal(k[9:25], engramID[:]) ||
		!bytes.Equal(k[25:33], fromHash[:]) || !bytes.Equal(k[34:42], toHash[:]) {
		t.Fatalf("legacy layout offsets drifted: %x", k)
	}
	if k[33] != 0x0B {
		t.Fatalf("relTypeByte slot [33] = %#x, want the planted 0x0B", k[33])
	}
}

// TestV7FrozenRecordMatchesLiveEncoder pins the frozen msgpack copy against
// the live encoder: a record written by storage.RelationshipRecord's encoder
// must round-trip through v7RelationshipRecord field-for-field.
func TestV7FrozenRecordMatchesLiveEncoder(t *testing.T) {
	live := storage.RelationshipRecord{
		FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
		RelType: "communicates_with", Weight: 0.9, Source: "plugin:enrich", UpdatedAt: 99,
	}
	val, err := msgpack.Marshal(live)
	if err != nil {
		t.Fatalf("marshal live record: %v", err)
	}
	var frozen v7RelationshipRecord
	if err := msgpack.Unmarshal(val, &frozen); err != nil {
		t.Fatalf("frozen copy cannot decode the live shape: %v", err)
	}
	if frozen.FromEntity != live.FromEntity || frozen.ToEntity != live.ToEntity ||
		frozen.RelType != live.RelType || frozen.Weight != live.Weight ||
		frozen.Source != live.Source || frozen.UpdatedAt != live.UpdatedAt {
		t.Fatalf("frozen copy drifted from the live struct: %+v vs %+v", frozen, live)
	}
}

// TestRegisterMigrations_IncludesV7 — v7 is registered and is the newest
// version: the refuse-newer downgrade guard and ForceRerunMigrations both key
// off MaxRegisteredVersion, so the exact pin lives in the NEWEST migration's
// test only (drift-and-obligations #8).
func TestRegisterMigrations_IncludesV7(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 7 {
			found = true
		}
	}
	if !found {
		t.Fatal("migration v7 (#894 predicate hash rekey) is not registered")
	}
	if got := MaxRegisteredVersion(); got != 7 {
		t.Fatalf("MaxRegisteredVersion() = %d; want 7 — the refuse-newer downgrade guard keys off this", got)
	}
}

// TestRunner_RefusesDowngradeAfterV7 is the v7 downgrade story: a binary that
// predates v7 guards 0x21 consumers by key length and would silently SKIP
// 49-byte keys in deleteEntityLinks and RelinkRelationshipEntity, leaking
// relationship records on hard-delete and entity-merge. The refuse-newer guard
// must block it structurally, mirroring the v6 posture.
func TestRunner_RefusesDowngradeAfterV7(t *testing.T) {
	db := v7TestDB(t)
	if err := writeMigrationVersion(db, 7); err != nil {
		t.Fatalf("write version: %v", err)
	}

	// A binary that predates v7 registers up to 6.
	older := NewRunner(db)
	older.Register(Migration{Version: 6, Description: "pre-v7 head", Up: func(*pebble.DB) error { return nil }})

	applied, err := older.Run()
	if err == nil {
		t.Fatal("older binary started against a v7 store; the refuse-newer guard did not fire")
	}
	if applied != 0 {
		t.Fatalf("applied = %d; want 0", applied)
	}
}

// TestMigrationV7_ForceRerunRekeysReplicatedStrays pins the replica-first
// recovery documented in STO-21 and the v7 header: a follower that upgraded
// (v7 stamped) and THEN applied a PRE-upgrade leader's replicated
// UpsertRelationshipRecord batch repr ends up with a legacy 42-byte key the
// applier committed byte-verbatim AFTER the stamp (internal/replication
// applier.go SetRepr has no key-length or version gate). A plain reopen
// applies nothing (7 == 7) and the stray is permanent; the documented
// recovery is `muninn start --force-migration-rerun`, whose forced pass must
// re-key the stray via v7's length discriminator. Runs the REAL
// RegisterMigrations set so the recovery path is exercised end to end.
func TestMigrationV7_ForceRerunRekeysReplicatedStrays(t *testing.T) {
	db := v7TestDB(t)
	ws := [8]byte{'v', '7', 0, 0, 0, 0, 0, 4}
	engramID := [16]byte{0xE9}
	fromHash := keys.EntityNameHash("Aurora Platform")
	toHash := keys.EntityNameHash("Kepler Cache")
	rec := v7RelationshipRecord{
		FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
		RelType: "integrates_with", Weight: 0.8, Source: "inline", UpdatedAt: 55,
	}

	// The follower has already run v7...
	if err := writeMigrationVersion(db, 7); err != nil {
		t.Fatalf("stamp version 7: %v", err)
	}
	// ...and then applies the old leader's replicated write: a legacy 42-byte
	// key landing after the stamp, exactly as the applier's byte-verbatim
	// SetRepr path delivers it.
	strayKey := plantLegacyRelationship(t, db, ws, engramID, fromHash, toHash, 0xFF, rec)

	// A plain reopen applies nothing — current version == max registered — and
	// the stray survives it. This is the hazard, asserted: without operator
	// action the stray is permanent.
	plain := NewRunner(db)
	RegisterMigrations(plain)
	applied, err := plain.Run()
	if err != nil {
		t.Fatalf("plain reopen: %v", err)
	}
	if applied != 0 {
		t.Fatalf("plain reopen applied %d migrations; want 0 at version 7", applied)
	}
	if _, closer, err := db.Get(strayKey); err != nil {
		t.Fatalf("plain reopen must leave the replicated stray in place (it did not — test premise broken): %v", err)
	} else {
		closer.Close()
	}
	if counts := countRelationshipKeys(t, db); counts[42] != 1 || counts[49] != 0 {
		t.Fatalf("post-reopen key lengths = %v; want the one 42-byte stray", counts)
	}

	// The documented recovery: force-rerun resets the version so the next open
	// re-applies every migration, and v7's length discriminator re-keys the
	// stray alongside the (empty) rest of the store.
	if err := ForceRerunMigrations(db); err != nil {
		t.Fatalf("force-rerun: %v", err)
	}
	recovered := NewRunner(db)
	RegisterMigrations(recovered)
	if _, err := recovered.Run(); err != nil {
		t.Fatalf("recovery run: %v", err)
	}

	counts := countRelationshipKeys(t, db)
	if counts[42] != 0 || counts[49] != 1 {
		t.Fatalf("post-recovery key lengths = %v; want zero strays and one 49-byte key", counts)
	}
	wantKey := keys.RelationshipKey(ws, engramID, fromHash, keys.PredicateHash("integrates_with"), toHash)
	wantVal, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	gotVal, closer, err := db.Get(wantKey)
	if err != nil {
		t.Fatalf("re-keyed record not found at %x: %v", wantKey, err)
	}
	defer closer.Close()
	if !bytes.Equal(gotVal, wantVal) {
		t.Fatalf("recovered value was re-encoded: got %x want %x (must be verbatim)", gotVal, wantVal)
	}
	if _, _, err := db.Get(strayKey); err != pebble.ErrNotFound {
		t.Fatalf("stray key %x still present after recovery", strayKey)
	}
}

// TestMigrationV7_BatchReallocationPath exercises the >500-record path: with
// batchSize 500, a pass over 600 legacy records must take the mid-scan commit
// AND the batch re-allocation (plus the final under-batch flush). Zero loss,
// zero duplication, and byte-identical values across the re-allocation
// boundary are the proof the path works; a second run must be a no-op.
func TestMigrationV7_BatchReallocationPath(t *testing.T) {
	db := v7TestDB(t)
	ws := [8]byte{'v', '7', 0, 0, 0, 0, 0, 5}

	const total = 600 // batchSize is 500: 600 forces the re-allocation path
	predicates := []struct {
		name string
		b    byte // the byte the deleted relTypeBytes map would have produced
	}{
		{"uses", 0x02}, {"belongs_to", 0xFF}, {"integrates_with", 0xFF},
		{"deployed_on", 0xFF}, {"supports", 0x09}, {"", 0xFF},
	}

	type planted struct {
		wantKey []byte
		wantVal []byte
	}
	want := make([]planted, 0, total)
	for i := 0; i < total; i++ {
		engramID := [16]byte{byte(i >> 8), byte(i)} // distinct engram per record
		p := predicates[i%len(predicates)]
		fromHash := keys.EntityNameHash("Aurora Platform")
		toHash := keys.EntityNameHash("Kepler Cache")
		if i%2 == 1 {
			fromHash, toHash = toHash, fromHash // vary direction too
		}
		rec := v7RelationshipRecord{
			FromEntity: "Aurora Platform", ToEntity: "Kepler Cache",
			RelType: p.name, Weight: float32(i%100) / 100.0, Source: "plugin:enrich", UpdatedAt: int64(i),
		}
		val, err := msgpack.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record %d: %v", i, err)
		}
		if err := db.Set(legacyRelationshipKey(ws, engramID, fromHash, p.b, toHash), val, pebble.NoSync); err != nil {
			t.Fatalf("plant record %d: %v", i, err)
		}
		want = append(want, planted{
			wantKey: keys.RelationshipKey(ws, engramID, fromHash, keys.PredicateHash(p.name), toHash),
			wantVal: val,
		})
	}

	if err := RekeyRelationshipPredicateHash(db); err != nil {
		t.Fatalf("v7 over %d records: %v", total, err)
	}

	counts := countRelationshipKeys(t, db)
	if counts[42] != 0 || counts[49] != total {
		t.Fatalf("post-v7 key lengths = %v; want zero 42-byte and %d 49-byte keys", counts, total)
	}
	seen := make(map[string]int, total)
	for _, w := range want {
		got, closer, err := db.Get(w.wantKey)
		if err != nil {
			t.Fatalf("record %x lost across the batch re-allocation: %v", w.wantKey, err)
		}
		if !bytes.Equal(got, w.wantVal) {
			t.Fatalf("record %x value not verbatim: got %x want %x", w.wantKey, got, w.wantVal)
		}
		closer.Close()
		seen[string(w.wantKey)]++
	}
	if len(seen) != total {
		t.Fatalf("distinct destination keys = %d; want %d (duplication check)", len(seen), total)
	}

	// Second pass: nothing left to do.
	if err := RekeyRelationshipPredicateHash(db); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if counts = countRelationshipKeys(t, db); counts[49] != total || counts[42] != 0 {
		t.Fatalf("second pass changed the store: %v", counts)
	}
}
