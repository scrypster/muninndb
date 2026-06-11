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
