package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ffNode is one node of a two-node cluster without the network: the leader's
// store hands each replicated batch to a log, and ship applies that log to the
// follower's Pebble the way the replication applier does (the raw repr, then
// the store's commit hooks, as cmd/muninn wires them).
type ffNode struct {
	db    *pebble.DB
	store *storage.PebbleStore
	eng   *Engine
	as    *auth.Store
	mu    sync.Mutex
	log   [][]byte
}

func ffOpen(t *testing.T, name string, fs vfs.FS) *ffNode {
	t.Helper()
	n := &ffNode{}
	opts := storage.DefaultOptions()
	opts.FS = fs
	db, err := storage.OpenPebble(name, opts)
	if err != nil {
		t.Fatal(err)
	}
	n.db = db
	n.store = storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000, RepLogAppend: func(_ uint8, _ []byte, repr []byte) error {
		n.mu.Lock()
		n.log = append(n.log, append([]byte(nil), repr...))
		n.mu.Unlock()
		return nil
	}})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	act := activation.New(n.store, &ftsAdapter{ftsIdx}, nil, embedder)
	trig := trigger.New(n.store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	n.as = auth.NewStore(db)
	n.eng = NewEngine(EngineConfig{Store: n.store, AuthStore: n.as, FTSIndex: ftsIdx, ActivationEngine: act, TriggerSystem: trig, Embedder: embedder})
	return n
}

func (n *ffNode) close(t *testing.T) {
	t.Helper()
	n.eng.Stop()
	if err := n.store.Close(); err != nil {
		t.Fatal(err)
	}
}

func (n *ffNode) ship(t *testing.T, f *ffNode) {
	t.Helper()
	n.mu.Lock()
	log := n.log
	n.log = nil
	n.mu.Unlock()
	for _, repr := range log {
		b := f.db.NewBatch()
		if err := b.SetRepr(append([]byte(nil), repr...)); err != nil {
			t.Fatal(err)
		}
		if err := f.db.Apply(b, pebble.Sync); err != nil {
			t.Fatal(err)
		}
		f.store.InvalidateCommittedRepr(repr)
		f.store.NoteCommittedRepr(repr)
		b.Close()
	}
}

