package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// STO-12 machine check: an association edge may only exist while BOTH of its
// endpoints have a live 0x01 engram record.
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
	eng := NewEngine(EngineConfig{
		Store:            store,
		AuthStore:        auth.NewStore(db),
		FTSIndex:         ftsIdx,
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
func TestSTO12_HardDeletePathsCleanTheSearchIndexes(t *testing.T) {
	const distinctive = "thermocline"

	inFTS := func(t *testing.T, eng *Engine, vault string, ws [8]byte, id storage.ULID) bool {
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

	t.Run("Forget(hard=true)", func(t *testing.T) {
		eng, store, _ := danglingEnv(t)
		vault := "sto12-fts-forget"
		ws := store.VaultPrefix(vault)
		if err := store.WriteVaultName(ws, vault); err != nil {
			t.Fatal(err)
		}
		w := danglingWriter(t, eng, vault, "sampling")
		victim := w("depth profile", "the "+distinctive+" sits near twelve metres")

		if !inFTS(t, eng, vault, ws, victim) {
			t.Fatal("precondition: the engram was never indexed in FTS")
		}
		if _, err := eng.Forget(context.Background(), &mbp.ForgetRequest{
			Vault: vault, ID: victim.String(), Hard: true,
		}); err != nil {
			t.Fatalf("hard forget: %v", err)
		}
		if inFTS(t, eng, vault, ws, victim) {
			t.Error("hard-deleted engram is still a live FTS hit — the hard branch did not clean the index")
		}
	})

	t.Run("PruneVault(MaxEngrams)", func(t *testing.T) {
		eng, store, _ := danglingEnv(t)
		vault := "sto12-fts-prune"
		ws := store.VaultPrefix(vault)
		if err := store.WriteVaultName(ws, vault); err != nil {
			t.Fatal(err)
		}
		w := danglingWriter(t, eng, vault, "sampling")
		var ids []storage.ULID
		for i := 0; i < 5; i++ {
			ids = append(ids, w(fmt.Sprintf("depth profile %d", i),
				fmt.Sprintf("the %s sits near %d metres", distinctive, 10+i)))
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
		var stillIndexed int
		for _, id := range ids {
			if _, err := store.GetEngram(context.Background(), ws, id); err == nil {
				continue // survived the prune
			}
			if inFTS(t, eng, vault, ws, id) {
				stillIndexed++
			}
		}
		if stillIndexed > 0 {
			t.Errorf("%d pruned engram(s) are still live FTS hits — the pruner did not clean the index", stillIndexed)
		}
	})
}
