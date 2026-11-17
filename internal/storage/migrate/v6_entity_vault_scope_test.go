package migrate

import (
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func v6TestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var (
	wsAlpha = [8]byte{'a', 'l', 'p', 'h', 'a', 0, 0, 0}
	wsBeta  = [8]byte{'b', 'e', 't', 'a', 0, 0, 0, 0}
)

// legacyEntityKey is the pre-#683 global entity record address.
func legacyEntityKey(name string) []byte {
	h := keys.EntityNameHash(name)
	k := make([]byte, 9)
	k[0] = prefix.Entity
	copy(k[1:], h[:])
	return k
}

func putLegacyEntity(t *testing.T, db *pebble.DB, rec legacyEntityRecord) {
	t.Helper()
	val, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if err := db.Set(legacyEntityKey(rec.Name), val, pebble.Sync); err != nil {
		t.Fatalf("set legacy record: %v", err)
	}
}

// putReverseLink writes a 0x23 entity→engram reverse index key.
func putReverseLink(t *testing.T, db *pebble.DB, name string, ws [8]byte, engram [16]byte) {
	t.Helper()
	if err := db.Set(keys.EntityReverseIndexKey(keys.EntityNameHash(name), ws, engram), nil, pebble.Sync); err != nil {
		t.Fatalf("set reverse link: %v", err)
	}
}

// putRelEntityIndex writes a 0x26 relationship-entity index key.
func putRelEntityIndex(t *testing.T, db *pebble.DB, name string, ws [8]byte, engram [16]byte) {
	t.Helper()
	if err := db.Set(keys.RelEntityIndexKey(ws, keys.EntityNameHash(name), engram), nil, pebble.Sync); err != nil {
		t.Fatalf("set rel entity index: %v", err)
	}
}

func readScoped(t *testing.T, db *pebble.DB, ws [8]byte, name string) *legacyEntityRecord {
	t.Helper()
	val, closer, err := db.Get(keys.EntityKey(ws, keys.EntityNameHash(name)))
	if err == pebble.ErrNotFound {
		return nil
	}
	if err != nil {
		t.Fatalf("get scoped record: %v", err)
	}
	defer closer.Close()
	var rec legacyEntityRecord
	if err := msgpack.Unmarshal(val, &rec); err != nil {
		t.Fatalf("decode scoped record: %v", err)
	}
	return &rec
}

func legacyExists(t *testing.T, db *pebble.DB, name string) bool {
	t.Helper()
	_, closer, err := db.Get(legacyEntityKey(name))
	if err == pebble.ErrNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("get legacy record: %v", err)
	}
	closer.Close()
	return true
}

func eid(b byte) [16]byte {
	var id [16]byte
	id[0] = b
	return id
}

// seedV6LegacyStore builds a store in the pre-#683 layout: one global entity
// record shared by two vaults, with the reverse index recording how many
// engrams in each vault actually mention it.
func seedV6LegacyStore(t *testing.T, db *pebble.DB) {
	t.Helper()
	// "Cindersmith" — 1 engram in alpha, 3 in beta. The legacy record carries
	// the cross-vault upsert total (7 here: mentions plus re-enrichment noise).
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Cindersmith", Type: "system", Confidence: 0.8, Source: "inline",
		FirstSeen: 111, UpdatedAt: 222, MentionCount: 7, State: "active",
	})
	putReverseLink(t, db, "Cindersmith", wsAlpha, eid(1))
	putReverseLink(t, db, "Cindersmith", wsBeta, eid(2))
	putReverseLink(t, db, "Cindersmith", wsBeta, eid(3))
	putReverseLink(t, db, "Cindersmith", wsBeta, eid(4))

	// "Quillfeather" — beta only.
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Quillfeather", Type: "person", Confidence: 0.5, Source: "plugin:enrich",
		FirstSeen: 333, UpdatedAt: 444, MentionCount: 2, State: "active",
	})
	putReverseLink(t, db, "Quillfeather", wsBeta, eid(5))
}

