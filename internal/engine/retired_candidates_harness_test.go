package engine

// Shared harness for the retired-candidates tests: a real Pebble store, real
// FTS and HNSW wired through the PRODUCTION activation adapters (so the
// retired-aware SearchLive paths are the ones exercised), and caller-supplied
// synthetic vectors. Every fixture here is invented; the vectors are seeded
// random directions with no relationship to any real corpus.

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const rcDim = 64

type rcEnv struct {
	t     testing.TB
	eng   *Engine
	store *storage.PebbleStore
	db    *pebble.DB
	reg   *hnswpkg.Registry
	ctx   context.Context
	ws    [8]byte
	stop  func()
}

// openRCEnv opens (or re-opens) an engine over the Pebble database in fs.
// Re-opening the same fs in a later call is a process restart as far as every
// in-memory structure is concerned: a new store, new caches, a new retired
// set, an HNSW graph loaded from Pebble. An in-memory filesystem keeps the
// 2,000-memory fixtures affordable under CI's -race job (the FTS indexer
// fsyncs once per engram, which on a real disk dominated fixture build time).
func openRCEnv(t testing.TB, fs vfs.FS) *rcEnv {
	t.Helper()
	return openRCEnvWith(t, fs, nil)
}

// openRCEnvWith is openRCEnv with an optional wrapper around the HNSW index
// the activation engine sees (a counting double).
func openRCEnvWith(t testing.TB, fs vfs.FS, wrapHNSW func(activation.HNSWIndex) activation.HNSWIndex) *rcEnv {
	t.Helper()
	return openRCEnvCache(t, fs, wrapHNSW, 20000)
}

// openRCEnvCache is openRCEnvWith with an explicit L1 cache size (a tiny cache
// makes evictions deterministic).
func openRCEnvCache(t testing.TB, fs vfs.FS, wrapHNSW func(activation.HNSWIndex) activation.HNSWIndex, cacheSize int) *rcEnv {
	t.Helper()
	db, err := pebble.Open("rc-db", &pebble.Options{FS: fs, MemTableSize: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: cacheSize, NoSyncEngrams: true})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	reg := hnswpkg.NewRegistry(db)
	var hnswIdx activation.HNSWIndex = activation.NewHNSWAdapter(reg)
	if wrapHNSW != nil {
		hnswIdx = wrapHNSW(hnswIdx)
	}
	act := activation.New(store, activation.NewFTSAdapter(ftsIdx), hnswIdx, embedder)
	trig := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), trigger.NewHNSWAdapter(reg), embedder)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: act, TriggerSystem: trig,
		Embedder: embedder, HNSWRegistry: reg,
		ContradictWorker: (*cognitive.Worker[cognitive.ContradictItem])(nil),
	})
	env := &rcEnv{t: t, eng: eng, store: store, db: db, reg: reg, ctx: context.Background(), ws: store.ResolveVaultPrefix("default")}
	stopped := false
	env.stop = func() {
		if stopped {
			return
		}
		stopped = true
		eng.Stop()
		reg.WaitPersisted()
		store.Close()
	}
	return env
}

func rcUnit(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	out := make([]float32, len(v))
	for i := range v {
		out[i] = v[i] / n
	}
	return out
}

func rcRand(r *rand.Rand) []float32 {
	v := make([]float32, rcDim)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return rcUnit(v)
}

// rcNear returns normalize(base + eps·noise): eps 0.25 is a near-duplicate of
// base, eps 0.8 is clearly related but distinct.
func rcNear(r *rand.Rand, base []float32, eps float64) []float32 {
	n := rcRand(r)
	v := make([]float32, rcDim)
	for i := range v {
		v[i] = base[i] + float32(eps)*n[i]
	}
	return rcUnit(v)
}

// writeBatch writes reqs in batches of 50 and returns their ids in order.
func (env *rcEnv) writeBatch(reqs []*mbp.WriteRequest) []string {
	env.t.Helper()
	ids := make([]string, 0, len(reqs))
	for off := 0; off < len(reqs); off += 50 {
		end := off + 50
		if end > len(reqs) {
			end = len(reqs)
		}
		resps, errs := env.eng.WriteBatch(env.ctx, reqs[off:end])
		for i, e := range errs {
			if e != nil {
				env.t.Fatalf("batch write: %v", e)
			}
			ids = append(ids, resps[i].ID)
		}
	}
	return ids
}

// evolveChain evolves first through n-1 successors (a chain of n versions)
// and returns every version id, oldest first. vec(i) supplies version i's
// vector (nil = no embedding); content(i) its text.
func (env *rcEnv) evolveChain(first string, n int, concept string, content func(i int) string, vec func(i int) []float32) []string {
	env.t.Helper()
	ids := []string{first}
	cur := first
	for i := 1; i < n; i++ {
		var v []float32
		if vec != nil {
			v = vec(i)
		}
		nid, err := env.eng.Evolve(env.ctx, "default", cur, content(i), "revision "+strconv.Itoa(i), v, concept)
		if err != nil {
			env.t.Fatalf("evolve %d: %v", i, err)
		}
		cur = nid.String()
		ids = append(ids, cur)
	}
	return ids
}

func (env *rcEnv) mustULID(id string) storage.ULID {
	env.t.Helper()
	u, err := storage.ParseULID(id)
	if err != nil {
		env.t.Fatal(err)
	}
	return u
}

// slotsFixture is the starvation fixture: bg unrelated live memories, rel
// relevant live memories around a topic, and (if chainLen > 0) one evolve chain
// of chainLen near-duplicate versions sitting right on the topic — the shape a
// repeatedly evolved bookmark-style memory grows.
type slotsFixture struct {
	rel   map[string]bool
	chain []string // oldest first; last is the head
	query []float32
}

func buildSlotsFixture(env *rcEnv, bg, rel, chainLen int) slotsFixture {
	env.t.Helper()
	r := rand.New(rand.NewSource(42))
	topic := rcRand(r)
	var reqs []*mbp.WriteRequest
	for i := 0; i < bg; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "trail log " + strconv.Itoa(i),
			Content: "switchback inspection note number " + strconv.Itoa(i), Embedding: rcRand(r)})
	}
	for i := 0; i < rel; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "hive survey " + strconv.Itoa(i),
			Content: "brood frame observation number " + strconv.Itoa(i), Embedding: rcNear(r, topic, 0.8)})
	}
	ids := env.writeBatch(reqs)
	fx := slotsFixture{rel: map[string]bool{}}
	for _, id := range ids[bg:] {
		fx.rel[id] = true
	}
	if chainLen > 0 {
		resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "apiary bookmark",
			Content: "apiary bookmark version 0", Embedding: rcNear(r, topic, 0.25)})
		if err != nil {
			env.t.Fatal(err)
		}
		fx.chain = env.evolveChain(resp.ID, chainLen, "apiary bookmark",
			func(i int) string { return "apiary bookmark version " + strconv.Itoa(i) },
			func(int) []float32 { return rcNear(r, topic, 0.25) })
	}
	env.eng.waitWriteTimeIdle()
	fx.query = rcNear(rand.New(rand.NewSource(7)), topic, 0.25)
	return fx
}

// recallVec runs a vector-only recall (the context is a token no fixture
// contains, so FTS contributes nothing).
func (env *rcEnv) recallVec(q []float32, maxResults int) *mbp.ActivateResponse {
	env.t.Helper()
	resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"qqzzxq"},
		Embedding: q, MaxResults: maxResults})
	if err != nil {
		env.t.Fatal(err)
	}
	return resp
}
