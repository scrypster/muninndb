package storage

import (
	"context"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ---------------------------------------------------------------------------
// The cache never serves a value older than the last committed write.
//
// Each test below lands a committed write AFTER a reader has loaded its value
// from Pebble (or pinned the snapshot it will load from) but BEFORE that
// reader installs the value in a cache. The interleaving is driven explicitly:
// either by a snapshot pinned before the write (the reader's view predates
// the write by construction) or by the fill seam (cache_fill_seam.go), which
// runs the write at exactly the point between load and fill. No sleeps.
// ---------------------------------------------------------------------------

// writeProbe stores one engram in a fresh vault and returns its coordinates.
// The engram is not cached afterwards (WriteEngram never caches).
func writeProbe(t *testing.T, s *PebbleStore, vault string, relevance float32) ([8]byte, ULID) {
	t.Helper()
	ws := s.VaultPrefix(vault)
	id, err := s.WriteEngram(context.Background(), ws, &Engram{
		Concept:    "boardwalk span " + vault,
		Content:    "replaced two rotten stringers on the marsh boardwalk",
		Confidence: 0.7,
		Relevance:  relevance,
		Stability:  30,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	s.cache.Delete(ws, id)
	s.metaCache.Remove([16]byte(id))
	return ws, id
}

// fireOnceFor returns a fill hook that runs write the first time a read for
// (ws, id) reaches its fill point, and ignores every other call (including
// the write's own internal reads, and any parallel test's reads).
func fireOnceFor(ws [8]byte, id ULID, write func()) func([8]byte, ULID) {
	var fired atomic.Bool
	return func(hws [8]byte, hid ULID) {
		if hws != ws || hid != id {
			return
		}
		if fired.CompareAndSwap(false, true) {
			write()
		}
	}
}

// Reproduction 1: a write commits during a recall; phase 6's snapshot batch
// read then fills the cache with the older value. Afterwards a plain read
// must see the committed value.
func TestCacheStaleFill_SnapshotBatchReadDoesNotOverwriteNewerCommit(t *testing.T) {
	for _, noStamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("noStamp=%v", noStamp), func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			ws, id := writeProbe(t, s, fmt.Sprintf("stale-snap-%v", noStamp), 0.20)

			snapCtx, snap := s.NewSnapshotContext(ctx)
			defer snap.Close()
			if noStamp {
				snapCtx = ContextWithNoAccessCacheStamp(snapCtx)
			}

			// The write commits after the recall pinned its view.
			if err := s.UpdateRelevance(ctx, ws, id, 0.90, 30); err != nil {
				t.Fatalf("UpdateRelevance: %v", err)
			}

			got, err := s.GetEngrams(snapCtx, ws, []ULID{id})
			if err != nil || got[0] == nil {
				t.Fatalf("GetEngrams(snapshot): %v %v", got, err)
			}
			if got[0].Relevance != 0.20 {
				t.Fatalf("precondition: the snapshot read should see the pre-write value 0.20, got %v", got[0].Relevance)
			}

			after, err := s.GetEngram(ctx, ws, id)
			if err != nil {
				t.Fatalf("GetEngram: %v", err)
			}
			if after.Relevance != 0.90 {
				t.Fatalf("stale cache fill: GetEngram returned relevance %v after a committed write of 0.90 "+
					"(the snapshot batch read re-cached the older value)", after.Relevance)
			}
		})
	}
}

// The same class on the metadata-only read: GetMetadata under a snapshot
// fills metaCache with the pre-write metadata.
func TestCacheStaleFill_SnapshotMetadataReadDoesNotOverwriteNewerCommit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "stale-meta", 0.20)

	snapCtx, snap := s.NewSnapshotContext(ctx)
	defer snap.Close()

	if err := s.UpdateRelevance(ctx, ws, id, 0.90, 30); err != nil {
		t.Fatalf("UpdateRelevance: %v", err)
	}
	if _, err := s.GetMetadata(snapCtx, ws, []ULID{id}); err != nil {
		t.Fatalf("GetMetadata(snapshot): %v", err)
	}
	metas, err := s.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || metas[0] == nil {
		t.Fatalf("GetMetadata: %v %v", metas, err)
	}
	if metas[0].Relevance != 0.90 {
		t.Fatalf("stale metaCache fill: GetMetadata returned relevance %v after a committed write of 0.90", metas[0].Relevance)
	}
}

