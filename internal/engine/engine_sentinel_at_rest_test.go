package engine

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestCreatedAtFloor_IsAboveERFZeroTimeSentinel pins a coupling that spans two
// packages and is invisible from either side alone.
//
// erf.decodeTimestamp maps erf.ZeroTimeSentinelNanos back to the zero time so
// IsZero() works on records written before #810. That mapping is ALIASED, not
// collision-free: the sentinel is a legal value inside UnixNano's defined range,
// so the instant 1754-08-30T22:43:41.128654848Z encodes to exactly those bits
// and is destroyed on read (see TestDecodeTimestamp_SentinelInstantAliases).
//
// The ONLY thing keeping that unreachable is this floor. Lowering it — the
// obvious change for a historical, genealogy or journal-import vault — makes the
// collision live and silently loses the stored instant with NO error. If you are
// here because this test failed, that is what you are about to ship.
func TestCreatedAtFloor_IsAboveERFZeroTimeSentinel(t *testing.T) {
	sentinelInstant := time.Unix(0, erf.ZeroTimeSentinelNanos)

	if !createdAtFloor.After(sentinelInstant) {
		t.Fatalf("createdAtFloor (%s) must be strictly after the ERF zero-time sentinel instant (%s): "+
			"a caller-supplied timestamp at or below it aliases the sentinel and is destroyed on read",
			createdAtFloor.UTC().Format(time.RFC3339Nano), sentinelInstant.UTC().Format(time.RFC3339Nano))
	}

	// The behavioural half: the validator must actually refuse that instant.
	if err := validateCreatedAt(sentinelInstant); err == nil {
		t.Fatalf("validateCreatedAt(%s) = nil; the ERF zero-time sentinel instant reached the encoder",
			sentinelInstant.UTC().Format(time.RFC3339Nano))
	}

	// And the floor must stay in step with the shared unset-timestamp year, which
	// every read-side guard tests against.
	if createdAtFloor.Year() != storage.MinPlausibleTimestampYear {
		t.Errorf("createdAtFloor.Year() = %d, want storage.MinPlausibleTimestampYear (%d): the write floor "+
			"and the read-side unset test must agree, or one of them is wrong about what data can exist",
			createdAtFloor.Year(), storage.MinPlausibleTimestampYear)
	}
}

// TestRecall_SentinelLastAccessAtRest_NotSilentlyEmpty is the end-to-end proof
// that #810's fix repairs DATA AT REST, not merely bytes at the ERF boundary.
//
// Everything else in this fix is proven one layer at a time: the erf tests
// inject the sentinel into raw bytes and assert on Decode; the clone tests
// assert on what the write path emits; the unit pin asserts on
// computeComponents in isolation. None of them constructs a record that is
// ACTUALLY on disk carrying the sentinel and recalls it through the whole
// pipeline — which is the exact population the fix claims to rescue: every
// vault cloned before it landed, which never self-heals, because the six
// read-modify-write encoders rewrite the sentinel back verbatim.
//
// The fixture is a real restart, not a mock: write a weighted_sum vault through
// the normal path, close the store (which closes Pebble), reopen, stamp
// uint64(time.Time{}.UnixNano()) into the 0x01 and 0x02 records exactly as the
// pre-fix clone did — recomputing the CRC32 trailer so the records stay
// readable — then bring up a second engine over the same directory and recall.
//
// THE SHAPE IS THE TEST, same three load-bearing properties as the clone e2e:
//
//   - EXACTLY ONE recall against the reopened store. A second one passes either
//     way: the failed first recall populates the L1 cache, and computeComponents
//     prefers the cache's lastAccessNs over eng.LastAccess. That self-masking is
//     how this reached production.
//   - A COLD cache, and this property is STRONGER than "don't recall twice". Any
//     read reaching storage.GetEngram masks the bug for THAT engram: GetEngram
//     ends in ps.cache.Set, which stamps the entry's lastAccess with
//     time.Now(), and that is the very value EngramLastAccessNs returns and
//     computeComponents prefers over eng.LastAccess. So a plain read-by-id
//     (muninn_read), a tree walk, an admin inspection — anything touching these
//     engrams between the reopen and the assertion — turns the fixture green
//     while the bug is fully present. Verified by sabotage: with the
//     computeComponents guard deleted, adding a store.GetEngram loop over the
//     three stamped IDs before the Activate call makes this test PASS. The mask
//     is per record — reading only ONE of the three still fails, on the count
//     assertion at 1 of 3 — so a partial read degrades the evidence silently
//     rather than breaking it loudly. The store is brand new over the reopened
//     DB precisely so that nothing has read them. Do NOT add a "sanity check the
//     records are there" read before the recall.
//   - Threshold left at 0, so the engine supplies COG-6's real weighted_sum
//     default (0.5). Hardcoding a low threshold makes it pass against the bug.
func TestRecall_SentinelLastAccessAtRest_NotSilentlyEmpty(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-sentinel-at-rest-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	const vaultName = "sentinel-at-rest"
	const query = "heron"
	concepts := []string{
		"heron migration routes",
		"heron nesting season",
		"heron feeding behaviour",
	}

	// ---- phase 1: an ordinary weighted_sum vault, written by the normal path ----
	var wantCount int
	var ws [8]byte
	func() {
		db, err := storage.OpenPebble(dir, storage.DefaultOptions())
		if err != nil {
			t.Fatalf("OpenPebble: %v", err)
		}
		eng, as, store, stop := newEngineOverDB(t, db)
		defer stop()

		if err := as.SetVaultConfig(auth.VaultConfig{
			Name: vaultName, Public: true,
			Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")},
		}); err != nil {
			t.Fatalf("SetVaultConfig: %v", err)
		}
		for _, c := range concepts {
			if _, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   vaultName,
				Concept: c,
				Content: "field notes on " + c,
				Tags:    []string{"birds"},
			}); err != nil {
				t.Fatalf("Write %q: %v", c, err)
			}
		}
		awaitFTS(t, eng)
		ws = store.VaultPrefix(vaultName)

		// Control: with healthy timestamps this query matches. If it does not,
		// the fixture never exercised the path and the assertion below would be
		// vacuous.
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault: vaultName, Context: []string{query}, MaxResults: 10,
		})
		if err != nil {
			t.Fatalf("control Activate: %v", err)
		}
		if len(resp.Activations) == 0 {
			t.Fatal("control: healthy weighted_sum vault returned zero results — fixture does not exercise the path")
		}
		wantCount = len(resp.Activations)
	}()

	// ---- corrupt at rest, with the store down: exactly what a pre-fix clone left ----
	stamped := stampSentinelLastAccess(t, dir, ws)
	if stamped == 0 {
		t.Fatal("stamped 0 engram records — the fixture did not find the vault's 0x01 keys")
	}
	t.Logf("stamped the pre-#810 zero-time sentinel (%d) into %d engram records at rest",
		erf.ZeroTimeSentinelNanos, stamped)

	// ---- phase 2: a cold restart over those bytes, and ONE recall ----
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("reopen OpenPebble: %v", err)
	}
	eng, _, _, stop := newEngineOverDB(t, db)
	defer stop()

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vaultName,
		Context:    []string{query},
		MaxResults: 10,
		// Threshold intentionally 0 => engine applies the weighted_sum default (0.5).
	})
	if err != nil {
		t.Fatalf("Activate over sentinel-bearing records at rest: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Fatalf("recall over records carrying the zero-time sentinel AT REST returned ZERO results "+
			"(%d expected) — the #810 silently-empty recall, on the population no write-side fix can repair",
			wantCount)
	}
	if len(resp.Activations) != wantCount {
		t.Errorf("recall returned %d activations, healthy control returned %d — a never-accessed "+
			"LastAccess must not change which memories are retrievable",
			len(resp.Activations), wantCount)
	}
}

