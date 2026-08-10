package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/engine/circuit"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
)

// errLLMFailed is an unexported sentinel wrapping errors that originate from
// the LLM call itself (bad output, nil result, parse error). It signals a
// permanent failure for the engram — distinct from transient failures (circuit
// open, context cancelled) and storage errors (persistence after a successful
// LLM call). Only the enrich path uses this sentinel.
var errLLMFailed = errors.New("enrich: llm call failed")

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
	flagBit  uint16 // DigestEmbed or DigestEnrich
	stats    RetroactiveStats
	statsMu  sync.RWMutex
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	notifyCh chan struct{} // buffered(1); non-blocking send from Notify()

	// vaultGuards establish a clear boundary. Plugin calls hold a per-vault
	// admission through persistence; ClearVault invalidates the generation,
	// closes admission, and waits for admitted calls to drain. Scope note: an
	// invalidation aborts the whole cross-vault pass (ScanWithoutFlag is
	// global), so unrelated vaults' pending work waits until the next pass —
	// their admissions are untouched, but the sweep serving them is not.
	// Sizing: the map holds one entry per vault EVER SCANNED, not ever
	// cleared — lockCurrentVault creates entries on the scan path.
	vaultGuards sync.Map // map[[8]byte]*retroactiveVaultGuard

	// onEmbed, when set, is called after an engram's embedding is computed and
	// inserted into HNSW. The engine uses it to re-evaluate push subscriptions
	// with the now-available vector (#512). Only set on the embed processor.
	onEmbed func(eng *Engram, vec []float32)
}

type retroactiveVaultGuard struct {
	mu         sync.Mutex
	generation atomic.Uint64
	readers    int
	clearing   bool
	changed    chan struct{}
}

// SetOnEmbed registers a callback invoked after each engram's embedding is
// computed and inserted into the index. Must be called before Start.
func (rp *RetroactiveProcessor) SetOnEmbed(fn func(eng *Engram, vec []float32)) {
	rp.onEmbed = fn
}

// NewRetroactiveProcessor creates a new processor for a plugin.
func NewRetroactiveProcessor(store PluginStore, p Plugin, flagBit uint16) *RetroactiveProcessor {
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

// BeginVaultClear invalidates scans that started before a destructive clear,
// closes per-vault admission, and waits for already-admitted plugin calls to
// finish persisting. If ctx is cancelled while waiting, the boundary is
// abandoned and no clear may proceed. The returned function releases a
// successfully established boundary and wakes the processor.
//
// The wait is bounded ONLY by the caller's context: an admitted enrich call
// can hold admission for the provider's full HTTP timeout, so a clear against
// a slow provider blocks for that long unless the caller's ctx carries its
// own deadline. Callers that must not stall should pass one.
func (rp *RetroactiveProcessor) BeginVaultClear(ctx context.Context, ws [8]byte) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	guard := rp.vaultGuard(ws)

	guard.mu.Lock()
	for guard.clearing {
		changed := guard.changed
		guard.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
		guard.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		guard.mu.Unlock()
		return nil, err
	}

	// The first increment invalidates scans that predate the clear request.
	guard.clearing = true
	guard.generation.Add(1)
	guard.signalLocked()

	for guard.readers > 0 {
		changed := guard.changed
		guard.mu.Unlock()
		select {
		case <-ctx.Done():
			guard.mu.Lock()
			guard.clearing = false
			guard.generation.Add(1)
			guard.signalLocked()
			guard.mu.Unlock()
			rp.Notify()
			return nil, ctx.Err()
		case <-changed:
		}
		guard.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		guard.clearing = false
		guard.generation.Add(1)
		guard.signalLocked()
		guard.mu.Unlock()
		rp.Notify()
		return nil, err
	}
	guard.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			guard.mu.Lock()
			// The second increment invalidates scans opened while the clear held
			// admission. Only scans starting after this release observe the final
			// generation and may admit work from the rebuilt Pebble view.
			guard.generation.Add(1)
			guard.clearing = false
			guard.signalLocked()
			guard.mu.Unlock()
		})
		rp.Notify()
	}, nil
}

func (rp *RetroactiveProcessor) vaultGuard(ws [8]byte) *retroactiveVaultGuard {
	guard, _ := rp.vaultGuards.LoadOrStore(ws, &retroactiveVaultGuard{
		changed: make(chan struct{}),
	})
	return guard.(*retroactiveVaultGuard)
}

func (guard *retroactiveVaultGuard) signalLocked() {
	close(guard.changed)
	guard.changed = make(chan struct{})
}

func (guard *retroactiveVaultGuard) release() {
	guard.mu.Lock()
	guard.readers--
	if guard.readers == 0 {
		guard.signalLocked()
	}
	guard.mu.Unlock()
}