// Reproduction 2: a use-count bump commits between a concurrent GetEngram's
// Pebble load and its cache fill. Later readers must never see the count go
// backwards.
func TestCacheStaleFill_AccessBumpDuringGetEngramNeverGoesBackwards(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "stale-touch", 0.5)

	before, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	base := before.AccessCount
	s.cache.Delete(ws, id)

	restore := setFillHookForTest(fireOnceFor(ws, id, func() {
		if err := s.TouchAccess(ctx, ws, id); err != nil {
			t.Errorf("TouchAccess: %v", err)
		}
	}))
	defer restore()

	// This read loads the pre-bump record, then the bump commits, then the
	// read fills. It may itself return the pre-bump value — it started first.
	if _, err := s.GetEngram(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	restore()

	after, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccessCount != base+1 {
		t.Fatalf("stale cache fill: AccessCount reads %d after a committed bump to %d (count went backwards for a later reader)",
			after.AccessCount, base+1)
	}
	metas, err := s.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || metas[0] == nil {
		t.Fatal(err)
	}
	if metas[0].AccessCount != base+1 {
		t.Fatalf("stale metadata: AccessCount reads %d via GetMetadata, want %d", metas[0].AccessCount, base+1)
	}
}

// Reproduction 3 (storage half): a forget commits while a recall's snapshot
// batch read is between its Pebble load and its fill. The memory must read as
// forgotten afterwards, through every read path.
func TestCacheStaleFill_ForgetDuringSnapshotReadSticks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "stale-forget", 0.5)

	snapCtx, snap := s.NewSnapshotContext(ctx)
	defer snap.Close()

	restore := setFillHookForTest(fireOnceFor(ws, id, func() {
		if err := s.SoftDelete(ctx, ws, id); err != nil {
			t.Errorf("SoftDelete: %v", err)
		}
	}))
	defer restore()
	if _, err := s.GetEngrams(snapCtx, ws, []ULID{id}); err != nil {
		t.Fatal(err)
	}
	restore()

	eng, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if eng.State != StateSoftDeleted {
		t.Fatalf("forget did not stick: GetEngram reports state %v after a committed soft-delete", eng.State)
	}
	metas, err := s.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || metas[0] == nil {
		t.Fatal(err)
	}
	if metas[0].State != StateSoftDeleted {
		t.Fatalf("forget did not stick: GetMetadata reports state %v", metas[0].State)
	}
}

// A writer's own batch (evolve's SupersedeEngram path) committing between a
// GetEngram load and its fill.
func TestCacheStaleFill_BatchSupersedeDuringGetEngram(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "stale-evolve", 0.5)

	restore := setFillHookForTest(fireOnceFor(ws, id, func() {
		b := s.NewBatch()
		if err := b.SupersedeEngram(ctx, ws, id, time.Now()); err != nil {
			t.Errorf("SupersedeEngram: %v", err)
			return
		}
		if err := b.Commit(); err != nil {
			t.Errorf("Commit: %v", err)
		}
	}))
	defer restore()
	if _, err := s.GetEngram(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	restore()

	eng, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if eng.State != StateSoftDeleted || eng.ValidUntil.IsZero() {
		t.Fatalf("stale cache fill after a committed batch supersede: state=%v validUntil=%v", eng.State, eng.ValidUntil)
	}
}

// ---------------------------------------------------------------------------
// Randomized stress. Many goroutines write (confidence, access bump,
// forget/restore, relevance, trust, batch supersede/state) and read (single,
// snapshot batch, snapshot batch without stamping, metadata live and under a
// snapshot) on a small key set. At quiescence every cached entry — L1 and
// metaCache — must equal what Pebble holds.
// ---------------------------------------------------------------------------

