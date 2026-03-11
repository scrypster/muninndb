package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

// pollInterval is how often the processor checks for newly written, unembedded engrams.
const pollInterval = 3 * time.Second

// maxBatchSize caps the number of engrams processed in a single pass.
// This bounds iterator lifetime during bulk imports and keeps the hot path
// responsive; any remaining unprocessed engrams are picked up on the next tick.
const maxBatchSize = 1000

// maxBackoff is the upper bound for exponential back-off when the store
// returns persistent errors on CountWithoutFlag / ScanWithoutFlag.
const maxBackoff = 5 * time.Minute

// maxIdleInterval is the upper bound for poll back-off when CountWithoutFlag
// reports pending items but the scan produces no actual work (phantom count).
// The processor resets to pollInterval as soon as real work is done or Notify()
// is called.
const maxIdleInterval = 3 * time.Minute

// RetroactiveProcessor processes engrams asynchronously with a registered plugin.
// It scans for engrams missing a digest flag and calls the plugin to process them.
// The processor runs continuously: it does an initial pass at startup, then polls
// every pollInterval seconds. Callers can call Notify() to wake it immediately
// (e.g. after a new engram is written) without waiting for the next poll.
type RetroactiveProcessor struct {
	store    PluginStore
	plugin   Plugin
	flagBit  uint8 // DigestEmbed or DigestEnrich
	stats    RetroactiveStats
	statsMu  sync.RWMutex
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	notifyCh chan struct{} // buffered(1); non-blocking send from Notify()
}

// NewRetroactiveProcessor creates a new processor for a plugin.
func NewRetroactiveProcessor(store PluginStore, p Plugin, flagBit uint8) *RetroactiveProcessor {
	return &RetroactiveProcessor{
		store:    store,
		plugin:   p,
		flagBit:  flagBit,
		notifyCh: make(chan struct{}, 1),
	}
}

// Notify wakes the processor to run a scan immediately, without waiting for
// the next poll tick. Safe to call concurrently; drops the signal if the
// channel is already full (i.e. a scan is already pending).
func (rp *RetroactiveProcessor) Notify() {
	select {
	case rp.notifyCh <- struct{}{}:
	default:
	}
}

// Start launches the background processing goroutine.
func (rp *RetroactiveProcessor) Start(ctx context.Context) {
	// Create a cancellable context
	ctx, rp.cancelFn = context.WithCancel(ctx)

	rp.wg.Add(1)
	go rp.run(ctx)
}

// Stop gracefully shuts down the processor.
func (rp *RetroactiveProcessor) Stop() {
	if rp.cancelFn != nil {
		rp.cancelFn()
	}
	rp.wg.Wait()
	rp.statsMu.Lock()
	rp.stats.Status = "stopped"
	rp.statsMu.Unlock()
}

// Stats returns a copy of the current processor statistics.
func (rp *RetroactiveProcessor) Stats() RetroactiveStats {
	rp.statsMu.RLock()
	defer rp.statsMu.RUnlock()
	return rp.stats
}

// Mode returns "embed" when this processor handles embedding (DigestEmbed flag)
// or "enrich" when it handles enrichment (DigestEnrich flag).
func (rp *RetroactiveProcessor) Mode() string {
	if rp.flagBit == DigestEmbed {
		return "embed"
	}
	return "enrich"
}

// skipFlags returns the digest flags that should be excluded from scanning.
// Embed processors skip DigestEmbedFailed engrams to avoid infinite retry loops.
func (rp *RetroactiveProcessor) skipFlags() uint8 {
	if rp.flagBit == DigestEmbed {
		return DigestEmbedFailed
	}
	return 0
}

func (rp *RetroactiveProcessor) run(ctx context.Context) {
	defer rp.wg.Done()

	rp.statsMu.Lock()
	rp.stats.PluginName = rp.plugin.Name()
	rp.stats.Status = "running"
	rp.stats.StartedAt = time.Now()
	rp.statsMu.Unlock()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var consecutiveErrors int
	var consecutiveIdle int

	// Initial pass immediately on start.
	if rp.processBatch(ctx) {
		consecutiveErrors = 0
	} else {
		consecutiveErrors++
	}

	for {
		select {
		case <-ctx.Done():
			rp.statsMu.Lock()
			rp.stats.Status = "stopped"
			rp.statsMu.Unlock()
			return
		case <-rp.notifyCh:
			// Explicit wakeup (new engram written) — always run immediately.
			if rp.processBatchIdle(ctx, &consecutiveIdle) {
				consecutiveErrors = 0
			} else {
				consecutiveErrors++
				rp.backoff(ctx, consecutiveErrors)
			}
			// Reset to fast polling on explicit notify.
			if consecutiveIdle == 0 {
				ticker.Reset(pollInterval)
			}
		case <-ticker.C:
			prevProcessed := rp.Stats().Processed
			if rp.processBatch(ctx) {
				consecutiveErrors = 0
				if rp.Stats().Processed == prevProcessed {
					// Ran successfully but did no actual work — back off.
					consecutiveIdle++
					newInterval := pollInterval * time.Duration(1<<min(consecutiveIdle, 6))
					if newInterval > maxIdleInterval {
						newInterval = maxIdleInterval
					}
					ticker.Reset(newInterval)
				} else {
					// Real work done — reset to fast polling.
					if consecutiveIdle > 0 {
						consecutiveIdle = 0
						ticker.Reset(pollInterval)
					}
				}
			} else {
				consecutiveErrors++
				rp.backoff(ctx, consecutiveErrors)
			}
		}
	}
}

