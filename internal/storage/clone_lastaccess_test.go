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

// TestCloneVaultData_LastAccessSurvivesOnDisk pins the on-disk bytes of a
// cloned engram's LastAccess: it is the SOURCE's LastAccess, verbatim — not the
// zero-time sentinel, and not CreatedAt.
//
// Clone originally wrote time.Time{} straight through erf.Encode, which stores
// uint64(time.Time{}.UnixNano()) and decodes as 1754-08-30 — a value whose
// IsZero() is false, so every downstream guard waved it through (#810). The
// first fix replaced it with CreatedAt; that is correct only for a record that
// has genuinely never been read, and this fixture is deliberately the other
// case — a memory created 72h ago and READ 1h ago, which is what #682's
// ReinforceOnRead makes the ordinary state of a live vault.
//
// This asserts on the RAW BYTES, not the decoded value, because the decode-side
// sentinel mapping (part 2 of #810) would otherwise mask a clone that still
// writes the garbage: decode would turn it into the zero time and this test
// would look green while the on-disk record was still wrong.
func TestCloneVaultData_LastAccessSurvivesOnDisk(t *testing.T) {
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
	if rawLastAccess != accessed.UnixNano() {
		t.Errorf("cloned LastAccess raw = %d (%v), want the SOURCE's LastAccess raw = %d (%v) — the clone must not invent a timestamp",
			rawLastAccess, time.Unix(0, rawLastAccess).UTC(), accessed.UnixNano(), accessed.UTC())
	}
	if rawLastAccess == rawCreatedAt {
		t.Errorf("cloned LastAccess raw == CreatedAt raw (%d) — LastAccess was rewound to the creation date, which is the second form of #810: "+
			"an actively-used memory loses ~%.0f days of recency and drops below threshold (DEFAULT ACT-R at 7 days of source age)",
			rawCreatedAt, accessed.Sub(created).Hours()/24.0)
	}

	// And the decoded view agrees, with AccessCount still reset.
	got, err := store.GetEngram(ctx, wsTarget, id)
	if err != nil {
		t.Fatalf("GetEngram target: %v", err)
	}
	if got.AccessCount != 0 {
		t.Errorf("AccessCount = %d after clone, want 0", got.AccessCount)
	}
	if !got.LastAccess.Equal(accessed) {
		t.Errorf("decoded LastAccess = %v, want the source's %v", got.LastAccess.UTC(), accessed.UTC())
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

// TestCloneVaultData_SentinelSourceHealsToCreatedAt is the other half of the
// carry-over rule, and the reason normalizeEngramTimes is still on the clone
// path at all.
//
// Carrying the source's LastAccess verbatim would be wrong if the source itself
// carries the pre-#810 zero-time sentinel — that is a vault cloned before the
// fix, and copying its garbage forward would make the defect hereditary.
// erf.Decode maps the sentinel back to the zero time, normalizeEngramTimes then
// substitutes CreatedAt, and the clone lands on the honest fallback: "we do not
// know when this was read; treat it as its creation date."
//
// So the rule is not "carry it over", it is "carry over what the source
// actually knows". This test is the case where it knows nothing.
func TestCloneVaultData_SentinelSourceHealsToCreatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("src-heal")
	wsTarget := store.VaultPrefix("dst-heal")

	id, _, sentinel := plantSentinelEngram(t, store, wsSource)

	if err := store.WriteVaultName(wsTarget, "dst-heal"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if _, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil); err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}

	raw, err := Get(store.db, keys.EngramKey(wsTarget, [16]byte(id)))
	if err != nil || raw == nil {
		t.Fatalf("get raw cloned engram: %v (raw nil = %v)", err, raw == nil)
	}
	rawLastAccess := binary.BigEndian.Uint64(raw[erf.OffsetLastAccess : erf.OffsetLastAccess+8])
	rawCreatedAt := binary.BigEndian.Uint64(raw[erf.OffsetCreatedAt : erf.OffsetCreatedAt+8])

	if rawLastAccess == sentinel {
		t.Fatalf("clone copied the pre-#810 zero-time sentinel forward (raw=%d, reads back as %v) — "+
			"the defect would be hereditary across every re-clone",
			int64(rawLastAccess), time.Unix(0, int64(rawLastAccess)).UTC())
	}
	if rawLastAccess != rawCreatedAt {
		t.Errorf("cloned LastAccess raw = %d (%v), want CreatedAt raw = %d (%v) — an unset source LastAccess must fall back to CreatedAt",
			int64(rawLastAccess), time.Unix(0, int64(rawLastAccess)).UTC(),
			int64(rawCreatedAt), time.Unix(0, int64(rawCreatedAt)).UTC())
	}

	got, err := store.GetEngram(ctx, wsTarget, id)
	if err != nil {
		t.Fatalf("GetEngram target: %v", err)
	}
	if IsUnsetTimestamp(got.LastAccess) {
		t.Errorf("decoded LastAccess = %v is still unset after the clone", got.LastAccess.UTC())
	}
}

