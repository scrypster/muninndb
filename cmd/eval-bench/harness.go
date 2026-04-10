package main

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"strings"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// evalEngine wraps an engine with a cleanup function and its embedder.
type evalEngine struct {
	Engine   *engine.Engine
	Embedder activation.Embedder
	Store    *storage.PebbleStore // exposed for interactive benchmark
	cleanup  func()
}

func (e *evalEngine) Close() {
	if e.cleanup != nil {
		e.cleanup()
	}
}

// WriteWithEmbedding computes the embedding and writes the engram with it,
// ensuring the HNSW index is populated synchronously.
func (e *evalEngine) WriteWithEmbedding(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	if e.Embedder != nil && len(req.Embedding) == 0 {
		text := req.Content
		if req.Concept != "" {
			text = req.Concept + ": " + text
		}
		vec, err := e.Embedder.Embed(ctx, []string{text})
		if err == nil && len(vec) > 0 {
			req.Embedding = vec
		}
	}
	return e.Engine.Write(ctx, req)
}

// EngineFactory creates a fresh isolated engine for each evaluation run.
type EngineFactory func(tmpDir string) (*evalEngine, error)

// NewEvalEngine creates an in-process MuninnDB engine with the given hippocampal config.
// If sharedEmbedder is non-nil, it's used instead of the default hash embedder.
func NewEvalEngine(baseDir string, hippoConfig *cognitive.HippocampalConfig, sharedEmbedder activation.Embedder) (*evalEngine, error) {
	tmpDir, err := os.MkdirTemp(baseDir, "eval-run-*")
	if err != nil {
		return nil, err
	}

	db, err := storage.OpenPebble(tmpDir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(tmpDir)
		return nil, err
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100_000})
	ftsIdx := fts.New(db)
	hnswReg := hnsw.NewRegistry(db)
	var embedder activation.Embedder = &hashEmbedder{}
	if sharedEmbedder != nil {
		embedder = sharedEmbedder
	}

	actEngine := activation.New(
		store,
		activation.NewFTSAdapter(ftsIdx),
		activation.NewHNSWAdapter(hnswReg),
		embedder,
	)
	trigSystem := trigger.New(
		store,
		trigger.NewFTSAdapter(ftsIdx),
		trigger.NewHNSWAdapter(hnswReg),
		embedder,
	)

	hebbianWorker := cognitive.NewHebbianWorker(&hebbianAdapter{store})
	contradictWorker := cognitive.NewContradictWorker(&contradictAdapter{store})
	confidenceWorker := cognitive.NewConfidenceWorker(&confidenceAdapter{store})

	workerCtx, workerCancel := context.WithCancel(context.Background())
	go contradictWorker.Worker.Run(workerCtx)
	go confidenceWorker.Worker.Run(workerCtx)

	var episodeWorker *cognitive.EpisodeWorker
	if hippoConfig != nil && hippoConfig.EnableEpisodes {
		episodeWorker = cognitive.NewEpisodeWorker(&episodeAdapter{store}, hippoConfig.EpisodeConfig)
		go episodeWorker.Worker.Run(workerCtx)
	}

	eng := engine.NewEngine(engine.EngineConfig{
		Store:             store,
		FTSIndex:          ftsIdx,
		ActivationEngine:  actEngine,
		TriggerSystem:     trigSystem,
		HebbianWorker:     hebbianWorker,
		ContradictWorker:  contradictWorker.Worker,
		ConfidenceWorker:  confidenceWorker.Worker,
		EpisodeWorker:     episodeWorker,
		Embedder:          embedder,
		HNSWRegistry:      hnswReg,
		HippocampalConfig: hippoConfig,
	})

	cleanup := func() {
		workerCancel()
		hebbianWorker.Stop()
		eng.Stop()
		store.Close()
		os.RemoveAll(tmpDir)
	}

	return &evalEngine{Engine: eng, Embedder: embedder, Store: store, cleanup: cleanup}, nil
}

// --- hashEmbedder: deterministic 384-dim word-hash vectors ---

type hashEmbedder struct{}

func (e *hashEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	const dims = 384
	vec := make([]float64, dims)
	for _, text := range texts {
		for _, word := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New64a()
			h.Write([]byte(word))
			rng := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec
			for i := range vec {
				vec[i] += rng.NormFloat64()
			}
		}
	}
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	out := make([]float32, dims)
	if norm > 0 {
		for i, v := range vec {
			out[i] = float32(v / norm)
		}
	}
	return out, nil
}

func (e *hashEmbedder) Tokenize(text string) []string {
	return strings.Fields(strings.ToLower(text))
}

// --- Cognitive worker adapters ---

type hebbianAdapter struct{ store *storage.PebbleStore }

func (a *hebbianAdapter) GetAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte) (float32, error) {
	return a.store.GetAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst))
}
func (a *hebbianAdapter) UpdateAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte, w float32) error {
	return a.store.UpdateAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst), w, 0)
}
func (a *hebbianAdapter) DecayAssocWeights(ctx context.Context, ws [8]byte, factor float64, min float32, archiveThreshold float64) (int, error) {
	return a.store.DecayAssocWeights(ctx, ws, factor, min, archiveThreshold)
}
func (a *hebbianAdapter) UpdateAssocWeightBatch(ctx context.Context, updates []cognitive.AssocWeightUpdate) error {
	su := make([]storage.AssocWeightUpdate, len(updates))
	for i, u := range updates {
		su[i] = storage.AssocWeightUpdate{
			WS: u.WS, Src: storage.ULID(u.Src), Dst: storage.ULID(u.Dst),
			Weight: u.Weight, CountDelta: u.CountDelta,
		}
	}
	return a.store.UpdateAssocWeightBatch(ctx, su)
}

type confidenceAdapter struct{ store *storage.PebbleStore }

func (a *confidenceAdapter) GetConfidence(ctx context.Context, ws [8]byte, id [16]byte) (float32, error) {
	return a.store.GetConfidence(ctx, ws, storage.ULID(id))
}
func (a *confidenceAdapter) UpdateConfidence(ctx context.Context, ws [8]byte, id [16]byte, c float32) error {
	return a.store.UpdateConfidence(ctx, ws, storage.ULID(id), c)
}

type contradictAdapter struct{ store *storage.PebbleStore }

func (a *contradictAdapter) FlagContradiction(ctx context.Context, ws [8]byte, engramA, engramB [16]byte) error {
	return a.store.FlagContradiction(ctx, ws, storage.ULID(engramA), storage.ULID(engramB))
}

type episodeAdapter struct{ store *storage.PebbleStore }

func (a *episodeAdapter) WriteAssociation(ctx context.Context, ws [8]byte, src, dst storage.ULID, assoc *storage.Association) error {
	return a.store.WriteAssociation(ctx, ws, src, dst, assoc)
}