// processBatchIdle wraps processBatch and updates the idle counter.
// Used by the Notify path where we always want to run but still track idle state.
func (rp *RetroactiveProcessor) processBatchIdle(ctx context.Context, consecutiveIdle *int) bool {
	prev := rp.Stats().Processed
	ok := rp.processBatch(ctx)
	if ok && rp.Stats().Processed > prev {
		*consecutiveIdle = 0
	} else if ok {
		(*consecutiveIdle)++
	}
	return ok
}

// backoff sleeps for an exponentially increasing duration (capped at maxBackoff)
// when the store returns persistent errors, preventing log floods.
// Returns early if ctx is cancelled.
func (rp *RetroactiveProcessor) backoff(ctx context.Context, consecutiveErrors int) {
	if consecutiveErrors <= 1 {
		return // first error: no extra wait, let the normal ticker handle it
	}
	// 2^(n-1) * pollInterval, capped at maxBackoff
	wait := pollInterval * (1 << (consecutiveErrors - 1))
	if wait > maxBackoff {
		wait = maxBackoff
	}
	slog.Warn("retroactive processor: backing off due to store errors",
		"plugin", rp.plugin.Name(),
		"consecutive_errors", consecutiveErrors,
		"backoff", wait)
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// processBatch scans for unprocessed engrams and processes up to maxBatchSize
// in one pass. Returns true on success (including zero-work passes), false if
// a store-level error prevents processing (used by run() for backoff decisions).
//
// For EmbedPlugin: accumulates up to MaxBatchSize() engrams and issues one
// inference call per micro-batch, then scatters vectors back individually.
// For EnrichPlugin: processes one engram at a time (LLM call per engram).
func (rp *RetroactiveProcessor) processBatch(ctx context.Context) bool {
	skipFlags := rp.skipFlags()
	total, err := rp.store.CountWithoutFlag(ctx, rp.flagBit, skipFlags)
	if err != nil {
		slog.Error("retroactive processor: count failed", "plugin", rp.plugin.Name(), "error", err)
		return false
	}

	if total == 0 {
		return true
	}

	// Snapshot cumulative processed count so we can detect per-pass work.
	rp.statsMu.RLock()
	passStart := rp.stats.Processed
	rp.statsMu.RUnlock()

	slog.Debug("retroactive processor: starting", "plugin", rp.plugin.Name(), "total", total)

	rp.statsMu.Lock()
	rp.stats.Total += total
	rp.statsMu.Unlock()

	iter := rp.store.ScanWithoutFlag(ctx, rp.flagBit, skipFlags)
	if iter == nil {
		slog.Error("retroactive processor: failed to create iterator", "plugin", rp.plugin.Name())
		return false
	}
	defer iter.Close()

	startTime := time.Now()
	batchCount := 0

	// For embed plugins, accumulate a micro-batch and embed in one ORT call.
	// The batch size is determined by the plugin's MaxBatchSize() so the provider
	// runs at its optimal throughput rather than a hardcoded constant.
	embedPlugin, isEmbedPlugin := rp.plugin.(EmbedPlugin)
	microBatchSize := 32 // fallback for the non-embed path (never used there)
	if isEmbedPlugin {
		microBatchSize = embedPlugin.MaxBatchSize()
	}
	microEngrams := make([]*Engram, 0, microBatchSize)
	microTexts := make([]string, 0, microBatchSize)

	flushMicroBatch := func() {
		if !isEmbedPlugin || len(microEngrams) == 0 {
			return
		}
		vecs, embedErr := embedPlugin.Embed(ctx, microTexts)
		if embedErr != nil {
			ids := make([]string, len(microEngrams))
			for i, e := range microEngrams {
				ids[i] = e.ID.String()
			}
			slog.Warn("retroactive processor: embed batch failed",
				"plugin", rp.plugin.Name(),
				"batch_size", len(microEngrams),
				"engram_ids", ids,
				"error", embedErr)
			// Mark each engram with DigestEmbedFailed so the processor does not
			// retry them indefinitely. If the underlying provider recovers, an
			// operator can clear the flag manually or via the admin API.
			for _, e := range microEngrams {
				if flagErr := rp.store.SetDigestFlag(ctx, e.ID, DigestEmbedFailed); flagErr != nil {
					slog.Warn("retroactive processor: failed to set DigestEmbedFailed",
						"plugin", rp.plugin.Name(), "engram_id", e.ID.String(), "error", flagErr)
				}
			}
			rp.statsMu.Lock()
			rp.stats.Errors += int64(len(microEngrams))
			rp.statsMu.Unlock()
			microEngrams = microEngrams[:0]
			microTexts = microTexts[:0]
			return
		}
		dim := len(vecs) / len(microEngrams)
		for i, eng := range microEngrams {
			vec := vecs[i*dim : (i+1)*dim]
			if storeErr := rp.store.UpdateEmbedding(ctx, eng.ID, vec); storeErr != nil {
				slog.Warn("retroactive processor: UpdateEmbedding failed",
					"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
				rp.statsMu.Lock()
				rp.stats.Errors++
				rp.statsMu.Unlock()
				continue
			}
			if storeErr := rp.store.HNSWInsert(ctx, eng.ID, vec); storeErr != nil {
				slog.Warn("retroactive processor: HNSWInsert failed",
					"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
			}
			if storeErr := rp.store.AutoLinkByEmbedding(ctx, eng.ID, vec); storeErr != nil {
				slog.Warn("retroactive processor: AutoLinkByEmbedding failed",
					"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
			}
			if storeErr := rp.store.SetDigestFlag(ctx, eng.ID, rp.flagBit); storeErr != nil {
				slog.Warn("retroactive processor: failed to set digest flag",
					"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
				rp.statsMu.Lock()
				rp.stats.Errors++
				rp.statsMu.Unlock()
				continue
			}
			rp.statsMu.Lock()
			rp.stats.Processed++
			processed := rp.stats.Processed
			rp.statsMu.Unlock()

			if processed%1000 == 0 {
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					rate := float64(processed) / elapsed
					remaining := total - processed
					etaSeconds := int64(float64(remaining) / rate)
					rp.statsMu.Lock()
					rp.stats.RatePerSec = rate
					rp.stats.ETASeconds = etaSeconds
					rp.statsMu.Unlock()
					slog.Info("retroactive processor: progress",
						"plugin", rp.plugin.Name(),
						"processed", processed,
						"total", total,
						"rate_per_sec", rate,
						"eta_seconds", etaSeconds)
				}
			}
		}
		microEngrams = microEngrams[:0]
		microTexts = microTexts[:0]
	}

	for iter.Next() {
		select {
		case <-ctx.Done():
			flushMicroBatch()
			slog.Info("retroactive processor: cancelled mid-batch", "plugin", rp.plugin.Name())
			return true
		default:
		}

		// Cap per-pass work to bound iterator lifetime during bulk imports.
		if batchCount >= maxBatchSize {
			flushMicroBatch()
			rp.Notify()
			break
		}

		eng := iter.Engram()
		if eng == nil {
			continue
		}

		if isEmbedPlugin {
			// Accumulate into micro-batch; flush when full.
			microEngrams = append(microEngrams, eng)
			microTexts = append(microTexts, eng.Concept+" "+eng.Content)
			batchCount++
			if len(microEngrams) >= microBatchSize {
				flushMicroBatch()
			}
			if batchCount%100 == 0 {
				runtime.Gosched()
			}
			continue
		}

		// Non-embed (enrich) path: one-at-a-time as before.
		if err := rp.processEngram(ctx, eng); err != nil {
			if errors.Is(err, ErrNothingToEnrich) {
				// Nothing to enrich is not a failure — mark the engram as
				// enrichment-complete so it is not retried on the next scan.
				slog.Debug("retroactive processor: nothing to enrich, marking complete",
					"plugin", rp.plugin.Name(),
					"engram_id", eng.ID.String())
				if flagErr := rp.store.SetDigestFlag(ctx, eng.ID, rp.flagBit); flagErr != nil {
					slog.Warn("retroactive processor: failed to set digest flag after nothing-to-enrich",
						"plugin", rp.plugin.Name(),
						"engram_id", eng.ID.String(),
						"error", flagErr)
				}
				rp.statsMu.Lock()
				rp.stats.Processed++
				rp.statsMu.Unlock()
				batchCount++
				continue
			}
			slog.Warn("retroactive processor: failed to process engram",
				"plugin", rp.plugin.Name(),
				"engram_id", eng.ID.String(),
				"error", err)
			rp.statsMu.Lock()
			rp.stats.Errors++
			rp.statsMu.Unlock()
			continue
		}

		if err := rp.store.SetDigestFlag(ctx, eng.ID, rp.flagBit); err != nil {
			slog.Warn("retroactive processor: failed to set digest flag",
				"plugin", rp.plugin.Name(),
				"engram_id", eng.ID.String(),
				"error", err)
			rp.statsMu.Lock()
			rp.stats.Errors++
			rp.statsMu.Unlock()
			continue
		}

		rp.statsMu.Lock()
		rp.stats.Processed++
		processed := rp.stats.Processed
		rp.statsMu.Unlock()

		batchCount++

		if batchCount%100 == 0 {
			runtime.Gosched()
		}

		if processed%1000 == 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				rate := float64(processed) / elapsed
				remaining := total - processed
				etaSeconds := int64(float64(remaining) / rate)

				rp.statsMu.Lock()
				rp.stats.RatePerSec = rate
				rp.stats.ETASeconds = etaSeconds
				rp.statsMu.Unlock()

				slog.Info("retroactive processor: progress",
					"plugin", rp.plugin.Name(),
					"processed", processed,
					"total", total,
					"rate_per_sec", rate,
					"eta_seconds", etaSeconds)
			}
		}
	}

	// Flush any remaining micro-batch at end of iterator.
	flushMicroBatch()

	rp.statsMu.Lock()
	rp.stats.Status = "idle"
	passProcessed := rp.stats.Processed - passStart
	totalProcessed := rp.stats.Processed
	totalErrors := rp.stats.Errors
	rp.statsMu.Unlock()

	if passProcessed > 0 {
		slog.Info("retroactive processor: complete",
			"plugin", rp.plugin.Name(),
			"pass_processed", passProcessed,
			"total_processed", totalProcessed,
			"errors", totalErrors)
	} else {
		slog.Debug("retroactive processor: idle (phantom count)",
			"plugin", rp.plugin.Name(),
			"reported_total", total)
	}

	return true
}

func (rp *RetroactiveProcessor) processEngram(ctx context.Context, eng *Engram) error {
	// Check if this is an embed plugin
	if embed, ok := rp.plugin.(EmbedPlugin); ok {
		// Call Embed with the concept and content
		text := eng.Concept + " " + eng.Content
		vec, err := embed.Embed(ctx, []string{text})
		if err != nil {
			return err
		}

		// Store the embedding
		if err := rp.store.UpdateEmbedding(ctx, eng.ID, vec); err != nil {
			return err
		}

		// Insert into HNSW index
		if err := rp.store.HNSWInsert(ctx, eng.ID, vec); err != nil {
			return err
		}

		// Auto-link by embedding
		if err := rp.store.AutoLinkByEmbedding(ctx, eng.ID, vec); err != nil {
			return err
		}

		return nil
	}

	// Check if this is an enrich plugin
	if enrich, ok := rp.plugin.(EnrichPlugin); ok {
		// Read per-stage digest flags so we don't re-run stages the caller already provided.
		// engramHasEntities previously used len(eng.KeyPoints) > 0, which incorrectly
		// conflated summarization keypoints with entity extraction. Flags are authoritative.
		flags, err := rp.store.GetDigestFlags(ctx, eng.ID)
		if err != nil {
			if errors.Is(err, pebble.ErrNotFound) {
				flags = 0
			} else {
				slog.Warn("enrich: failed to read digest flags, skipping engram", "id", eng.ID.String(), "err", err)
				return nil
			}
		}
		hasSummary := eng.Summary != "" || (flags&DigestSummarized != 0)
		hasEntities := flags&DigestEntities != 0
		hasRelationships := flags&DigestRelationships != 0
		hasClassification := flags&DigestClassified != 0

		// All pipeline stages are already done for this engram — skip it entirely.
		if hasSummary && hasEntities && hasRelationships && hasClassification {
			return nil
		}

		// Call Enrich for missing fields.
		result, err := enrich.Enrich(ctx, eng)
		if err != nil {
			return err
		}
		if result == nil {
			return fmt.Errorf("enrich returned nil result")
		}

		// Only overwrite fields the caller didn't provide.
		// hasSummary covers both eng.Summary != "" and DigestSummarized flag;
		// KeyPoints are part of the summarization output so they're guarded by hasSummary.
		if hasSummary {
			result.Summary = eng.Summary
			if len(eng.KeyPoints) > 0 {
				result.KeyPoints = eng.KeyPoints
			}
		}

		if hasEntities {
			result.Entities = nil
		}
		if hasRelationships {
			result.Relationships = nil
		}

		if err := PersistEnrichmentResult(ctx, rp.store, eng.ID, result); err != nil {
			return err
		}

		return nil
	}

	return nil
}
