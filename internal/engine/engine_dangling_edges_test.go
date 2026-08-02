package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// STO-12 machine check: every association writer REFUSES an edge unless BOTH of
// its endpoints have a live 0x01 engram record. A refusal at each writer, not an
// unrepresentable state — see the invariant entry for the measured residual.
//
// Each sub-case drives a real delete path through the real engine and then
// resumes ordinary use, because the leak this pins is not in the delete itself
// — DeleteEngram's own cascade is complete — but in what the SEARCH INDEXES
// keep handing to the association writers afterwards.
//
// Deliberately a unit-scale test on tiny vaults: the census below is O(edges)
// over a handful of rows, milliseconds per case. The O(vault) equivalent
// belongs in a maintenance/repair pass, not in the gate.

// danglingCensus walks 0x03 / 0x04 / 0x14 / 0x25 for a vault and returns every
// row with an endpoint that has no 0x01 engram record.
func danglingCensus(t *testing.T, db *pebble.DB, ws [8]byte) []string {
	t.Helper()

	live := func(id [16]byte) bool {
		_, closer, err := db.Get(keys.EngramKey(ws, id))
		if err != nil {
			return false
		}
		_ = closer.Close()
		return true
	}

	var out []string
	scan := func(label string, p byte, wantLen int, srcAt, dstAt int) {
		lo := append([]byte{p}, ws[:]...)
		it, err := db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: keys.PrefixUpperBound(lo)})
		if err != nil {
			t.Fatalf("iter %s: %v", label, err)
		}
		defer it.Close()
		for it.First(); it.Valid(); it.Next() {
			k := it.Key()
			if len(k) < wantLen {
				continue
			}
			var src, dst [16]byte
			copy(src[:], k[srcAt:srcAt+16])
			copy(dst[:], k[dstAt:dstAt+16])
			if !live(src) {
				out = append(out, fmt.Sprintf("%s: source %s has no engram", label, storage.ULID(src)))
			}
			if !live(dst) {
				out = append(out, fmt.Sprintf("%s: target %s has no engram", label, storage.ULID(dst)))
			}
		}
	}
	// 0x03: ws | src(16) | weightComplement(4) | dst(16)
	scan("0x03 assoc-fwd", 0x03, 45, 9, 29)
	// 0x04: ws | dst(16) | weightComplement(4) | src(16)
	scan("0x04 assoc-rev", 0x04, 45, 29, 9)
	// 0x14: ws | src(16) | dst(16)
	scan("0x14 weight-index", 0x14, 41, 9, 25)
	// 0x25: ws | src(16) | dst(16) — an archived edge to a dead engram is a
	// dangle in waiting: recall's lazy restore writes it back live.
	scan("0x25 assoc-archive", 0x25, 41, 9, 25)
	return out
}

func danglingEnv(t *testing.T) (*Engine, *storage.PebbleStore, *pebble.DB) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-sto31-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	// A real HNSW registry, not nil: the hard-delete cascade's vector-index half
	// (TombstoneNode) is unobservable without one.
	hnswReg := hnsw.NewRegistry(db)
	eng := NewEngine(EngineConfig{
		Store:            store,
		AuthStore:        auth.NewStore(db),
		FTSIndex:         ftsIdx,
		HNSWRegistry:     hnswReg,
		ActivationEngine: activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder),
		TriggerSystem:    trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder),
		Embedder:         embedder,
	})
	t.Cleanup(func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	})
	return eng, store, db
}

