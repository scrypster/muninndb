package trigger

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// DeliveryRouter sends pushes to client connections.
type DeliveryRouter struct {
	registry *SubscriptionRegistry
}

// Send delivers a push to a subscription's connection.
func (d *DeliveryRouter) Send(sub *Subscription, push *ActivationPush) {
	sub.mu.Lock()
	push.PushNumber = sub.pushCount + 1
	deliverFn := sub.Deliver
	sub.mu.Unlock()

	if deliverFn == nil {
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("trigger: delivery panic recovered", "panic", r)
				d.registry.Remove(sub.ID)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), deliverTimeout)
		defer cancel()
		err := deliverFn(ctx, push)
		if err != nil {
			d.registry.Remove(sub.ID)
		}
	}()
}

// TriggerWorker is the shared event loop for all subscriptions.
type TriggerWorker struct {
	registry   *SubscriptionRegistry
	embedCache *EmbedCache
	store      TriggerStore
	fts        FTSIndex
	hnsw       HNSWIndex
	embedder   Embedder
	deliver    *DeliveryRouter

	writeEvents  <-chan *EngramEvent
	cogEvents    <-chan CognitiveEvent
	contraEvents <-chan ContradictEvent
	embedEvents  <-chan *EmbedEvent
}

// Run starts the trigger event loop.
func (w *TriggerWorker) Run(ctx context.Context) error {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-w.contraEvents:
			if !ok {
				return nil
			}
			w.handleContradiction(ctx, event)

		case event, ok := <-w.writeEvents:
			if !ok {
				return nil
			}
			w.handleWrite(ctx, event)

		case event, ok := <-w.cogEvents:
			if !ok {
				return nil
			}
			w.handleCognitive(ctx, event)

		case event, ok := <-w.embedEvents:
			if !ok {
				return nil
			}
			w.handleEmbed(ctx, event)

		case <-sweep.C:
			w.handleSweep(ctx)
			w.registry.PruneExpired()
		}
	}
}

func (w *TriggerWorker) vaultWS(vaultID uint32) [8]byte {
	var ws [8]byte
	ws[0] = byte(vaultID >> 24)
	ws[1] = byte(vaultID >> 16)
	ws[2] = byte(vaultID >> 8)
	ws[3] = byte(vaultID)
	return ws
}

func (w *TriggerWorker) handleWrite(ctx context.Context, event *EngramEvent) {
	if !event.IsNew {
		return
	}
	subs := w.registry.ForVault(event.VaultID)
	if len(subs) == 0 {
		return
	}

	engramVec := event.Engram.Embedding

	// On a freshly-written engram, event.Engram.Embedding is usually empty
	// because embeddings are computed asynchronously by the retroactive processor
	// (internal/plugin/retroactive.go) ~tens of ms later. So vectorScore is 0
	// here, and context/threshold-filtered subscriptions can only fire via the
	// non-vector components below (decay, recency, confidence) at write time.
	// Once the embedding lands, the processor calls back into the engine, which
	// invokes handleEmbed (below) to re-evaluate these subscriptions with the
	// now-available vector — pushing a newly-matching engram exactly once, since
	// handleEmbed skips anything already delivered here (pushedScores dedup) (#512).

	for _, sub := range subs {
		if !sub.PushOnWrite {
			continue
		}
		sub.mu.Lock()
		subVec := sub.embedding
		sub.mu.Unlock()

		// When either the engram or the subscription has no embedding, fall back
		// to vectorScore=0. TriggerScore will still fire for Threshold=0 subs
		// using decay, recency, and confidence components. This ensures
		// PushOnWrite delivers even for engrams that have not yet been embedded.
		var vectorScore float64
		if len(subVec) > 0 && len(engramVec) > 0 {
			vectorScore = cosineSimilarity(subVec, engramVec)
		}

		meta := engramToMeta(event.Engram)
		score, above := TriggerScore(sub, meta, vectorScore, 0)
		if !above {
			continue
		}

		// T4: rate-limit write pushes before delivery.
		if !sub.rateLimiter.TryConsume() {
			continue
		}

		w.deliver.Send(sub, &ActivationPush{
			SubscriptionID: sub.ID,
			Engram:         event.Engram,
			Score:          score,
			Trigger:        TriggerNewWrite,
			At:             time.Now(),
		})

		sub.mu.Lock()
		sub.pushedScores[event.Engram.ID] = score
		sub.pushCount++
		sub.mu.Unlock()
	}
}