// rmwWriters drives every ENTRY POINT into the set of writers that encode a
// 0x01 engram record WITHOUT going through normalizeEngramTimes. The writer set
// was derived by walking all ten erf.Encode/EncodeV2 call sites in package
// storage: four normalize (WriteEngram, WriteEngramBatch, BatchWriter.WriteEngram,
// CloneVaultData) and six read-modify-write. There is no eleventh.
//
// The table has SEVEN entries for those six writers, and the extra one is the
// point rather than a miscount: pebbleStoreBatch.mutateEngram is ONE encoder
// reached through TWO exported methods, UpdateEngramState and SupersedeEngram,
// which set different fields on the way in. Driving one of them would leave the
// other's argument handling unexercised, so both are driven — the same reason
// this table exists at all. timestamps.go's SCOPE paragraph states the writer
// set the same way ("mutateEngram (backing UpdateEngramState and
// SupersedeEngram)"); this comment previously said "these six" over seven rows
// and left a reader to reconcile it.
//
// Each entry mutates ONE field of an existing record and re-encodes. Each is
// exercised against its own freshly-planted sentinel record by
// TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal.
//
// Driving only one of them — the shape this test had before the #810 review
// round — proved nothing about the other five: adding normalizeEngramTimes to
// UpdateTags left the entire suite green.
var rmwWriters = []struct {
	name string
	// apply performs the writer's mutation against id. It must return an error
	// only for a genuine failure; a nil error means the record was rewritten.
	apply func(t *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error
}{
	{"SoftDelete", func(_ *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		return store.SoftDelete(ctx, ws, id)
	}},
	{"UpdateTags", func(_ *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		return store.UpdateTags(ctx, ws, id, []string{"heron", "quarry"})
	}},
	{"UpdateConfidence", func(_ *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		return store.UpdateConfidence(ctx, ws, id, 0.42)
	}},
	{"UpdateConfidenceWithContradiction", func(_ *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		_, _, err := store.UpdateConfidenceWithContradiction(ctx, ws, id, -0.1, id, false)
		return err
	}},
	{"UpdateDigest", func(_ *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		return store.UpdateDigest(ctx, id, "a one-line digest", []string{"first point"}, "", "")
	}},
	{"mutateEngram/UpdateEngramState", func(t *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		b := store.NewBatch()
		if err := b.UpdateEngramState(ctx, ws, id, StateArchived); err != nil {
			b.Discard()
			return err
		}
		return b.Commit()
	}},
	{"mutateEngram/SupersedeEngram", func(t *testing.T, ctx context.Context, store *PebbleStore, ws [8]byte, id ULID) error {
		b := store.NewBatch()
		if err := b.SupersedeEngram(ctx, ws, id, time.Now()); err != nil {
			b.Discard()
			return err
		}
		return b.Commit()
	}},
}

// TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal pins the honest scope
// of normalizeEngramTimes, which an earlier version of its doc comment got
// wrong by claiming "every writer of a 0x01 record must funnel through this".
//
// The six writers in rmwWriters do not, and correctly so: mutateEngram (behind
// UpdateEngramState and SupersedeEngram), SoftDelete, UpdateTags,
// UpdateConfidence, UpdateConfidenceWithContradiction and UpdateDigest are
// read-modify-write. They create no NEW corruption — but they decode, mutate one
// field and re-encode, so a zero LastAccess that decode just repaired is written
// straight back out as the sentinel. They preserve; they do not heal.
//
// EVERY ONE of them is driven here, against its own freshly-planted sentinel
// record. The previous version of this test named six writers and drove only
// SoftDelete, which is the defect pattern this whole round is about: adding
// normalizeEngramTimes to UpdateTags left the shipped pin GREEN.
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
	for _, w := range rmwWriters {
		t.Run(w.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			ws := store.VaultPrefix("rmw-sentinel")

			id, key, sentinel := plantSentinelEngram(t, store, ws)

			if err := w.apply(t, ctx, store, ws, id); err != nil {
				t.Fatalf("%s: %v", w.name, err)
			}

			after, err := Get(store.db, key)
			if err != nil || after == nil {
				t.Fatalf("get raw engram after %s: %v", w.name, err)
			}
			gotRaw := int64(binary.BigEndian.Uint64(after[erf.OffsetLastAccess : erf.OffsetLastAccess+8]))
			if gotRaw != int64(sentinel) {
				t.Fatalf("after %s the on-disk LastAccess raw = %d (%v), want the sentinel %d — this RMW writer "+
					"started healing timestamps; see this test's doc for the three statements that depend on it not doing so",
					w.name, gotRaw, time.Unix(0, gotRaw).UTC(), int64(sentinel))
			}
			t.Logf("PERPETUATED as designed: after %s the on-disk LastAccess is still the sentinel raw=%d (%v)",
				w.name, gotRaw, time.Unix(0, gotRaw).UTC())

			// The decode-side repair is what keeps that byte pattern harmless.
			got, err := store.GetEngram(ctx, ws, id)
			if err != nil {
				t.Fatalf("GetEngram: %v", err)
			}
			if got == nil {
				t.Fatalf("GetEngram returned nil after %s", w.name)
			}
			if !got.LastAccess.IsZero() {
				t.Errorf("decoded LastAccess = %v, want the zero time — the decode-side repair is the only thing "+
					"standing between this record and a 740,000-day age", got.LastAccess.UTC())
			}
		})
	}
}