func TestSTO12_NoDanglingAssociationEdges(t *testing.T) {
	const sharedTag = "quarterly-planning"

	cases := []struct {
		name string
		// run drives a delete path and then resumes ordinary use.
		run func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte)
	}{
		{
			// Leak 1: Engine.Forget's HARD branch never cleaned the FTS index
			// (fts.DeleteEngram was called on the soft branch only), so
			// autoassoc's tag search kept returning the dead ID and minting a
			// RelRelatesTo edge to it on every later write sharing a tag.
			name: "hard forget then ordinary writes on the same tag",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				w := danglingWriter(t, eng, vault, sharedTag)
				victim := w("archive rotation policy", "rotate the planning archive every quarter")
				w("seed neighbour", "planning notes that keep the tag alive")

				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
					Vault: vault, ID: victim.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
				for i := 0; i < 4; i++ {
					w(fmt.Sprintf("follow-up note %d", i), fmt.Sprintf("more quarterly planning detail %d", i))
				}
			},
		},
		{
			// Leak 1, growth arm: the leak was monotone in the number of later
			// writes (5 dangling after 5 writes, 10 after 10, 14 after 15) and
			// nothing reaped it — DecayAssocWeights never reads 0x01, so 24
			// passes with real `default` preset parameters deleted zero.
			name: "hard forget then sustained use, with association decay running",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				ctx := context.Background()
				w := danglingWriter(t, eng, vault, sharedTag)
				victim := w("archive rotation policy", "rotate the planning archive every quarter")
				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
					Vault: vault, ID: victim.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
				for i := 0; i < 10; i++ {
					w(fmt.Sprintf("note %d", i), fmt.Sprintf("quarterly planning detail %d", i))
				}
				// `default` preset parameters, exactly what runPruneWorker passes.
				for i := 0; i < 3; i++ {
					if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.05); err != nil {
						t.Fatalf("DecayAssocWeights: %v", err)
					}
				}
			},
		},
		{
			// Leak 3: PruneVault calls DeleteEngram directly and cleaned
			// NEITHER FTS nor HNSW, so the pruner leaked through both
			// amplifiers with no operator action at all.
			name: "background pruner (MaxEngrams) then ordinary writes",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				ctx := context.Background()
				w := danglingWriter(t, eng, vault, sharedTag)
				for i := 0; i < 6; i++ {
					w(fmt.Sprintf("seed %d", i), fmt.Sprintf("quarterly planning seed content %d", i))
				}
				if err := eng.authStore.SetVaultConfig(auth.VaultConfig{
					Name: vault, Public: true,
					Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(2)},
				}); err != nil {
					t.Fatalf("SetVaultConfig: %v", err)
				}
				if _, err := eng.PruneVault(ctx, vault); err != nil {
					t.Fatalf("PruneVault: %v", err)
				}
				for i := 0; i < 4; i++ {
					w(fmt.Sprintf("post-prune %d", i), fmt.Sprintf("quarterly planning after prune %d", i))
				}
			},
		},
		{
			// Leak 2: DeleteEngram's cascade scanned 0x03/0x04 only, so an edge
			// ARCHIVED into 0x25 before its target was hard-deleted survived,
			// and recall's lazy RestoreArchivedEdges wrote it back live —
			// stamping restoredAt, which permanently exempts it from
			// GCArchivedEdges. FTS-independent: fixing leak 1 does not fix it.
			name: "archived edge whose target is hard-deleted, then recall restores",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				ctx := context.Background()
				w := danglingWriter(t, eng, vault, sharedTag)
				target := w("archive rotation policy", "rotate the planning archive every quarter")
				source := w("seed neighbour", "planning notes that keep the tag alive")

				// Force the live edge into the 0x25 archive.
				if _, err := store.DecayAssocWeights(ctx, ws, time.Nanosecond, 0.001, 0.05); err != nil {
					t.Fatalf("archiving decay: %v", err)
				}
				if countPrefix(t, db, ws, 0x25) == 0 {
					t.Fatal("precondition: no edge reached the 0x25 archive")
				}
				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
					Vault: vault, ID: target.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
				// The lazy restore recall performs per candidate.
				if _, err := store.RestoreArchivedEdgesTransitive(ctx, ws, source, 10, 5); err != nil {
					t.Fatalf("RestoreArchivedEdgesTransitive: %v", err)
				}
			},
		},
		{
			// Leak 4 (STO-11 inside the cascade): a 0x03 key is
			// prefix(25)|weightComplement(4)|dst(16) and the complement is
			// MaxUint32 - uint32(w*MaxUint32), so complement[0] == 0xFF for every
			// weight at or below ~1/256. DeleteEngram's forward and reverse
			// scans bounded themselves with a naive appended 0xFF, which sorts
			// at or below all of those keys — so the cascade never saw them.
			// Nothing clamps Weight on the way in, so `weight: 0.001` on an
			// ordinary muninn_link / REST link is all it takes.
			name: "client link below the 1/256 weight boundary, then hard forget",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				ctx := context.Background()
				w := danglingWriter(t, eng, vault, sharedTag)
				source := w("seed neighbour", "planning notes that keep the tag alive")
				target := w("archive rotation policy", "rotate the planning archive every quarter")

				if _, err := eng.Link(ctx, &mbp.LinkRequest{
					Vault: vault, SourceID: source.String(), TargetID: target.String(),
					RelType: 1, Weight: 0.001,
				}); err != nil {
					t.Fatalf("Link at weight 0.001: %v", err)
				}
				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
					Vault: vault, ID: target.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
			},
		},
		{
			// Leak 4, legacy arm. Complement 0xFFFFFFFF is both the weight-0
			// position AND the position every PRE-FIX weight-1.0 edge was
			// written at (legacyFullWeightComplement). Those rows are on disk in
			// real vaults, the startup repair pass exists because of them, and
			// that pass scans with a CORRECT bound — so a cascade that cannot
			// reach the legacy position hands the repair a stranded row to
			// promote into a fully-reachable dangling edge at the true 1.0
			// position. Both halves are exercised: hard-delete, then repair.
			name: "legacy full-weight edge, hard forget, then the startup repair pass",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, db *pebble.DB, vault string, ws [8]byte) {
				ctx := context.Background()
				w := danglingWriter(t, eng, vault, sharedTag)
				source := w("seed neighbour", "planning notes that keep the tag alive")
				target := w("archive rotation policy", "rotate the planning archive every quarter")

				seedLegacyFullWeightRows(t, db, ws, source, target)
				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
					Vault: vault, ID: target.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
				if _, err := store.RepairLegacyFullWeightAssocKeys(ctx, ws); err != nil {
					t.Fatalf("RepairLegacyFullWeightAssocKeys: %v", err)
				}
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, store, db := danglingEnv(t)
			vault := fmt.Sprintf("sto31-%d", i)
			ws := store.VaultPrefix(vault)
			if err := store.WriteVaultName(ws, vault); err != nil {
				t.Fatal(err)
			}

			tc.run(t, eng, store, db, vault, ws)
			eng.WaitWriteTimeIdle()

			if bad := danglingCensus(t, db, ws); len(bad) > 0 {
				t.Errorf("STO-12 violated: %d dangling association row(s)\n  %s",
					len(bad), joinLines(bad))
			}
		})
	}
}