func (rp *RetroactiveProcessor) vaultGeneration(ws [8]byte) uint64 {
	guard, ok := rp.vaultGuards.Load(ws)
	if !ok {
		return 0
	}
	return guard.(*retroactiveVaultGuard).generation.Load()
}

func (rp *RetroactiveProcessor) snapshotVaultGenerations() map[[8]byte]uint64 {
	snapshot := make(map[[8]byte]uint64)
	rp.vaultGuards.Range(func(key, value any) bool {
		snapshot[key.([8]byte)] = value.(*retroactiveVaultGuard).generation.Load()
		return true
	})
	return snapshot
}

func (rp *RetroactiveProcessor) lockCurrentVault(ws [8]byte, snapshot map[[8]byte]uint64) (*retroactiveVaultGuard, bool) {
	guard := rp.vaultGuard(ws)
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if guard.clearing || guard.generation.Load() != snapshot[ws] {
		return nil, false
	}
	guard.readers++
	return guard, true
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

// Plugin returns the plugin associated with this processor.
func (p *RetroactiveProcessor) Plugin() Plugin {
	return p.plugin
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
// Both embed and enrich processors skip permanently-failed engrams to prevent
// infinite retry loops that trip the circuit breaker and block other memories.
func (rp *RetroactiveProcessor) skipFlags() uint16 {
	if rp.flagBit == DigestEmbed {
		return DigestEmbedFailed
	}
	return DigestEnrichFailed
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
	// Capture generations before opening the Pebble snapshot. A vault clear
	// increments its generation before waiting for an in-flight call, so an old
	// iterator can never gain admission after the clear boundary advances.
	scanGenerations := rp.snapshotVaultGenerations()

	// Reset rate/ETA at the start of every pass so stale values from a prior
	// pass don't leak into the embed-status API response while the processor is idle.
	rp.statsMu.Lock()
	rp.stats.RatePerSec = 0
	rp.stats.ETASeconds = 0
	rp.statsMu.Unlock()

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
	dimSkipped := 0 // engrams skipped for a vault-dimension mismatch this pass (#582)

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
	microVaults := make([][8]byte, 0, microBatchSize)
	passInvalidated := false

	flushMicroBatch := func() {
		if !isEmbedPlugin || len(microEngrams) == 0 {
			return
		}

		vaultSet := make(map[[8]byte]struct{}, len(microVaults))
		for _, ws := range microVaults {
			vaultSet[ws] = struct{}{}
		}
		vaults := make([][8]byte, 0, len(vaultSet))
		for ws := range vaultSet {
			vaults = append(vaults, ws)
		}
		sort.Slice(vaults, func(i, j int) bool {
			return bytes.Compare(vaults[i][:], vaults[j][:]) < 0
		})

		locked := make(map[[8]byte]*retroactiveVaultGuard, len(vaults))
		stale := make(map[[8]byte]bool)
		for _, ws := range vaults {
			guard, current := rp.lockCurrentVault(ws, scanGenerations)
			if !current {
				stale[ws] = true
				passInvalidated = true
				continue
			}
			locked[ws] = guard
		}
		defer func() {
			for _, guard := range locked {
				guard.release()
			}
		}()

		if len(stale) > 0 {
			liveEngrams := microEngrams[:0]
			liveTexts := microTexts[:0]
			liveVaults := microVaults[:0]
			for i, ws := range microVaults {
				if stale[ws] {
					continue
				}
				liveEngrams = append(liveEngrams, microEngrams[i])
				liveTexts = append(liveTexts, microTexts[i])
				liveVaults = append(liveVaults, ws)
			}
			microEngrams = liveEngrams
			microTexts = liveTexts
			microVaults = liveVaults
			if len(microEngrams) == 0 {
				return
			}
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
			microVaults = microVaults[:0]
			return
		}
		dim := len(vecs) / len(microEngrams)
		for i, eng := range microEngrams {
			vec := vecs[i*dim : (i+1)*dim]
			if storeErr := rp.store.UpdateEmbedding(ctx, eng.ID, vec); storeErr != nil {
				var dimErr *hnswpkg.DimMismatchError
				if errors.As(storeErr, &dimErr) {
					// Vault-level condition (#582), not an engram failure: leave
					// the engram pending — deliberately NO DigestEmbedFailed — so
					// it embeds normally once the vault is re-embedded or the
					// configuration is fixed. (DigestEmbedFailed shares bit 0x80
					// with DigestEnrichFailed; stamping it here would also stop
					// enrichment.) The pre-scan CheckEmbedDim makes this branch a
					// rare race, not the steady state.
					slog.Warn("retroactive processor: embedding dimension mismatch — engram left pending until the vault is re-embedded (`muninn vault reembed`)",
						"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(),
						"embedder_dim", dimErr.Got, "vault_dim", dimErr.Want)
				} else {
					slog.Warn("retroactive processor: UpdateEmbedding failed",
						"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
				}
				rp.statsMu.Lock()
				rp.stats.Errors++
				rp.statsMu.Unlock()
				continue
			}
			if storeErr := rp.store.HNSWInsert(ctx, eng.ID, vec); storeErr != nil {
				var dimErr *hnswpkg.DimMismatchError
				if errors.As(storeErr, &dimErr) {
					// Refused by the registry's atomic guard (#582) — e.g. a
					// concurrent first insert established a different dimension
					// after the UpdateEmbedding pre-check passed. Leave the
					// engram pending (no processed flag, no push notification):
					// the next pass re-evaluates it against the settled vault.
					slog.Warn("retroactive processor: HNSW refused embedding dimension — engram left pending",
						"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(),
						"embedder_dim", dimErr.Got, "vault_dim", dimErr.Want)
					rp.statsMu.Lock()
					rp.stats.Errors++
					rp.statsMu.Unlock()
					continue
				}
				slog.Warn("retroactive processor: HNSWInsert failed",
					"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", storeErr)
			} else if rp.onEmbed != nil {
				// The vector is now searchable — let the engine re-evaluate push
				// subscriptions with it (#512). Copy first: vec is a sub-slice of
				// the batch result and the callback hands it to an async consumer.
				rp.onEmbed(eng, append([]float32(nil), vec...))
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
			rp.statsMu.Unlock()
		}
		microEngrams = microEngrams[:0]
		microTexts = microTexts[:0]
		microVaults = microVaults[:0]

		// Rate/ETA fires after every micro-batch (not gated on %100 — always update).
		// Use pass-local count (flushedProcessed - passStart) so rate reflects this
		// pass's throughput, not the cumulative total across all passes.
		// Log message fires only at 100-engram boundaries to avoid log spam.
		rp.statsMu.RLock()
		flushedProcessed := rp.stats.Processed
		rp.statsMu.RUnlock()
		passProcessedSoFar := flushedProcessed - passStart
		if passProcessedSoFar > 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				rate := float64(passProcessedSoFar) / elapsed
				remaining := total - passProcessedSoFar
				etaSeconds := int64(float64(remaining) / rate)
				rp.statsMu.Lock()
				rp.stats.RatePerSec = rate
				rp.stats.ETASeconds = etaSeconds
				rp.statsMu.Unlock()
				if passProcessedSoFar%100 == 0 {
					slog.Info("retroactive: progress",
						"plugin", rp.plugin.Name(),
						"processed", flushedProcessed,
						"total", total,
						"rate_per_sec", rate,
						"eta_seconds", etaSeconds)
				}
			}
		}
	}

	type enrichAction uint8
	const (
		enrichContinue enrichAction = iota
		enrichStopPass
		enrichReturnSuccess
		enrichInvalidatePass
	)

engramLoop:
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
		ws := iter.CurrentWS()

		if isEmbedPlugin {
			// Skip engrams of a vault whose established dimension does not
			// match the active embedder BEFORE paying for inference (#582).
			// Deliberately no failure flag: the mismatch is a vault-level
			// configuration condition (resolved by `muninn vault reembed` or
			// a config fix), and skipped engrams embed normally afterwards.
			// Skips do not count toward the per-pass cap — one cheap check
			// per engram per pass, no inference, no hot re-notify loop.
			if dim := embedPlugin.Dimension(); dim > 0 {
				if dimErr := rp.store.CheckEmbedDim(ctx, eng.ID, dim); dimErr != nil {
					dimSkipped++
					continue
				}
			}
			// Accumulate into micro-batch; flush when full.
			microEngrams = append(microEngrams, eng)
			microTexts = append(microTexts, eng.Concept+" "+eng.Content)
			microVaults = append(microVaults, ws)
			batchCount++
			if len(microEngrams) >= microBatchSize {
				flushMicroBatch()
				if passInvalidated {
					// No self-notify: the clear that invalidated this pass owns
					// the wake-up (its finish closure and cancellation arm both
					// call rp.Notify at exactly the right moment). A notify here
					// spins — the reawakened pass is rejected by the same held
					// boundary and re-notifies itself, measured at ~175k
					// passes/sec for the clear's whole duration, starving
					// unrelated vaults' scans.
					break
				}
			}
			if batchCount%100 == 0 {
				runtime.Gosched()
			}
			continue
		}

		// Non-embed (enrich) path: one-at-a-time as before. Keep the admission
		// and its deferred release in this per-engram closure so every return
		// path—including provider failures and cancellation—releases it.
		action, processed := func() (enrichAction, int64) {
			guard, current := rp.lockCurrentVault(ws, scanGenerations)
			if !current {
				return enrichInvalidatePass, 0
			}
			defer guard.release()

			if err := rp.processEnrichEngram(ctx, eng); err != nil {
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
					return enrichContinue, 0
				}
				slog.Warn("retroactive processor: failed to process engram",
					"plugin", rp.plugin.Name(),
					"engram_id", eng.ID.String(),
					"error", err)
				rp.statsMu.Lock()
				rp.stats.Errors++
				rp.statsMu.Unlock()
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return enrichReturnSuccess, 0
				}
				// Permanent per-engram failures — LLM-originated (bad output, parse
				// error) or content-caused provider 4xx (400/413/422/404). Mark
				// DigestEnrichFailed so the processor does not retry indefinitely
				// (which would trip the circuit breaker) and, crucially, so
				// ScanWithoutFlag excludes this engram on the next pass. A content 4xx
				// that sorts first would otherwise re-break every pass and wedge every
				// follower forever (#587). Continue so the rest of the batch drains.
				// Storage/persistence errors are NOT latched — they are transient and
				// retried once storage recovers.
				if errors.Is(err, errLLMFailed) {
					if flagErr := rp.store.SetDigestFlag(ctx, eng.ID, DigestEnrichFailed); flagErr != nil {
						slog.Warn("retroactive processor: failed to set DigestEnrichFailed",
							"plugin", rp.plugin.Name(), "engram_id", eng.ID.String(), "error", flagErr)
					}
					return enrichContinue, 0
				}
				if errors.Is(err, circuit.ErrOpen) || IsProviderError(err) {
					// Systemic provider condition (throttle, auth/config outage,
					// transport): stop this pass so the processor cannot fan the
					// failure across the pending queue. The engram stays pending and
					// retries once the provider recovers.
					return enrichStopPass, 0
				}
				return enrichContinue, 0
			}

			if err := rp.store.SetDigestFlag(ctx, eng.ID, rp.flagBit); err != nil {
				slog.Warn("retroactive processor: failed to set digest flag",
					"plugin", rp.plugin.Name(),
					"engram_id", eng.ID.String(),
					"error", err)
				rp.statsMu.Lock()
				rp.stats.Errors++
				rp.statsMu.Unlock()
				return enrichContinue, 0
			}

			rp.statsMu.Lock()
			rp.stats.Processed++
			processed := rp.stats.Processed
			rp.statsMu.Unlock()
			batchCount++
			return enrichContinue, processed
		}()

		switch action {
		case enrichStopPass:
			break engramLoop
		case enrichReturnSuccess:
			return true
		case enrichInvalidatePass:
			// No self-notify — see the micro-batch invalidation branch above.
			passInvalidated = true
			break engramLoop
		}
		if processed == 0 {
			continue
		}

		if batchCount%100 == 0 {
			runtime.Gosched()
		}

		passProcessedSoFar := processed - passStart
		if passProcessedSoFar%100 == 0 {
			elapsed := time.Since(startTime).Seconds()
			if elapsed > 0 {
				rate := float64(passProcessedSoFar) / elapsed
				remaining := total - passProcessedSoFar
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

	// Flush any remaining micro-batch at end of iterator. An invalidated pass
	// does NOT re-notify here — see the micro-batch invalidation branch.
	flushMicroBatch()

	// One summary line per pass instead of one per engram — a mismatched
	// vault can hold thousands of pending engrams.
	if dimSkipped > 0 {
		slog.Warn("retroactive processor: engrams skipped — vault embedding dimension does not match the active embedder; run `muninn vault reembed` to re-embed them",
			"plugin", rp.plugin.Name(), "skipped", dimSkipped)
	}

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

func (rp *RetroactiveProcessor) processEnrichEngram(ctx context.Context, eng *Engram) error {
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
			// Transient: circuit open, context cancelled/deadline exceeded.
			// Do not wrap — the caller must not mark the engram as permanently failed.
			if errors.Is(err, circuit.ErrOpen) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if IsRetryableProviderError(err) {
				return fmt.Errorf("transient enrichment provider failure: %w", err)
			}
			if IsPermanentContentProviderError(err) {
				// Content-caused 4xx (400/413/422/404): this engram's content is
				// too large or malformed and will fail identically forever. Wrap
				// with errLLMFailed so the caller stamps DigestEnrichFailed and
				// continues, exactly as develop did for LLM failures (#587).
				return fmt.Errorf("%w: %w", errLLMFailed, err)
			}
			if IsProviderError(err) {
				return fmt.Errorf("enrichment provider failure: %w", err)
			}
			// Permanent per-engram LLM failure (bad output or parse error).
			// Wrap with errLLMFailed so the caller can mark DigestEnrichFailed.
			return fmt.Errorf("%w: %w", errLLMFailed, err)
		}
		if result == nil {
			return errLLMFailed
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
