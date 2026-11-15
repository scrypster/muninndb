package engine

// The retired set must follow what was COMMITTED. A former design noted a
// lifecycle change only when the writer's cached "old state" differed from
// the new one; a recall's snapshot read could refill the metadata cache with a
// pre-forget "active", so a later restore looked like a no-op and the restored,
// live memory stayed in the set — skipped at candidate collection with no
// error until a restart. Both tests below are the reviewer's machine checks,
// made permanent.

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func recallReturns(t testing.TB, env *rcEnv, q []float32, id string) bool {
	t.Helper()
	resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"qqzzxq"}, Embedding: q, MaxResults: 10, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Activations {
		if a.ID == id {
			return true
		}
	}
	return false
}

// (a) The deterministic ordering: forget while a recall's snapshot is open,
// the snapshot read refills the metadata cache with the old "active", then a
// restore. The restored memory must leave the set, move in the 0x0B index,
// and come back from recall — before and after a restart.
func TestRetiredSet_RestoreAfterStaleSnapshotFill(t *testing.T) {
	fs := vfs.NewMem()
	// A one-entry L1 cache: reading any other engram evicts the forgotten
	// one, so the snapshot read below misses L1 and fills the metadata cache
	// from the snapshot — the ordinary-eviction case, made deterministic.
	env := openRCEnvCache(t, fs, nil, 1)
	// Vector recall: a soft forget drops the memory's keyword postings and
	// restore does not re-add them (a separate, pre-existing gap), so only the
	// vector path can show whether the restored memory is a candidate again.
	r := rand.New(rand.NewSource(17))
	var bg []*mbp.WriteRequest
	for i := 0; i < 20; i++ {
		bg = append(bg, &mbp.WriteRequest{Vault: "default", Concept: "trail log " + strconv.Itoa(i), Content: "waterbar note " + strconv.Itoa(i), Embedding: rcRand(r)})
	}
	env.writeBatch(bg)
	vec := rcRand(r)
	resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "lantern mantle", Content: "the lantern mantle at the north shelter is cracked", Embedding: vec})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.ID
	u := env.mustULID(id)
	env.eng.waitWriteTimeIdle()
	if _, err := env.store.RetiredIDs(env.ctx, env.ws); err != nil { // built before the race
		t.Fatal(err)
	}

	snap := env.db.NewSnapshot()
	sctx := storage.ContextWithSnapshot(env.ctx, snap)
	if _, err := env.eng.Forget(env.ctx, &mbp.ForgetRequest{Vault: "default", ID: id}); err != nil {
		t.Fatal(err)
	}
	// Evict the forgotten engram from L1 (one-entry cache), then the
	// in-flight recall's read: through the pre-forget snapshot, filling the
	// metadata cache with the old ACTIVE state.
	if _, err := env.store.GetEngram(env.ctx, env.ws, env.mustULID(env.writeBatch([]*mbp.WriteRequest{{Vault: "default", Concept: "evictor", Content: "stile replaced", Embedding: rcRand(r)}})[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetMetadata(sctx, env.ws, []storage.ULID{u}); err != nil {
		t.Fatal(err)
	}
	snap.Close()
	if _, err := env.eng.Restore(env.ctx, "default", id); err != nil {
		t.Fatal(err)
	}

	check := func(e *rcEnv, when string) {
		view, err := e.store.RetiredIDs(e.ctx, e.ws)
		if err != nil {
			t.Fatal(err)
		}
		eng, err := e.store.GetEngram(e.ctx, e.ws, u)
		if err != nil {
			t.Fatal(err)
		}
		inSet := view.Contains([16]byte(u))
		returned := recallReturns(t, e, vec, id)
		deleted, _ := e.store.ListByState(e.ctx, e.ws, storage.StateSoftDeleted, 1000)
		inDeletedIndex := false
		for _, d := range deleted {
			if d == u {
				inDeletedIndex = true
			}
		}
		if inSet || !returned || inDeletedIndex {
			t.Errorf("%s: record state=%v inRetiredSet=%v returned=%v inSoftDeletedIndex=%v — a restored live memory must be visible",
				when, eng.State, inSet, returned, inDeletedIndex)
		}
	}
	check(env, "after restore")
	env.stop()
	env2 := openRCEnv(t, fs)
	defer env2.stop()
	check(env2, "after restart")
}

// (b) The randomized lifecycle differential: concurrent evolve, forget,
// restore, archive, un-archive, hard delete, not_true_since, consolidate and
// digest rewrites, with read-only recalls and the lazy first build racing
// them. Afterwards the maintained set must equal a from-scratch derivation
// from the records, and again after a restart. Seeds are logged; set
// MUNINN_RETIRED_SEED to replay one. Its value is under -race.
func TestRetiredSet_RandomizedLifecycleDifferential(t *testing.T) {
	seeds := []int64{1, 2, 3}
	if s := os.Getenv("MUNINN_RETIRED_SEED"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seeds = []int64{n}
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Logf("seed %d (replay: MUNINN_RETIRED_SEED=%d)", seed, seed)
			fs := vfs.NewMem()
			env := openRCEnv(t, fs)
			ctx := env.ctx
			r := rand.New(rand.NewSource(seed))
			topic := rcRand(r)
			var reqs []*mbp.WriteRequest
			for i := 0; i < 200; i++ {
				reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "frame " + strconv.Itoa(i),
					Content: "brood frame inspection " + strconv.Itoa(i), Embedding: rcNear(r, topic, 0.8)})
			}
			ids := env.writeBatch(reqs)
			env.eng.waitWriteTimeIdle()

			var poolMu sync.Mutex
			pool := append([]string(nil), ids...)
			pick := func(rr *rand.Rand) string {
				poolMu.Lock()
				defer poolMu.Unlock()
				return pool[rr.Intn(len(pool))]
			}
			add := func(id string) {
				poolMu.Lock()
				pool = append(pool, id)
				poolMu.Unlock()
			}
			q := rcNear(r, topic, 0.25)

			var wg sync.WaitGroup
			stop := make(chan struct{})
			const workers, ops = 4, 100
			for w := 0; w < workers; w++ {
				wseed := seed*1000 + int64(w)
				wg.Add(1)
				go func() {
					defer wg.Done()
					rr := rand.New(rand.NewSource(wseed))
					for i := 0; i < ops; i++ {
						id := pick(rr)
						// Errors are expected under random interleaving
						// (evolving a retired id, restoring a live one, ...);
						// only the end state is checked.
						switch rr.Intn(9) {
						case 0:
							if nid, err := env.eng.Evolve(ctx, "default", id, "evolved "+strconv.Itoa(i), "rev", rcNear(rr, topic, 0.25), "frame evolved"); err == nil {
								add(nid.String())
							}
						case 1:
							_, _ = env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: id})
						case 2:
							_, _ = env.eng.Restore(ctx, "default", id)
						case 3:
							_ = env.eng.UpdateLifecycleState(ctx, "default", id, "archived")
						case 4:
							_ = env.eng.UpdateLifecycleState(ctx, "default", id, "active")
						case 5:
							if rr.Intn(4) == 0 {
								_, _ = env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: id, Hard: true})
							}
						case 6:
							ts := time.Now().Add(-time.Minute)
							_, _ = env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: id, NotTrueSince: &ts})
						case 7:
							if res, err := env.eng.Consolidate(ctx, "default", []string{id, pick(rr)}, "merged frame notes "+strconv.Itoa(i)); err == nil && res != nil && res.MergedID != (storage.ULID{}) {
								add(res.MergedID.String())
							}
						case 8:
							if u, err := storage.ParseULID(id); err == nil {
								_ = env.store.UpdateDigest(ctx, u, "digest", []string{"point"}, "fact", "")
							}
						}
					}
				}()
			}
			var rwg sync.WaitGroup
			for g := 0; g < 2; g++ {
				rwg.Add(1)
				go func() {
					defer rwg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						_, _ = env.eng.Activate(ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"brood frame"}, Embedding: q, MaxResults: 10, ReadOnly: true})
					}
				}()
			}
			wg.Wait()
			close(stop)
			rwg.Wait()
			env.eng.waitWriteTimeIdle()
			assertRetiredMatches(t, env, fmt.Sprintf("seed %d, after concurrent lifecycle ops", seed))
			env.stop()

			env2 := openRCEnv(t, fs)
			defer env2.stop()
			assertRetiredMatches(t, env2, fmt.Sprintf("seed %d, after restart", seed))
		})
	}
}
