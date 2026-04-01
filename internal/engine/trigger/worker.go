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
		sub.mu.Lock()
		vec := sub.embedding
		subCtx := sub.Context
		sub.mu.Unlock()

		if len(vec) == 0 {
			if w.embedder != nil {
				computed, err := w.embedder.Embed(ctx, subCtx)
				if err != nil {
					continue
				}
				sub.mu.Lock()
				sub.embedding = computed
				vec = computed
				sub.mu.Unlock()
			}
		}

		if len(vec) == 0 {
			continue
		}

		fp := contextFingerprint(subCtx)
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