func ffStored(t *testing.T, s *storage.PebbleStore, ws [8]byte) int {
	t.Helper()
	ids, err := s.EngramIDsByCreatedRange(context.Background(), ws, time.Unix(0, 0), time.Now().Add(48*time.Hour), 100000)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// TestVaultCount_FormerFollowerStandaloneKeepsItsMemories: a follower's
// applied records never move its counts. Its clean Close used to certify them
// anyway, and when the node next ran standalone (cluster disabled, or a
// replica's data dir restored on its own) it trusted the stale count and
// pruned real memories: a vault holding 5 under a cap of 5 went to 2.
func TestVaultCount_FormerFollowerStandaloneKeepsItsMemories(t *testing.T) {
	ctx := context.Background()
	const vault = "weir-tide-tables"
	cfg := auth.VaultConfig{Name: vault, Public: true, Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(5)}}
	notLeader := func() error { return errors.New("not leader") }

	L := ffOpen(t, "leader", vfs.NewMem())
	t.Cleanup(func() { L.close(t) })
	ffs := vfs.NewMem()
	F := ffOpen(t, "follower", ffs)
	for _, n := range []*ffNode{L, F} {
		if err := n.as.SetVaultConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	F.eng.SetWriteGate(notLeader)
	var ids []storage.ULID
	for i := 0; i < 8; i++ {
		r, err := L.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: fmt.Sprintf("tide table %d", i), Content: fmt.Sprintf("the tide turned at the weir on day %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		id, err := storage.ParseULID(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ws := L.store.ResolveVaultPrefix(vault)
	L.ship(t, F)
	F.close(t)

	// The follower's next start is a recount, which SAVES every count: it
	// crashed, or it was upgraded from a build without the marker. Removing
	// the marker is that start's precondition, made deterministic.
	db, err := pebble.Open("follower", &pebble.Options{FS: ffs})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete([]byte("\xffvcount_clean"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	F = ffOpen(t, "follower", ffs)
	F.eng.SetWriteGate(notLeader)
	if got := F.store.GetVaultCount(ctx, ws); got != 8 {
		t.Fatalf("fixture: follower count %d after its recount, want 8", got)
	}

	// The leader forgets 3. The follower applies the deletes and stops cleanly.
	for _, id := range ids[:3] {
		if err := L.store.DeleteEngram(ctx, ws, id); err != nil {
			t.Fatal(err)
		}
	}
	L.ship(t, F)
	F.close(t)

	// Restart standalone: no coordinator, no gate, no promotion.
	S := ffOpen(t, "follower", ffs)
	t.Cleanup(func() { S.close(t) })
	if got, stored := S.store.GetVaultCount(ctx, ws), ffStored(t, S.store, ws); got != int64(stored) || stored != 5 {
		t.Errorf("standalone after restart: count %d for a vault holding %d (want 5)", got, stored)
	}
	pruned, err := S.eng.PruneVault(ctx, vault)
	if err != nil {
		t.Fatal(err)
	}
	if left := ffStored(t, S.store, ws); left != 5 {
		t.Fatalf("a vault holding 5 under a cap of 5 was pruned to %d (pruned=%d)", left, pruned)
	}
}

// TestVaultCount_LateDeleteAfterPromotionKeepsRealMemories: a node promoted
// without a drain (a forced promote under write load, or a partition) can
// still apply entries the deposed leader streams after the role changed. Two
// late deletes arrived after a recall had read the vault on the new leader:
// the count read 7 for 5 and the pruner deleted 2 real memories under a cap
// of 5. A foreign commit after promotion now drops the touched vault's
// counter, so the prune reads a fresh scan.
func TestVaultCount_LateDeleteAfterPromotionKeepsRealMemories(t *testing.T) {
	for _, readBeforeLate := range []bool{false, true} {
		t.Run(fmt.Sprintf("readBetweenFlipAndLateEntry=%v", readBeforeLate), func(t *testing.T) {
			ctx := context.Background()
			const vault = "weir-late-entries"
			cfg := auth.VaultConfig{Name: vault, Public: true, Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(5)}}
			L := ffOpen(t, "leader", vfs.NewMem())
			F := ffOpen(t, "follower", vfs.NewMem())
			t.Cleanup(func() { L.close(t); F.close(t) })
			for _, n := range []*ffNode{L, F} {
				if err := n.as.SetVaultConfig(cfg); err != nil {
					t.Fatal(err)
				}
			}
			var leading bool
			F.eng.SetWriteGate(func() error {
				if leading {
					return nil
				}
				return errors.New("not leader")
			})
			var ids []storage.ULID
			for i := 0; i < 7; i++ {
				r, err := L.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: fmt.Sprintf("tide table %d", i), Content: fmt.Sprintf("the tide turned at the weir on day %d", i)})
				if err != nil {
					t.Fatal(err)
				}
				id, err := storage.ParseULID(r.ID)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			ws := L.store.ResolveVaultPrefix(vault)
			L.ship(t, F)
			// The old leader forgets 2 just before it hears the claim. Not shipped yet.
			for _, id := range ids[:2] {
				if err := L.store.DeleteEngram(ctx, ws, id); err != nil {
					t.Fatal(err)
				}
			}
			F.store.ResetAllVaultCounts() // BeforeBecomingCortex
			leading = true                // the role flips
			if readBeforeLate {
				_ = F.store.GetVaultCount(ctx, ws) // a recall or stats read on the new leader
			}
			L.ship(t, F) // the late entries
			if _, err := F.eng.PruneVault(ctx, vault); err != nil {
				t.Fatal(err)
			}
			if left := ffStored(t, F.store, ws); left != 5 {
				t.Fatalf("a vault holding 5 under a cap of 5 was pruned to %d on the new leader", left)
			}
		})
	}
}