func TestV6_RelocatesPerVaultWithRecomputedCounts(t *testing.T) {
	db := v6TestDB(t)
	seedV6LegacyStore(t, db)

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("VaultScopeEntityRecords: %v", err)
	}

	alpha := readScoped(t, db, wsAlpha, "Cindersmith")
	if alpha == nil {
		t.Fatal("alpha lost its Cindersmith record")
	}
	if alpha.MentionCount != 1 {
		t.Errorf("alpha mention_count = %d; want 1 (one mentioning engram in alpha)", alpha.MentionCount)
	}
	beta := readScoped(t, db, wsBeta, "Cindersmith")
	if beta == nil {
		t.Fatal("beta lost its Cindersmith record")
	}
	if beta.MentionCount != 3 {
		t.Errorf("beta mention_count = %d; want 3", beta.MentionCount)
	}
	// Non-count metadata is carried verbatim into every vault.
	if alpha.Type != "system" || alpha.Confidence != 0.8 || alpha.FirstSeen != 111 || alpha.State != "active" {
		t.Errorf("alpha metadata not preserved: %+v", alpha)
	}

	// An entity only beta ever mentioned must not appear in alpha at all —
	// this is the tenancy half of #683.
	if got := readScoped(t, db, wsAlpha, "Quillfeather"); got != nil {
		t.Errorf("alpha gained a beta-only entity: %+v", got)
	}
	if got := readScoped(t, db, wsBeta, "Quillfeather"); got == nil || got.MentionCount != 1 {
		t.Errorf("beta Quillfeather = %+v; want mention_count 1", got)
	}

	// The legacy global keys are gone.
	for _, name := range []string{"Cindersmith", "Quillfeather"} {
		if legacyExists(t, db, name) {
			t.Errorf("legacy global record for %q survived the migration", name)
		}
	}
}

// TestV6_IdempotentOnRerun models a crash after the relocation but before the
// version stamp: the whole migration re-runs. It must converge and must NOT
// clobber post-migration state with the stale legacy copy.
func TestV6_IdempotentOnRerun(t *testing.T) {
	db := v6TestDB(t)
	seedV6LegacyStore(t, db)

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Simulate live traffic after the migration: beta's record moves on.
	beta := readScoped(t, db, wsBeta, "Cindersmith")
	beta.MentionCount = 99
	beta.State = "deprecated"
	val, err := msgpack.Marshal(beta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Set(keys.EntityKey(wsBeta, keys.EntityNameHash("Cindersmith")), val, pebble.Sync); err != nil {
		t.Fatalf("set post-migration state: %v", err)
	}
	// Re-introduce the legacy key as a crash would have left it.
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Cindersmith", Type: "system", Confidence: 0.8, Source: "inline",
		FirstSeen: 111, UpdatedAt: 222, MentionCount: 7, State: "active",
	})

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("second run: %v", err)
	}

	got := readScoped(t, db, wsBeta, "Cindersmith")
	if got == nil || got.MentionCount != 99 || got.State != "deprecated" {
		t.Errorf("re-run clobbered post-migration state: %+v", got)
	}
	if legacyExists(t, db, "Cindersmith") {
		t.Error("re-run left the legacy key behind")
	}
}

// TestV6_KeepsMergeTombstonesWithTheirTarget: MergeEntity relinks every engram
// and relationship off A before stamping A "merged", so A has no references of
// its own. A naive drop-the-unreferenced rule would delete every merge
// tombstone in the store.
func TestV6_KeepsMergeTombstonesWithTheirTarget(t *testing.T) {
	db := v6TestDB(t)
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Thornbury Co", Type: "org", MentionCount: 4, State: "merged",
		MergedInto: "Thornbury Company",
	})
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Thornbury Company", Type: "org", MentionCount: 9, State: "active",
	})
	putReverseLink(t, db, "Thornbury Company", wsAlpha, eid(1))
	putReverseLink(t, db, "Thornbury Company", wsAlpha, eid(2))

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("VaultScopeEntityRecords: %v", err)
	}

	tomb := readScoped(t, db, wsAlpha, "Thornbury Co")
	if tomb == nil {
		t.Fatal("merge tombstone was dropped; it must follow its MergedInto target")
	}
	if tomb.State != "merged" || tomb.MergedInto != "Thornbury Company" {
		t.Errorf("tombstone lost its merge state: %+v", tomb)
	}
	if tomb.MentionCount != 0 {
		t.Errorf("tombstone mention_count = %d; want 0 (it has no mentions of its own)", tomb.MentionCount)
	}
	if got := readScoped(t, db, wsBeta, "Thornbury Co"); got != nil {
		t.Errorf("tombstone leaked into a vault that never saw the merge: %+v", got)
	}
}