// TestSTO12_InlineAssociationsCannotNameADeadEngram is the SURFACE-REACHABLE
// arm of the machine check.
//
// mbp.WriteRequest has two distinct inline relationship fields and they take
// different routes into the keyspace:
//
//   - Relationships → Engine.Write's post-write loop → store.WriteAssociation,
//     which has been guarded since #803's first round;
//   - Associations  → eng.Associations → store.WriteEngram / WriteEngramBatch,
//     whose inline loops Set 0x03/0x04/0x14 directly and were NOT guarded.
//
// The second is reachable by an ordinary client over REST (WriteRequest is a
// type alias of mbp.WriteRequest), gRPC, MBP and the embedded library — no
// hostile actor required, just a client that holds an ID across a delete. Only
// MCP escapes it, because muninn_link and muninn_remember use `relationships`.
//
// Asserted against the same danglingCensus as the delete-path table so the two
// layers are pinned by the same definition of the invariant.
func TestSTO12_InlineAssociationsCannotNameADeadEngram(t *testing.T) {
	ctx := context.Background()

	// deadTarget writes an engram, hard-deletes it, and returns its ID — a
	// perfectly well-formed ULID with no 0x01 record behind it.
	deadTarget := func(t *testing.T, eng *Engine, vault string) string {
		t.Helper()
		w := danglingWriter(t, eng, vault, "retention")
		victim := w("superseded rota", "the old duty rota nobody uses any more")
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: victim.String(), Hard: true}); err != nil {
			t.Fatalf("hard forget: %v", err)
		}
		return victim.String()
	}

	t.Run("Engine.Write", func(t *testing.T) {
		eng, store, db := danglingEnv(t)
		vault := "sto12-inline-write"
		ws := store.VaultPrefix(vault)
		if err := store.WriteVaultName(ws, vault); err != nil {
			t.Fatal(err)
		}
		dead := deadTarget(t, eng, vault)

		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: vault, Concept: "replacement rota", Content: "the duty rota that replaced it",
			Associations: []mbp.Association{{TargetID: dead, RelType: 1, Weight: 0.8, Confidence: 1}},
		})
		if err == nil {
			t.Fatal("Engine.Write ACCEPTED an inline association naming a hard-deleted engram")
		}
		// A client naming an ID that no longer exists is a bad request, not a
		// storage fault: REST maps ErrInvalidID to 400.
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("want the refusal to reach the transports as ErrInvalidID (400), got %v", err)
		}
		eng.WaitWriteTimeIdle()
		if bad := danglingCensus(t, db, ws); len(bad) > 0 {
			t.Errorf("STO-12 violated via the inline-association path: %d dangling row(s)\n  %s",
				len(bad), joinLines(bad))
		}
	})

	t.Run("Engine.WriteBatch", func(t *testing.T) {
		eng, store, db := danglingEnv(t)
		vault := "sto12-inline-batch"
		ws := store.VaultPrefix(vault)
		if err := store.WriteVaultName(ws, vault); err != nil {
			t.Fatal(err)
		}
		dead := deadTarget(t, eng, vault)

		// Item 0 is innocent and must still land; only item 1 is refused.
		_, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
			{Vault: vault, Concept: "handover note", Content: "who is covering the shift"},
			{Vault: vault, Concept: "replacement rota", Content: "the duty rota that replaced it",
				Associations: []mbp.Association{{TargetID: dead, RelType: 1, Weight: 0.8, Confidence: 1}}},
		})
		if errs[0] != nil {
			t.Errorf("an innocent batch item must not be collateral: %v", errs[0])
		}
		if errs[1] == nil {
			t.Fatal("Engine.WriteBatch ACCEPTED an inline association naming a hard-deleted engram")
		}
		eng.WaitWriteTimeIdle()
		if bad := danglingCensus(t, db, ws); len(bad) > 0 {
			t.Errorf("STO-12 violated via the batched inline-association path: %d dangling row(s)\n  %s",
				len(bad), joinLines(bad))
		}
	})
}