// stampSentinelLastAccess opens the closed Pebble directory directly and writes
// uint64(time.Time{}.UnixNano()) into the LastAccess field of every 0x01 engram
// record for ws, and of its 0x02 metadata slice. Returns how many it patched.
//
// The 0x01 record's CRC32 trailer is recomputed so Decode still accepts it — the
// point is a VALID record carrying a garbage timestamp, which is precisely what
// the pre-fix clone produced. The 0x02 slice needs no CRC work: DecodeMeta
// verifies only the CRC16 over bytes 0..8, which LastAccess (offset 40) is
// outside of.
func stampSentinelLastAccess(t *testing.T, dir string, ws [8]byte) int {
	t.Helper()

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("stamp: OpenPebble: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("stamp: close: %v", err)
		}
	}()

	lower := append([]byte{prefix.Engram}, ws[:]...)
	upper := append([]byte(nil), lower...)
	for i := len(upper) - 1; i >= 0; i-- {
		upper[i]++
		if upper[i] != 0 {
			break
		}
	}

	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		t.Fatalf("stamp: NewIter: %v", err)
	}
	var ids [][16]byte
	var records [][]byte
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 25 {
			continue
		}
		var id [16]byte
		copy(id[:], k[9:25])
		ids = append(ids, id)
		records = append(records, append([]byte(nil), iter.Value()...))
	}
	if err := iter.Close(); err != nil {
		t.Fatalf("stamp: iter close: %v", err)
	}

	sentinel := uint64(time.Time{}.UnixNano())
	batch := db.NewBatch()
	defer batch.Close()
	for i, raw := range records {
		if len(raw) < erf.FixedOverhead {
			t.Fatalf("stamp: record %d too short (%d bytes)", i, len(raw))
		}
		binary.BigEndian.PutUint64(raw[erf.OffsetLastAccess:erf.OffsetLastAccess+8], sentinel)
		crc := erf.ComputeCRC32(raw[:len(raw)-erf.TrailerSize])
		binary.BigEndian.PutUint32(raw[len(raw)-erf.TrailerSize:], crc)

		// Fixture self-check: the patched bytes must still decode, and must now
		// present LastAccess as the zero time. If either fails the test below
		// would be asserting on something other than the sentinel.
		decoded, err := erf.Decode(raw)
		if err != nil {
			t.Fatalf("stamp: patched record %d no longer decodes: %v", i, err)
		}
		if !decoded.LastAccess.IsZero() {
			t.Fatalf("stamp: patched record %d decodes LastAccess as %v, want the zero time",
				i, decoded.LastAccess.UTC())
		}
		if err := batch.Set(keys.EngramKey(ws, ids[i]), raw, nil); err != nil {
			t.Fatalf("stamp: set 0x01: %v", err)
		}
		if err := batch.Set(keys.MetaKey(ws, ids[i]), erf.MetaKeySlice(raw), nil); err != nil {
			t.Fatalf("stamp: set 0x02: %v", err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatalf("stamp: commit: %v", err)
	}
	return len(records)
}

// newEngineOverDB builds a full Engine over an ALREADY-OPEN Pebble handle, so a
// test can close and reopen the database around it — the only way to get a
// genuinely cold L1 cache over bytes it has modified. The returned stop func
// closes the store, which closes the handle.
func newEngineOverDB(t *testing.T, db *pebble.DB) (*Engine, *auth.Store, *storage.PebbleStore, func()) {
	t.Helper()
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	as := auth.NewStore(db)
	eng := NewEngine(EngineConfig{
		Store: store, AuthStore: as, FTSIndex: ftsIdx,
		ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder,
	})
	return eng, as, store, func() {
		eng.Stop()
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}
}