// TestV6_TombstoneFansOutIntoUninvolvedVault documents a known, accepted
// residual of clause 4 (see the "WHICH VAULTS A RECORD GOES TO" doc comment on
// VaultScopeEntityRecords and STO-17's residual paragraph): a merge tombstone
// inherits the FULL vault set of its MergedInto target, and that set can have
// more than one member. TestV6_KeepsMergeTombstonesWithTheirTarget above only
// ever gives the target a SINGLE referencing vault — a shape that cannot
// exercise the fan-out. Here the target is referenced by both alpha (which
// performed the merge) and beta (an unrelated vault that never mentioned the
// merged-away name at all); beta still receives the tombstone, because
// nothing in the keyspace records which vault ran the merge. This is not a
// regression — beta saw the same record through the pre-#683 global key — and
// is kept deliberately rather than guessed away.
func TestV6_TombstoneFansOutIntoUninvolvedVault(t *testing.T) {
	db := v6TestDB(t)
	// alpha merged "Acme Co" into "Acme Corporation".
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Acme Co", Type: "org", MentionCount: 4, State: "merged",
		MergedInto: "Acme Corporation",
	})
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Acme Corporation", Type: "org", MentionCount: 9, State: "active",
	})
	// alpha references the target, having performed the merge...
	putReverseLink(t, db, "Acme Corporation", wsAlpha, eid(1))
	// ...and so does beta, an unrelated tenant that never merged anything and
	// has zero references of any kind to "Acme Co".
	putReverseLink(t, db, "Acme Corporation", wsBeta, eid(2))

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("VaultScopeEntityRecords: %v", err)
	}

	got := readScoped(t, db, wsBeta, "Acme Co")
	if got == nil {
		t.Fatal("expected the known fan-out: beta should receive the tombstone " +
			"because it references the merge target, even though it never " +
			"referenced the merged-away name itself")
	}
	if got.State != "merged" || got.MergedInto != "Acme Corporation" {
		t.Errorf("beta's fanned-out tombstone lost its merge state: %+v", got)
	}
	t.Log("known residual reproduced: beta has zero references to \"Acme Co\" " +
		"yet received a full merge-tombstone record for it, because it " +
		"references \"Acme Corporation\" and clause 4 is not per-vault")
}

// TestV6_RelationshipOnlyEntityKeepsItsVault: an entity referenced only by a
// 0x26 relationship index entry still belongs to that vault, with no mentions.
func TestV6_RelationshipOnlyEntityKeepsItsVault(t *testing.T) {
	db := v6TestDB(t)
	putLegacyEntity(t, db, legacyEntityRecord{
		Name: "Halvard Protocol", Type: "protocol", MentionCount: 3, State: "active",
	})
	putRelEntityIndex(t, db, "Halvard Protocol", wsBeta, eid(7))

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("VaultScopeEntityRecords: %v", err)
	}
	got := readScoped(t, db, wsBeta, "Halvard Protocol")
	if got == nil {
		t.Fatal("relationship-only entity was dropped")
	}
	if got.MentionCount != 0 {
		t.Errorf("mention_count = %d; want 0 — a relationship is a reference, not a mention", got.MentionCount)
	}
}