func danglingWriter(t *testing.T, eng *Engine, vault, tag string) func(concept, content string) storage.ULID {
	t.Helper()
	return func(concept, content string) storage.ULID {
		t.Helper()
		resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
			Vault: vault, Concept: concept, Content: content, Tags: []string{tag},
		})
		if err != nil {
			t.Fatalf("write %q: %v", concept, err)
		}
		// Deterministic drain of the write-time workers (autoassoc, neighbor,
		// goal-link) — never a sleep. See docs/internals/testing-hermeticity.md.
		eng.WaitWriteTimeIdle()
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
}

// seedLegacyFullWeightRows writes the three keys a PRE-FIX weight-1.0 edge left
// on disk: 0x03 and 0x04 at complement 0xFFFFFFFF (the position the old encoder
// produced for weight exactly 1.0), and a 0x14 index holding a true 1.0. That
// disagreement is what RepairLegacyFullWeightAssocKeys scans for, and the keys
// are written raw because no current encoder can produce that combination.
func seedLegacyFullWeightRows(t *testing.T, db *pebble.DB, ws [8]byte, src, dst storage.ULID) {
	t.Helper()
	legacy := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	key := func(p byte, a, b storage.ULID) []byte {
		k := make([]byte, 0, 45)
		k = append(k, p)
		k = append(k, ws[:]...)
		k = append(k, a[:]...)
		k = append(k, legacy...)
		k = append(k, b[:]...)
		return k
	}
	val := make([]byte, 26)
	var idxVal [4]byte
	binary.BigEndian.PutUint32(idxVal[:], math.Float32bits(1.0))
	b := db.NewBatch()
	defer b.Close()
	_ = b.Set(key(0x03, src, dst), val, nil)
	_ = b.Set(key(0x04, dst, src), val, nil)
	_ = b.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), idxVal[:], nil)
	if err := b.Commit(pebble.NoSync); err != nil {
		t.Fatalf("seed legacy full-weight rows: %v", err)
	}
}

func countPrefix(t *testing.T, db *pebble.DB, ws [8]byte, p byte) int {
	t.Helper()
	lo := append([]byte{p}, ws[:]...)
	it, err := db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: keys.PrefixUpperBound(lo)})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n := 0
	for it.First(); it.Valid(); it.Next() {
		n++
	}
	return n
}

func joinLines(s []string) string {
	var b bytes.Buffer
	for i, l := range s {
		if i > 0 {
			b.WriteString("\n  ")
		}
		b.WriteString(l)
	}
	return b.String()
}