// handleEmbed re-evaluates PushOnWrite subscriptions after an engram's embedding
// finishes computing asynchronously (#512). handleWrite runs at write time with
// vectorScore=0 (the embedding is not ready yet), so a context/threshold-filtered
// subscription that should match semantically cannot fire then. Once the vector
// lands, this pushes engrams that NOW match — but only those NOT already
// delivered at write time (dedup via pushedScores), so a write-time match is
// never double-pushed. The result is a single, correctly vector-scored push.
func (w *TriggerWorker) handleEmbed(ctx context.Context, event *EmbedEvent) {
	if event == nil || event.Engram == nil || len(event.Embedding) == 0 {
		return
	}
	subs := w.registry.ForVault(event.VaultID)
	if len(subs) == 0 {
		return
	}

	meta := engramToMeta(event.Engram)
	for _, sub := range subs {
		if !sub.PushOnWrite {
			continue
		}
		sub.mu.Lock()
		_, already := sub.pushedScores[event.Engram.ID]
		sub.mu.Unlock()
		if already {
			continue // already delivered at write time — no double-push
		}

		// Compute the subscription's context embedding on demand (it is created
		// without one and otherwise only filled lazily by the periodic sweep).
		// Without this, an embed event arriving before the first sweep would have
		// no vector to compare and the whole point of the re-evaluation is lost.
		subVec := w.subEmbedding(ctx, sub)
		if len(subVec) == 0 {
			continue
		}

		vectorScore := cosineSimilarity(subVec, event.Embedding)
		score, above := TriggerScore(sub, meta, vectorScore, 0)
		if !above {
			continue
		}
		if !sub.rateLimiter.TryConsume() {
			continue
		}

		w.deliver.Send(sub, &ActivationPush{
			SubscriptionID: sub.ID,
			Engram:         event.Engram,
			Score:          score,
			Trigger:        TriggerNewWrite,
			At:             time.Now(),
		})

		sub.mu.Lock()
		sub.pushedScores[event.Engram.ID] = score
		sub.pushCount++
		sub.mu.Unlock()
	}
}

func (w *TriggerWorker) handleCognitive(ctx context.Context, event CognitiveEvent) {
	subs := w.registry.ForVault(event.VaultID)
	if len(subs) == 0 {
		return
	}

	ws := w.vaultWS(event.VaultID)
	metas, err := w.store.GetMetadata(ctx, ws, []storage.ULID{event.EngramID})
	if err != nil || len(metas) == 0 {
		return
	}
	meta := metas[0]
	if meta == nil {
		return
	}

	for _, sub := range subs {
		sub.mu.Lock()
		subVec := sub.embedding
		lastScore := sub.pushedScores[event.EngramID]
		sub.mu.Unlock()

		score, above := TriggerScore(sub, meta, cosineSimilarity(subVec, nil), 0)
		if !above {
			if lastScore > 0 {
				sub.mu.Lock()
				delete(sub.pushedScores, event.EngramID)
				sub.mu.Unlock()
			}
			continue
		}

		delta := math.Abs(score - lastScore)
		if lastScore > 0 && delta < sub.DeltaThreshold {
			continue
		}

		if !sub.rateLimiter.TryConsume() {
			continue
		}

		w.deliver.Send(sub, &ActivationPush{
			SubscriptionID: sub.ID,
			Score:          score,
			Trigger:        TriggerThresholdCrossed,
			At:             time.Now(),
		})

		sub.mu.Lock()
		sub.pushedScores[event.EngramID] = score
		sub.pushCount++
		sub.mu.Unlock()
	}
}

func (w *TriggerWorker) handleContradiction(ctx context.Context, event ContradictEvent) {
	subs := w.registry.ForVault(event.VaultID)
	if len(subs) == 0 {
		return
	}

	ws := w.vaultWS(event.VaultID)
	engrams, err := w.store.GetEngrams(ctx, ws, []storage.ULID{event.EngramA, event.EngramB})
	if err != nil || len(engrams) == 0 {
		return
	}
	byID := make(map[storage.ULID]*storage.Engram, 2)
	for _, e := range engrams {
		byID[e.ID] = e
	}

	for _, sub := range subs {
		sub.mu.Lock()
		_, aWasPushed := sub.pushedScores[event.EngramA]
		_, bWasPushed := sub.pushedScores[event.EngramB]
		sub.mu.Unlock()

		if !aWasPushed && !bWasPushed {
			continue
		}

		// T5: contradiction events use burst-aware rate limiting (overdraft 3).
		if !sub.rateLimiter.TryConsumeOrBurst(3) {
			continue
		}

		for _, id := range []storage.ULID{event.EngramA, event.EngramB} {
			eng := byID[id]
			if eng == nil || eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
				continue
			}
			w.deliver.Send(sub, &ActivationPush{
				SubscriptionID: sub.ID,
				Engram:         eng,
				Score:          1.0,
				Trigger:        TriggerContradiction,
				Why:            fmt.Sprintf("contradiction detected (severity %.0f%%, type: %s)", event.Severity*100, event.Type),
				At:             time.Now(),
			})
		}
	}
}

// subEmbedding returns the subscription's context embedding, computing and
// caching it on first use. Subscriptions are created without an embedding; it is
// filled here (by the sweep or by handleEmbed) the first time a vector is needed.
// Returns nil if there is no embedder or the context cannot be embedded.
func (w *TriggerWorker) subEmbedding(ctx context.Context, sub *Subscription) []float32 {
	sub.mu.Lock()
	vec := sub.embedding
	subCtx := sub.Context
	sub.mu.Unlock()

	if len(vec) > 0 {
		return vec
	}
	if w.embedder == nil || len(subCtx) == 0 {
		return nil
	}
	computed, err := w.embedder.Embed(ctx, subCtx)
	if err != nil || len(computed) == 0 {
		return nil
	}
	// Embed returns the flat concatenation of per-phrase vectors. Pool a
	// multi-phrase context into a single dim-sized vector so cosine against a
	// per-engram vector is meaningful (mirrors the activation-path fix in #498).
	if n := len(subCtx); n > 1 && len(computed)%n == 0 {
		computed = meanPoolVec(computed, n)
	}
	sub.mu.Lock()
	sub.embedding = computed
	sub.mu.Unlock()
	return computed
}