// TestV6_LeavesForeignKeysAlone is the #726 lesson applied here: the delete
// sweep is key-by-key behind a positive test, so a 9-byte key under 0x1F whose
// value is not a self-consistent entity record is never touched — and neither
// is the migration's own 17-byte output.
func TestV6_LeavesForeignKeysAlone(t *testing.T) {
	db := v6TestDB(t)
	seedV6LegacyStore(t, db)

	// A 9-byte 0x1F key whose value decodes as an entity record but whose name
	// hashes somewhere else entirely — the conjunctive clause must reject it.
	impostorKey := make([]byte, 9)
	impostorKey[0] = prefix.Entity
	copy(impostorKey[1:], []byte{9, 9, 9, 9, 9, 9, 9, 9})
	impostorVal, err := msgpack.Marshal(legacyEntityRecord{Name: "Wrenfield", MentionCount: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Set(impostorKey, impostorVal, pebble.Sync); err != nil {
		t.Fatalf("set impostor: %v", err)
	}
	if IsLegacyEntityRecord(impostorKey, impostorVal) {
		t.Fatal("discriminator accepted a record whose name does not hash to its address")
	}

	if err := VaultScopeEntityRecords(db); err != nil {
		t.Fatalf("VaultScopeEntityRecords: %v", err)
	}

	if _, closer, err := db.Get(impostorKey); err != nil {
		t.Errorf("migration deleted a key it could not positively identify: %v", err)
	} else {
		closer.Close()
	}
	// And the relocated records survived their own delete sweep.
	if readScoped(t, db, wsBeta, "Cindersmith") == nil {
		t.Error("the delete sweep removed a relocated record")
	}
}

// TestV6_DiscriminatorRejectsRelocatedKey pins the length clause directly: the
// migration's own 17-byte output can never be mistaken for legacy input.
func TestV6_DiscriminatorRejectsRelocatedKey(t *testing.T) {
	rec := legacyEntityRecord{Name: "Cindersmith", MentionCount: 1}
	val, err := msgpack.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	scoped := keys.EntityKey(wsAlpha, keys.EntityNameHash(rec.Name))
	if IsLegacyEntityRecord(scoped, val) {
		t.Error("discriminator accepted a vault-scoped (17-byte) key as legacy")
	}
	if !IsLegacyEntityRecord(legacyEntityKey(rec.Name), val) {
		t.Error("discriminator rejected a genuine legacy record")
	}
}

func TestRegisterMigrations_IncludesV6(t *testing.T) {
	r := &Runner{}
	RegisterMigrations(r)
	found := false
	for _, m := range r.migrations {
		if m.Version == 6 {
			found = true
		}
	}
	if !found {
		t.Fatalf("migration v6 (#683 entity vault scoping) is not registered")
	}
	if got := MaxRegisteredVersion(); got != 6 {
		t.Errorf("MaxRegisteredVersion() = %d; want 6 — the refuse-newer downgrade guard keys off this", got)
	}
}

// TestRunner_RefusesDowngradeAfterV6 is the downgrade story for #683: a store
// migrated to v6 has no global 0x1F record at any name, so an older binary
// would report every entity as unknown and re-create records at the global
// address — resurrecting the leak. The refuse-newer guard must stop it, and it
// must be structural rather than a documented warning.
func TestRunner_RefusesDowngradeAfterV6(t *testing.T) {
	db := v6TestDB(t)
	if err := writeMigrationVersion(db, 6); err != nil {
		t.Fatalf("write version: %v", err)
	}

	// A binary that predates v6 registers up to 5.
	older := NewRunner(db)
	older.Register(Migration{Version: 5, Description: "pre-v6 head", Up: func(*pebble.DB) error { return nil }})

	applied, err := older.Run()
	if err == nil {
		t.Fatal("older binary started against a v6 store; the refuse-newer guard did not fire")
	}
	if applied != 0 {
		t.Errorf("applied = %d; want 0", applied)
	}
}

// TestV6DiscriminatorMatchesLiveEncoder pins the frozen legacyEntityRecord copy
// against the LIVE storage.EntityRecord msgpack shape. A field-name or type
// change on the live struct that this copy did not track would make the
// discriminator stop recognising real records — the migration would then
// silently relocate nothing and leave every vault on the leaking layout.
func TestV6DiscriminatorMatchesLiveEncoder(t *testing.T) {
	live := storage.EntityRecord{
		Name: "Cindersmith", Type: "system", Confidence: 0.75, Source: "inline",
		UpdatedAt: 12, FirstSeen: 11, MentionCount: 5, State: "active",
	}
	val, err := msgpack.Marshal(live)
	if err != nil {
		t.Fatalf("marshal live record: %v", err)
	}
	if !IsLegacyEntityRecord(legacyEntityKey(live.Name), val) {
		t.Fatal("the discriminator does not recognise a record written by the live encoder")
	}

	var frozen legacyEntityRecord
	if err := msgpack.Unmarshal(val, &frozen); err != nil {
		t.Fatalf("frozen copy cannot decode the live shape: %v", err)
	}
	if frozen.Name != live.Name || frozen.Type != live.Type || frozen.Source != live.Source ||
		frozen.State != live.State || frozen.MergedInto != live.MergedInto ||
		frozen.Confidence != live.Confidence || frozen.MentionCount != live.MentionCount ||
		frozen.FirstSeen != live.FirstSeen || frozen.UpdatedAt != live.UpdatedAt {
		t.Errorf("frozen copy drifted from the live struct: %+v vs %+v", frozen, live)
	}
}