// plantSentinelEngram writes an engram and then patches it into the pre-#810
// on-disk state: erf.ZeroTimeSentinelNanos in the LastAccess slot of both the
// 0x01 record and its 0x02 metadata slice, with a recomputed CRC32 so the record
// is indistinguishable from one a pre-fix CloneVaultData produced.
func plantSentinelEngram(t *testing.T, store *PebbleStore, ws [8]byte) (ULID, []byte, uint64) {
	t.Helper()
	ctx := context.Background()
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
	return id, key, sentinel
}

// TestUpdateMetadata_HealsOnlyWhenTheCallerSuppliesALastAccess corrects a claim
// #810 shipped in two places (normalizeEngramTimes' doc and the #811 note at
// WriteLastAccessEntry): "only TouchAccess and UpdateMetadata repair a record".
//
// UpdateMetadata repairs nothing on its own. It is a pass-through — erf.PatchAllMeta
// writes meta.LastAccess verbatim, and a zero time patched in re-encodes to the
// 1754 sentinel. Whether a record heals depends entirely on what the CALLER put
// in the field, and only one of its four callers supplies a fresh clock:
//
//	engram.go   TouchAccess              time.Now()                heals
//	consolidation/dedup.go               representative.LastAccess perpetuates
//	engine/engine.go  Restore            eng.LastAccess            perpetuates
//	storage/lease.go  CompareAndSet      updated := *cur           perpetuates
//
// The three perpetuating callers all pass a value that came from a DECODE, which
// the #810 repair has already turned into the zero time — so they round-trip it
// straight back to the sentinel. The error direction is toward #810's own
// conclusion (the decode-side repair is even more load-bearing than stated), but
// the claim was wrong and a wrong claim is how the next reviewer gets misled.
func TestUpdateMetadata_HealsOnlyWhenTheCallerSuppliesALastAccess(t *testing.T) {
	sentinelRaw := int64(uint64(time.Time{}.UnixNano()))

	t.Run("caller passes a decoded (repaired) LastAccess: perpetuates", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()
		ws := store.VaultPrefix("updatemeta-sentinel")
		id, key, _ := plantSentinelEngram(t, store, ws)

		// Exactly what Restore, dedup and CompareAndSet do: read, carry the
		// decoded LastAccess through unchanged, write.
		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEngram: %v", err)
		}
		if !eng.LastAccess.IsZero() {
			t.Fatalf("decoded LastAccess = %v, want the zero time — the fixture is not in the pre-#810 state", eng.LastAccess)
		}
		if err := store.UpdateMetadata(ctx, ws, id, &EngramMeta{
			State:       StateArchived,
			Confidence:  eng.Confidence,
			Relevance:   eng.Relevance,
			Stability:   eng.Stability,
			AccessCount: eng.AccessCount,
			UpdatedAt:   time.Now(),
			LastAccess:  eng.LastAccess, // the carried-through decode
		}); err != nil {
			t.Fatalf("UpdateMetadata: %v", err)
		}

		after, err := Get(store.db, key)
		if err != nil || after == nil {
			t.Fatalf("get raw after UpdateMetadata: %v", err)
		}
		got := int64(binary.BigEndian.Uint64(after[erf.OffsetLastAccess : erf.OffsetLastAccess+8]))
		if got != sentinelRaw {
			t.Errorf("on-disk LastAccess raw = %d (%v), want the sentinel %d. UpdateMetadata started "+
				"normalizing, so its three carry-through callers (Restore, dedup, CompareAndSet) now heal — "+
				"which is fine, but update normalizeEngramTimes' doc and the #811 note at WriteLastAccessEntry "+
				"in the same change.", got, time.Unix(0, got).UTC(), sentinelRaw)
		}
	})

	t.Run("TouchAccess supplies time.Now(): heals", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()
		ws := store.VaultPrefix("updatemeta-touch")
		id, key, _ := plantSentinelEngram(t, store, ws)

		if err := store.TouchAccess(ctx, ws, id); err != nil {
			t.Fatalf("TouchAccess: %v", err)
		}
		after, err := Get(store.db, key)
		if err != nil || after == nil {
			t.Fatalf("get raw after TouchAccess: %v", err)
		}
		got := int64(binary.BigEndian.Uint64(after[erf.OffsetLastAccess : erf.OffsetLastAccess+8]))
		if got == sentinelRaw {
			t.Fatalf("TouchAccess left the sentinel on disk — the one caller that DOES heal stopped healing")
		}
		if IsUnsetTimestamp(time.Unix(0, got)) {
			t.Errorf("after TouchAccess the on-disk LastAccess is still unset (raw=%d, %v)", got, time.Unix(0, got).UTC())
		}
	})
}
