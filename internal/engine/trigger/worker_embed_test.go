package trigger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// #512: when an embedding lands asynchronously after a write, the worker must
// re-evaluate PushOnWrite subscriptions with the now-available vector, pushing
// engrams that NEWLY match — but never double-pushing one already delivered at
// write time (dedup via pushedScores).

func newEmbedTestWorker(registry *SubscriptionRegistry, embedCh chan *EmbedEvent) *TriggerWorker {
	return &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		deliver:      &DeliveryRouter{registry: registry},
		writeEvents:  make(chan *EngramEvent),
		cogEvents:    make(chan CognitiveEvent),
		contraEvents: make(chan ContradictEvent),
		embedEvents:  embedCh,
	}
}

func runWorkerBriefly(w *TriggerWorker) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
}

func TestTriggerWorker_HandleEmbed_PushesNewlyMatching(t *testing.T) {
	registry := newRegistry()
	var pushCount atomic.Int32
	var gotTrigger atomic.Value // TriggerType; delivery runs on its own goroutine
	sub := &Subscription{
		ID:          "embed-sub",
		VaultID:     1,
		Context:     []string{"vector topic"},
		Threshold:   0.5, // requires a real vector match to fire
		PushOnWrite: true,
		expiresAt:   time.Now().Add(time.Hour),
		embedding:   []float32{1, 0, 0},
		Deliver: func(_ context.Context, push *ActivationPush) error {
			pushCount.Add(1)
			gotTrigger.Store(push.Trigger)
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(10),
	}
	registry.Add(sub)

	embedCh := make(chan *EmbedEvent, 10)
	engID := storage.NewULID()
	embedCh <- &EmbedEvent{
		VaultID:   1,
		Embedding: []float32{1, 0, 0}, // cosine 1.0 with the sub vector
		Engram: &storage.Engram{
			ID: engID, Concept: "c", Content: "x",
			Confidence: 0.9, Relevance: 0.8, Stability: 30,
			CreatedAt: time.Now(), LastAccess: time.Now(),
		},
	}

	runWorkerBriefly(newEmbedTestWorker(registry, embedCh))

	if pushCount.Load() != 1 {
		t.Fatalf("expected 1 push for newly-matching embed, got %d", pushCount.Load())
	}
	if v := gotTrigger.Load(); v == nil || v.(TriggerType) != TriggerNewWrite {
		t.Errorf("trigger = %v, want TriggerNewWrite", v)
	}
	if _, ok := sub.pushedScores[engID]; !ok {
		t.Error("expected engram recorded in pushedScores after embed push")
	}
}

// TestTriggerWorker_HandleEmbed_ComputesSubEmbeddingOnDemand locks in the gap a
// Docker end-to-end test surfaced: subscriptions are created WITHOUT an embedding
// (it is otherwise filled lazily by the periodic sweep), so handleEmbed must
// compute it on demand — else an embed event arriving before the first sweep has
// no vector to compare and never pushes.
func TestTriggerWorker_HandleEmbed_ComputesSubEmbeddingOnDemand(t *testing.T) {
	registry := newRegistry()
	var pushCount atomic.Int32
	sub := &Subscription{
		ID:          "embed-sub-3",
		VaultID:     1,
		Context:     []string{"some topic"},
		Threshold:   0.4,
		PushOnWrite: true,
		expiresAt:   time.Now().Add(time.Hour),
		// embedding deliberately empty — must be computed on demand.
		Deliver: func(_ context.Context, _ *ActivationPush) error {
			pushCount.Add(1)
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(10),
	}
	registry.Add(sub)

	embedCh := make(chan *EmbedEvent, 10)
	worker := newEmbedTestWorker(registry, embedCh)
	worker.embedder = &stubTrigEmbedder{} // returns [0.5,0.5,0.5,0.5] for any context

	embedCh <- &EmbedEvent{
		VaultID:   1,
		Embedding: []float32{0.5, 0.5, 0.5, 0.5}, // cosine 1.0 with the computed sub vector
		Engram: &storage.Engram{
			ID: storage.NewULID(), Concept: "c", Content: "x",
			Confidence: 0.9, Relevance: 0.8, Stability: 30,
			CreatedAt: time.Now(), LastAccess: time.Now(),
		},
	}

	runWorkerBriefly(worker)

	if pushCount.Load() != 1 {
		t.Fatalf("expected handleEmbed to compute the sub embedding and push, got %d", pushCount.Load())
	}
	sub.mu.Lock()
	cached := len(sub.embedding)
	sub.mu.Unlock()
	if cached == 0 {
		t.Error("expected sub.embedding to be computed and cached")
	}
}

// phraseEmbedder returns dim floats per input phrase (the real embedder's flat
// N*dim contract), so a multi-phrase context yields a longer-than-dim vector.
type phraseEmbedder struct{ dim int }

func (e *phraseEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	out := make([]float32, 0, len(texts)*e.dim)
	for range texts {
		for i := 0; i < e.dim; i++ {
			out = append(out, 0.5)
		}
	}
	return out, nil
}

// TestTriggerWorker_HandleEmbed_PoolsMultiPhraseContext verifies a multi-phrase
// subscription context is pooled to a single dim-sized vector before comparison
// — without pooling the sub vector is N*dim, cosine vs the dim-sized engram
// vector is 0, and the engram never matches (the #498 class of bug).
func TestTriggerWorker_HandleEmbed_PoolsMultiPhraseContext(t *testing.T) {
	registry := newRegistry()
	var pushCount atomic.Int32
	sub := &Subscription{
		ID:           "embed-sub-multi",
		VaultID:      1,
		Context:      []string{"phrase one", "phrase two", "phrase three"}, // 3 phrases
		Threshold:    0.4,
		PushOnWrite:  true,
		expiresAt:    time.Now().Add(time.Hour),
		Deliver:      func(_ context.Context, _ *ActivationPush) error { pushCount.Add(1); return nil },
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(10),
	}
	registry.Add(sub)

	embedCh := make(chan *EmbedEvent, 10)
	worker := newEmbedTestWorker(registry, embedCh)
	worker.embedder = &phraseEmbedder{dim: 4} // 3 phrases -> 12 floats, pooled to 4

	embedCh <- &EmbedEvent{
		VaultID:   1,
		Embedding: []float32{0.5, 0.5, 0.5, 0.5}, // dim 4 — matches the pooled sub vector
		Engram: &storage.Engram{
			ID: storage.NewULID(), Concept: "c", Content: "x",
			Confidence: 0.9, Relevance: 0.8, Stability: 30,
			CreatedAt: time.Now(), LastAccess: time.Now(),
		},
	}

	runWorkerBriefly(worker)

	if pushCount.Load() != 1 {
		t.Fatalf("expected a push after pooling the multi-phrase context, got %d", pushCount.Load())
	}
	sub.mu.Lock()
	dim := len(sub.embedding)
	sub.mu.Unlock()
	if dim != 4 {
		t.Errorf("expected sub.embedding pooled to dim 4, got %d", dim)
	}
}

func TestTriggerWorker_HandleEmbed_NoDoublePush(t *testing.T) {
	registry := newRegistry()
	var pushCount atomic.Int32
	engID := storage.NewULID()
	sub := &Subscription{
		ID:          "embed-sub-2",
		VaultID:     1,
		Context:     []string{"vector topic"},
		Threshold:   0.5,
		PushOnWrite: true,
		expiresAt:   time.Now().Add(time.Hour),
		embedding:   []float32{1, 0, 0},
		Deliver: func(_ context.Context, _ *ActivationPush) error {
			pushCount.Add(1)
			return nil
		},
		// Already delivered at write time.
		pushedScores: map[storage.ULID]float64{engID: 0.8},
		rateLimiter:  newTokenBucket(10),
	}
	registry.Add(sub)

	embedCh := make(chan *EmbedEvent, 10)
	embedCh <- &EmbedEvent{
		VaultID:   1,
		Embedding: []float32{1, 0, 0},
		Engram: &storage.Engram{
			ID: engID, Concept: "c", Content: "x",
			Confidence: 0.9, Relevance: 0.8, Stability: 30,
			CreatedAt: time.Now(), LastAccess: time.Now(),
		},
	}

	runWorkerBriefly(newEmbedTestWorker(registry, embedCh))

	if pushCount.Load() != 0 {
		t.Fatalf("expected no double-push when already pushed at write time, got %d", pushCount.Load())
	}
}