// meanPoolVec averages n equal-length sub-vectors concatenated in flat and
// L2-normalizes the result, collapsing a multi-phrase embedding into one vector.
func meanPoolVec(flat []float32, n int) []float32 {
	dim := len(flat) / n
	pooled := make([]float32, dim)
	for p := 0; p < n; p++ {
		base := p * dim
		for i := 0; i < dim; i++ {
			pooled[i] += flat[base+i]
		}
	}
	var norm float64
	for i := range pooled {
		pooled[i] /= float32(n)
		norm += float64(pooled[i]) * float64(pooled[i])
	}
	if norm > 0 {
		inv := float32(1.0 / math.Sqrt(norm))
		for i := range pooled {
			pooled[i] *= inv
		}
	}
	return pooled
}

func (w *TriggerWorker) handleSweep(ctx context.Context) {
	vaults := w.registry.ActiveVaults()
	for _, vaultID := range vaults {
		subs := w.registry.ForVault(vaultID)
		if len(subs) == 0 {
			continue
		}
		ws := w.vaultWS(vaultID)
		w.sweepVault(ctx, vaultID, ws, subs)
	}
}

func (w *TriggerWorker) sweepVault(ctx context.Context, vaultID uint32, ws [8]byte, subs []*Subscription) {
	if w.hnsw == nil {
		return
	}

	type vecGroup struct {
		vec  []float32
		subs []*Subscription
	}
	groups := make(map[[32]byte]*vecGroup)

	for _, sub := range subs {
		vec := w.subEmbedding(ctx, sub)
		if len(vec) == 0 {
			continue
		}

		fp := contextFingerprint(sub.Context)
		if g, ok := groups[fp]; ok {
			g.subs = append(g.subs, sub)
		} else {
			groups[fp] = &vecGroup{vec: vec, subs: []*Subscription{sub}}
		}
	}

	for _, group := range groups {
		candidates, err := w.hnsw.Search(ctx, ws, group.vec, sweepTopK)
		if err != nil {
			slog.Warn("trigger: hnsw search failed in sweep", "vault", vaultID, "err", err)
			continue
		}
		if len(candidates) == 0 {
			continue
		}

		ids := make([]storage.ULID, len(candidates))
		for i, c := range candidates {
			ids[i] = c.ID
		}
		metas, err := w.store.GetMetadata(ctx, ws, ids)
		if err != nil {
			slog.Warn("trigger: GetMetadata failed in sweep", "vault", vaultID, "err", err)
			continue
		}
		metaByID := make(map[storage.ULID]*storage.EngramMeta, len(metas))
		for _, m := range metas {
			if m == nil {
				continue
			}
			metaByID[m.ID] = m
		}

		vecScores := make(map[storage.ULID]float64, len(candidates))
		for _, c := range candidates {
			vecScores[c.ID] = c.Score
		}

		// Batch-load all candidate engrams in a single call before entering the
		// subscription loop — avoids N×M individual store calls (N subs × M candidates).
		allEngrams, err := w.store.GetEngrams(ctx, ws, ids)
		if err != nil {
			slog.Warn("trigger: GetEngrams failed in sweep", "vault", vaultID, "err", err)
			allEngrams = nil
		}
		engramByID := make(map[storage.ULID]*storage.Engram, len(allEngrams))
		for _, eng := range allEngrams {
			if eng != nil {
				engramByID[eng.ID] = eng
			}
		}

		for _, sub := range group.subs {
			for _, c := range candidates {
				meta := metaByID[c.ID]
				if meta == nil || meta.State == storage.StateSoftDeleted || meta.State == storage.StateArchived {
					continue
				}

				score, above := TriggerScore(sub, meta, vecScores[c.ID], 0)
				if !above {
					continue
				}

				sub.mu.Lock()
				lastScore := sub.pushedScores[c.ID]
				delta := math.Abs(score - lastScore)
				rateLimitOK := sub.rateLimiter.TryConsume()
				sub.mu.Unlock()

				if lastScore > 0 && delta < sub.DeltaThreshold {
					continue
				}
				if !rateLimitOK {
					continue
				}

				eng := engramByID[c.ID]
				if eng == nil || eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
					continue
				}

				w.deliver.Send(sub, &ActivationPush{
					SubscriptionID: sub.ID,
					Engram:         eng,
					Score:          score,
					Trigger:        TriggerThresholdCrossed,
					At:             time.Now(),
				})

				sub.mu.Lock()
				sub.pushedScores[c.ID] = score
				sub.pushCount++
				sub.mu.Unlock()
			}
		}
	}
}