// TestSTO12_HardDeletePathsCleanTheSearchIndexes is the independent RED for
// LAYER ONE, the cascades.
//
// The endpoint guard (layer two) absorbs the dangling-edge symptom of a dirty
// FTS index, so TestSTO12_NoDanglingAssociationEdges stays green if only the
// cascade regresses — which would leave a real defect standing: a hard-deleted
// engram remains a live search hit, wasting every amplifier's work on an ID
// that can never be linked, and (before this fix) taking the HNSW tombstone
// with it on the prune path. This asserts the cascade directly.
// Both amplifiers are asserted, and both arms of PruneVault: the MaxEngrams arm
// and the RetentionDays arm are separate loops with separate cleanup call sites,
// so covering one leaves the other free to regress silently.
func TestSTO12_HardDeletePathsCleanTheSearchIndexes(t *testing.T) {
	const distinctive = "thermocline"

	inFTS := func(t *testing.T, eng *Engine, ws [8]byte, id storage.ULID) bool {
		t.Helper()
		hits, err := eng.fts.Search(context.Background(), ws, distinctive, 50)
		if err != nil {
			t.Fatalf("fts search: %v", err)
		}
		for _, h := range hits {
			if h.ID == [16]byte(id) {
				return true
			}
		}
		return false
	}

	// write puts the engram in BOTH indexes: FTS via the ordinary write path,
	// HNSW via the caller-supplied embedding (Engine.Write inserts it inline).
	write := func(t *testing.T, eng *Engine, vault, concept, content string, createdAt *time.Time) storage.ULID {
		t.Helper()
		vec := make([]float32, 384)
		vec[0] = 1
		resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
			Vault: vault, Concept: concept, Content: content,
			Tags: []string{"sampling"}, Embedding: vec, CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("write %q: %v", concept, err)
		}
		eng.WaitWriteTimeIdle()
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	cases := []struct {
		name string
		// run seeds the vault and drives one hard-delete caller, returning every
		// id it seeded. Survivors are filtered out by the assertions.
		run func(t *testing.T, eng *Engine, store *storage.PebbleStore, vault string) []storage.ULID
	}{
		{
			name: "Forget(hard=true)",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, vault string) []storage.ULID {
				victim := write(t, eng, vault, "depth profile", "the "+distinctive+" sits near twelve metres", nil)
				if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
					Vault: vault, ID: victim.String(), Hard: true,
				}); err != nil {
					t.Fatalf("hard forget: %v", err)
				}
				return []storage.ULID{victim}
			},
		},
		{
			name: "PruneVault(MaxEngrams)",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, vault string) []storage.ULID {
				var ids []storage.ULID
				for i := 0; i < 5; i++ {
					ids = append(ids, write(t, eng, vault, fmt.Sprintf("depth profile %d", i),
						fmt.Sprintf("the %s sits near %d metres", distinctive, 10+i), nil))
				}
				if err := eng.authStore.SetVaultConfig(auth.VaultConfig{
					Name: vault, Public: true,
					Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(1)},
				}); err != nil {
					t.Fatalf("SetVaultConfig: %v", err)
				}
				if _, err := eng.PruneVault(context.Background(), vault); err != nil {
					t.Fatalf("PruneVault: %v", err)
				}
				return ids
			},
		},
		{
			// The retention arm is a SECOND, independent delete loop with its
			// own cleanup call site. Age comes from an explicit created_at (the
			// ULID carries it, which is what EngramIDsByCreatedRange scans) —
			// never from waiting on a clock.
			name: "PruneVault(RetentionDays)",
			run: func(t *testing.T, eng *Engine, store *storage.PebbleStore, vault string) []storage.ULID {
				old := time.Now().AddDate(0, 0, -90)
				var ids []storage.ULID
				for i := 0; i < 3; i++ {
					ids = append(ids, write(t, eng, vault, fmt.Sprintf("archived profile %d", i),
						fmt.Sprintf("the %s sat near %d metres", distinctive, 10+i), &old))
				}
				if err := eng.authStore.SetVaultConfig(auth.VaultConfig{
					Name: vault, Public: true,
					Plasticity: &auth.PlasticityConfig{RetentionDays: ptr[float32](30)},
				}); err != nil {
					t.Fatalf("SetVaultConfig: %v", err)
				}
				if _, err := eng.PruneVault(context.Background(), vault); err != nil {
					t.Fatalf("PruneVault: %v", err)
				}
				return ids
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, store, _ := danglingEnv(t)
			vault := fmt.Sprintf("sto12-search-idx-%d", i)
			ws := store.VaultPrefix(vault)
			if err := store.WriteVaultName(ws, vault); err != nil {
				t.Fatal(err)
			}

			ids := tc.run(t, eng, store, vault)

			var deleted, stillFTS, notTombstoned int
			for _, id := range ids {
				if _, err := store.GetEngram(context.Background(), ws, id); err == nil {
					continue // survived
				}
				deleted++
				if inFTS(t, eng, ws, id) {
					stillFTS++
				}
				if !eng.hnswRegistry.IsTombstoned(ws, [16]byte(id)) {
					notTombstoned++
				}
			}
			if deleted == 0 {
				t.Fatal("precondition: this delete path removed nothing")
			}
			if stillFTS > 0 {
				t.Errorf("%d hard-deleted engram(s) are still live FTS hits — the caller did not clean the index", stillFTS)
			}
			if notTombstoned > 0 {
				t.Errorf("%d hard-deleted engram(s) are NOT tombstoned in HNSW — they stay live vector hits, "+
					"and the neighbor worker turns each one back into an association edge", notTombstoned)
			}
		})
	}
}