func TestCacheStaleFill_RandomizedStress_CacheEqualsStoreAtQuiescence(t *testing.T) {
	rounds := 60
	if testing.Short() {
		rounds = 15
	}
	s := openTestStore(t)
	ctx := context.Background()
	ws := s.VaultPrefix("stale-stress")

	const nKeys = 6
	ids := make([]ULID, nKeys)
	for i := range ids {
		id, err := s.WriteEngram(ctx, ws, &Engram{
			Concept:    fmt.Sprintf("trail marker %d", i),
			Content:    "repainted the blaze on the ridge trail marker post",
			Confidence: 0.5,
			Relevance:  0.5,
			Stability:  30,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	// A ring of edges so the association caches are exercised too.
	for i := range ids {
		if err := s.WriteAssociation(ctx, ws, ids[i], ids[(i+1)%nKeys], &Association{
			TargetID: ids[(i+1)%nKeys], Weight: 0.5, RelType: RelRelatesTo, Confidence: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	const writers, readers, opsPer = 4, 6, 40
	for round := 0; round < rounds; round++ {
		// Readers run for as long as the writers do, so the LAST write to
		// each key is overlapped by readers — only a stale fill that lands
		// after a key's final write survives to the quiescence check.
		var wg, rwg sync.WaitGroup
		var writersDone atomic.Bool
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				r := rand.New(rand.NewSource(seed))
				for i := 0; i < opsPer; i++ {
					id := ids[r.Intn(nKeys)]
					switch r.Intn(8) {
					case 7:
						j := r.Intn(nKeys)
						_ = s.UpdateAssocWeight(ctx, ws, ids[j], ids[(j+1)%nKeys], 0.05+0.9*r.Float32(), 0)
					case 0:
						_ = s.UpdateConfidence(ctx, ws, id, r.Float32())
					case 1:
						_ = s.TouchAccess(ctx, ws, id)
					case 2:
						_ = s.SoftDelete(ctx, ws, id)
					case 3:
						// restore: UpdateMetadata back to active (Engine.Restore's write)
						unlock := s.LockEngram(id)
						if e, err := s.GetEngram(ctx, ws, id); err == nil {
							_ = s.UpdateMetadata(ctx, ws, id, &EngramMeta{
								State: StateActive, Confidence: e.Confidence, Relevance: e.Relevance,
								Stability: e.Stability, AccessCount: e.AccessCount,
								UpdatedAt: time.Now(), LastAccess: e.LastAccess,
							})
						}
						unlock()
					// UpdateRelevance, UpdateTrust and the StoreBatch mutate path
					// take no stripe lock, so racing a locked writer they can
					// lose its update IN PEBBLE (record vs 0x0B index) with no
					// cache involved — a separate, pre-existing write-write race.
					// They are serialized here so this test measures the cache.
					case 4:
						unlock := s.LockEngram(id)
						_ = s.UpdateRelevance(ctx, ws, id, r.Float32(), 30)
						unlock()
					case 5:
						unlock := s.LockEngram(id)
						_ = s.UpdateTrust(ctx, ws, id, TrustLevel(r.Intn(3)))
						unlock()
					case 6:
						unlock := s.LockEngram(id)
						b := s.NewBatch()
						var err error
						if r.Intn(2) == 0 {
							err = b.SupersedeEngram(ctx, ws, id, time.Now())
						} else {
							err = b.UpdateEngramState(ctx, ws, id, StateActive)
						}
						if err == nil {
							_ = b.Commit()
						} else {
							b.Discard()
						}
						unlock()
					}
				}
			}(int64(round*1000 + w))
		}
		for rd := 0; rd < readers; rd++ {
			rwg.Add(1)
			go func(seed int64) {
				defer rwg.Done()
				r := rand.New(rand.NewSource(seed))
				for !writersDone.Load() {
					switch r.Intn(5) {
					case 0:
						_, _ = s.GetEngram(ctx, ws, ids[r.Intn(nKeys)])
					case 1, 2:
						sctx, snap := s.NewSnapshotContext(ctx)
						if r.Intn(2) == 0 {
							sctx = ContextWithNoAccessCacheStamp(sctx)
						}
						// A recall does other work between pinning and phase 6
						// (seeding, traversal); yield so writes land in between.
						_, _ = s.GetMetadata(sctx, ws, ids)
						runtime.Gosched()
						_, _ = s.GetEngrams(sctx, ws, ids)
						_, _ = s.GetAssociations(sctx, ws, ids, 0)
						snap.Close()
					case 3:
						_, _ = s.GetMetadata(ctx, ws, ids[:1+r.Intn(nKeys)])
					case 4:
						s.cache.Delete(ws, ids[r.Intn(nKeys)]) // eviction pressure
					}
				}
			}(int64(round*1000 + 500 + rd))
		}
		wg.Wait()
		writersDone.Store(true)
		rwg.Wait()

		if diffs := cacheVsStore(t, s, ws, ids); len(diffs) > 0 {
			t.Fatalf("round %d: cache diverged from committed store at quiescence:\n%s", round, joinLines(diffs))
		}
	}
}

// cacheVsStore compares every cached L1 entry and metaCache entry for ids
// against a direct decode of the committed Pebble records.
func cacheVsStore(t *testing.T, s *PebbleStore, ws [8]byte, ids []ULID) []string {
	t.Helper()
	var diffs []string
	for _, id := range ids {
		if st := stateIndexStates(t, s, ws, id); len(st) != 1 {
			diffs = append(diffs, fmt.Sprintf("state index %s: holds %v (want exactly the record's state)", id, st))
		}
		if e, ok := s.assocCache.Peek(assocCacheKey(ws, id)); ok {
			want := committedFwdWeights(t, s, ws, id)
			got := make([]float32, len(e.assocs))
			for i, a := range e.assocs {
				got[i] = a.Weight
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				diffs = append(diffs, fmt.Sprintf("assoc %s: cached weights %v, store %v", id, got, want))
			}
		}
		raw, err := Get(s.db, keys.EngramKey(ws, [16]byte(id)))
		if err != nil || raw == nil {
			t.Fatalf("read committed record: %v", err)
		}
		dec, err := erf.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := fromERFEngram(dec)
		if st := stateIndexStates(t, s, ws, id); len(st) == 1 && st[0] != want.State {
			diffs = append(diffs, fmt.Sprintf("state index %s: index says %v, record says %v", id, st[0], want.State))
		}
		if got, ok := s.cache.GetNoStamp(ws, id); ok {
			if d := engramStateDiff(got, want); d != "" {
				diffs = append(diffs, "L1 "+id.String()+": "+d)
			}
		}
		if m, ok := s.metaCache.Peek([16]byte(id)); ok {
			if m.State != want.State || m.Confidence != want.Confidence || m.Relevance != want.Relevance ||
				m.AccessCount != want.AccessCount || m.Trust != want.Trust {
				diffs = append(diffs, fmt.Sprintf("meta %s: cached{state=%v conf=%v rel=%v acc=%d trust=%v} store{state=%v conf=%v rel=%v acc=%d trust=%v}",
					id, m.State, m.Confidence, m.Relevance, m.AccessCount, m.Trust,
					want.State, want.Confidence, want.Relevance, want.AccessCount, want.Trust))
			}
		}
	}
	return diffs
}

func engramStateDiff(got, want *Engram) string {
	if got.State != want.State || got.Confidence != want.Confidence || got.Relevance != want.Relevance ||
		got.AccessCount != want.AccessCount || got.Trust != want.Trust || !got.ValidUntil.Equal(want.ValidUntil) {
		return fmt.Sprintf("cached{state=%v conf=%v rel=%v acc=%d trust=%v vu=%v} store{state=%v conf=%v rel=%v acc=%d trust=%v vu=%v}",
			got.State, got.Confidence, got.Relevance, got.AccessCount, got.Trust, got.ValidUntil,
			want.State, want.Confidence, want.Relevance, want.AccessCount, want.Trust, want.ValidUntil)
	}
	return ""
}

func joinLines(ls []string) string {
	out := ""
	for _, l := range ls {
		out += "  " + l + "\n"
	}
	return out
}

// ---------------------------------------------------------------------------
// The size counter equals the number of live entries.
// ---------------------------------------------------------------------------

func liveEntries(c *L1Cache) int {
	n := 0
	c.data.Range(func(_, _ any) bool { n++; return true })
	return n
}

func TestL1Cache_CountEqualsLive_AfterOverwrites(t *testing.T) {
	c := NewL1Cache(50)
	var ws [8]byte
	id := NewULID()
	for i := 0; i < 60; i++ {
		c.Set(ws, id, &Engram{ID: id, AccessCount: uint32(i)})
	}
	if live := liveEntries(c); c.Len() != live || live != 1 {
		t.Fatalf("count drift: Len()=%d live=%d after 60 overwrites of one key (want 1/1)", c.Len(), live)
	}
	// An inflated counter made the no-evict fill path refuse every fill.
	other := NewULID()
	c.SetNoStamp(ws, other, &Engram{ID: other})
	if _, ok := c.GetNoStamp(ws, other); !ok {
		t.Fatalf("SetNoStamp refused a fill into a cache holding 1 of 50 entries (counter=%d)", c.Len())
	}
}

func TestL1Cache_CountEqualsLive_ConcurrentSameKeySets(t *testing.T) {
	c := NewL1Cache(200)
	var ws [8]byte
	keys6 := make([]ULID, 6)
	for i := range keys6 {
		keys6[i] = NewULID()
	}
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := keys6[(g+i)%len(keys6)]
				if i%2 == 0 {
					c.Set(ws, id, &Engram{ID: id})
				} else {
					c.SetNoStamp(ws, id, &Engram{ID: id})
				}
			}
		}(g)
	}
	wg.Wait()
	if live := liveEntries(c); c.Len() != live || live != 6 {
		t.Fatalf("count drift: Len()=%d live=%d after concurrent cold loads of 6 keys (want 6/6)", c.Len(), live)
	}
}

func TestL1Cache_CountEqualsLive_ChurnWithEvictionAndDelete(t *testing.T) {
	const max = 40
	c := NewL1Cache(max)
	var ws [8]byte
	pool := make([]ULID, 120)
	for i := range pool {
		pool[i] = NewULID()
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 2000; i++ {
				id := pool[r.Intn(len(pool))]
				switch r.Intn(4) {
				case 0, 1:
					c.Set(ws, id, &Engram{ID: id})
				case 2:
					c.SetNoStamp(ws, id, &Engram{ID: id})
				case 3:
					c.Delete(ws, id)
				}
			}
		}(int64(g))
	}
	wg.Wait()
	live := liveEntries(c)
	if c.Len() != live {
		t.Fatalf("count drift: Len()=%d live=%d after concurrent churn", c.Len(), live)
	}
	if live > max+16 { // concurrent fills may overshoot by at most one per goroutine
		t.Fatalf("cache holds %d entries, max %d", live, max)
	}
	c.DeleteByVault(ws)
	if c.Len() != 0 || liveEntries(c) != 0 {
		t.Fatalf("after DeleteByVault: Len()=%d live=%d", c.Len(), liveEntries(c))
	}
}

// committedFwdWeights scans the committed 0x03 rows for src, bypassing every cache.
func committedFwdWeights(t *testing.T, s *PebbleStore, ws [8]byte, src ULID) []float32 {
	t.Helper()
	pfx := keys.AssocFwdPrefixForID(ws, [16]byte(src))
	it, err := PrefixIterator(s.db, pfx)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	out := []float32{}
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) < 45 {
			continue
		}
		var wc [4]byte
		copy(wc[:], k[25:29])
		out = append(out, keys.WeightFromComplement(wc))
	}
	return out
}
