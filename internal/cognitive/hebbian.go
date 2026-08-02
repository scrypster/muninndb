package cognitive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

const (
	HebbianLearningRate = 0.01
	HebbianPassInterval = time.Minute
)

// hebbianMetadataKey returns the Pebble key for Hebbian worker metadata.
func hebbianMetadataKey(name string) []byte {
	return append([]byte{0x19, 0x01}, name...)
}

// HebbianStore is the storage interface for Hebbian updates.
type HebbianStore interface {
	UpdateAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte, newWeight float32) error
	GetAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte) (float32, error)
	// DecayAssocWeights applies the peak-anchored, elapsed-time decay ceiling
	// (COG-27) to every association under ws. Returns count deleted.
	// halfLife is the wall-clock half-life of an unused edge; it must be > 0.
	// archiveThreshold > 0 enables moving strong floor-hit edges to the 0x25 archive namespace.
	DecayAssocWeights(ctx context.Context, ws [8]byte, halfLife time.Duration, minWeight float32, archiveThreshold float64) (int, error)
	// UpdateAssocWeightBatch updates multiple association weights in a single
	// Pebble batch. What it writes is atomic; what it applies may be a SUBSET.
	// A pair whose existing metadata cannot be read is skipped rather than
	// overwritten with fabricated defaults, and reported through an error that
	// also exposes SkippedUpdates() []int (indices into updates). Callers that
	// act on an update landing must consult it.
	UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error
}

// AssocWeightUpdate represents a single weight update for batching.
type AssocWeightUpdate struct {
	WS         [8]byte
	Src        [16]byte
	Dst        [16]byte
	Weight     float32
	CountDelta uint32 // number of co-activations observed for this pair in the batch
	// LastActivatedAt is the Unix-seconds stamp to record as the edge's
	// lastActivated, carried through from CoActivationEvent.At. ZERO =
	// time.Now() at the storage layer, i.e. the pre-#779 behaviour.
	LastActivatedAt int32
}

// CoActivationEvent records a set of engrams that were retrieved together.
type CoActivationEvent struct {
	WS      [8]byte
	At      time.Time
	Engrams []CoActivatedEngram
	// LTP is the per-vault LTP configuration resolved from the vault's plasticity config.
	// When nil, LTP is disabled for this event. This allows per-vault LTP settings
	// even though the HebbianWorker is shared across all vaults.
	LTP *LTPConfig
}

// CoActivatedEngram is one engram in a co-activation event.
type CoActivatedEngram struct {
	ID    [16]byte
	Score float64
}

// pairKey is a canonical (sorted) pair of engram IDs.
type pairKey struct {
	a, b [16]byte
}

func canonicalPair(x, y [16]byte) pairKey {
	for i := 0; i < 16; i++ {
		if x[i] < y[i] {
			return pairKey{a: x, b: y}
		} else if x[i] > y[i] {
			return pairKey{a: y, b: x}
		}
	}
	return pairKey{a: x, b: y}
}

// HebbianWorker strengthens co-activated associations.
type HebbianWorker struct {
	*Worker[CoActivationEvent]
	store HebbianStore
	db    *pebble.DB // optional, reserved for future persistence

	// OnWeightUpdate is called after each association weight update.
	// Used by the Engine to forward cognitive events to the trigger system.
	// Must not block — the trigger system drops events if its channel is full.
	OnWeightUpdate func(ws [8]byte, id [16]byte, field string, oldVal, newVal float64)

	// LTP (Long-Term Potentiation) configuration and state.
	// When ltpCfg is nil, LTP is disabled and behavior is unchanged.
	ltpCfg   *LTPConfig
	ltpState *ltpState

	// loggedReadFaults suppresses repeat log lines for a pair whose current
	// association weight could not be read. See shouldLogReadFault.
	readFaultMu      sync.Mutex
	loggedReadFaults map[pairKey]struct{}

	// internal stop channel for tests and lifecycle management.
	stopCh chan struct{}
	doneCh chan struct{}
}

// NewHebbianWorker creates a new Hebbian worker with no persistence and no callback.
// Use NewHebbianWorkerWithDB to supply a callback before the background goroutine starts,
// eliminating the initialization order race described in the field notes below.
func NewHebbianWorker(store HebbianStore) *HebbianWorker {
	return NewHebbianWorkerWithDB(store, nil, nil)
}

// NewHebbianWorkerWithDB creates a new Hebbian worker with optional Pebble persistence
// and an optional OnWeightUpdate callback.
//
// Initialization order requirement: the callback is assigned to hw.OnWeightUpdate
// BEFORE the background goroutine is started. This eliminates the race where the
// goroutine could process a co-activation event and attempt to call OnWeightUpdate
// while the caller was still setting it after construction.
//
// Callers that previously did:
//
//	hw := NewHebbianWorkerWithDB(store, db)
//	hw.OnWeightUpdate = myCallback   // RACE: goroutine already running
//
// should now pass the callback directly:
//
//	hw := NewHebbianWorkerWithDB(store, db, myCallback)  // safe: set before goroutine starts
func NewHebbianWorkerWithDB(store HebbianStore, db *pebble.DB, onWeightUpdate func(ws [8]byte, id [16]byte, field string, oldVal, newVal float64)) *HebbianWorker {
	return NewHebbianWorkerWithLTP(store, db, onWeightUpdate, nil)
}

// NewHebbianWorkerWithLTP creates a new Hebbian worker with optional LTP configuration.
// When ltpCfg is nil, LTP is disabled and behavior is identical to NewHebbianWorkerWithDB.
func NewHebbianWorkerWithLTP(store HebbianStore, db *pebble.DB, onWeightUpdate func(ws [8]byte, id [16]byte, field string, oldVal, newVal float64), ltpCfg *LTPConfig) *HebbianWorker {
	hw := &HebbianWorker{
		store:          store,
		db:             db,
		OnWeightUpdate: onWeightUpdate, // set BEFORE the background goroutine starts
		ltpCfg:         ltpCfg,
		ltpState:       newLTPState(),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}

	hw.Worker = NewWorker[CoActivationEvent](
		5000, 100, HebbianPassInterval,
		hw.processBatch,
	)
	// Start the background run loop automatically.
	// IMPORTANT: OnWeightUpdate must be assigned before this goroutine starts
	// (done above) so no event is silently dropped due to a nil callback check.
	go func() {
		defer close(hw.doneCh)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-hw.stopCh
			cancel()
		}()
		hw.Worker.Run(ctx) //nolint:errcheck
	}()
	return hw
}

// Run bridges an external context to the auto-started worker's lifecycle.
// When ctx is cancelled, the worker stops. Blocks until the worker exits.
// This satisfies callers (tests, server) that start workers via Run(ctx).
// It does NOT start a second consumer goroutine — the auto-start in NewHebbianWorker
// owns the single processing loop; Run() only manages shutdown signalling.
func (hw *HebbianWorker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		hw.Stop()
	case <-hw.stopCh:
		// Worker already stopped externally (e.g., hw.Stop() called directly).
	}
	<-hw.doneCh
}

// IsPotentiated returns true if the given association pair is LTP-potentiated
// for the specified workspace. Returns false if LTP state is unavailable.
// Potentiation can occur via worker-level LTP config or per-event LTP config.
func (hw *HebbianWorker) IsPotentiated(ws [8]byte, pair pairKey) bool {
	if hw.ltpState == nil {
		return false
	}
	return hw.ltpState.isPotentiated(ws, pair)
}

// Stop signals the HebbianWorker to flush pending work and shut down.
// Blocks until the worker goroutine has exited.
func (hw *HebbianWorker) Stop() {
	select {
	case <-hw.stopCh:
		// already stopped
	default:
		close(hw.stopCh)
	}
	<-hw.doneCh
}

func (hw *HebbianWorker) processBatch(ctx context.Context, batch []CoActivationEvent) error {
	// Collect unique vault workspace prefixes in this batch.
	wsSet := make(map[[8]byte]struct{})
	for _, ev := range batch {
		wsSet[ev.WS] = struct{}{}
	}

	// Aggregate co-activations per pair
	type pairStats struct {
		count  int
		signal float64
		ws     [8]byte
		ltp    *LTPConfig // per-vault LTP config from the event (nil = use worker default)
		// at is the LATEST CoActivationEvent.At seen for this pair in the
		// batch, in Unix seconds; 0 when no event carried a time (the worker's
		// own submissions always do, direct constructions may not). Taking the
		// max rather than the last keeps the aggregation ORDER-INDEPENDENT,
		// which is the same property the log-space weight update has and the
		// reason a batch can be split or reordered without changing its result.
		at int64
	}
	pairs := make(map[pairKey]*pairStats)

	for _, event := range batch {
		for i := 0; i < len(event.Engrams); i++ {
			for j := i + 1; j < len(event.Engrams); j++ {
				key := canonicalPair(event.Engrams[i].ID, event.Engrams[j].ID)
				signal := event.Engrams[i].Score * event.Engrams[j].Score // geometric product
				var eventAt int64
				if !event.At.IsZero() {
					eventAt = event.At.Unix()
				}
				if ps, ok := pairs[key]; ok {
					ps.count++
					ps.signal += signal
					if eventAt > ps.at {
						ps.at = eventAt
					}
					if ps.ltp == nil {
						ps.ltp = event.LTP // keep non-nil LTP from later events
					}
				} else {
					pairs[key] = &pairStats{count: 1, signal: signal, ws: event.WS, ltp: event.LTP, at: eventAt}
				}
			}
		}
	}

	// Apply multiplicative updates in log-space to prevent float64 overflow
	// when effectiveSignal is large (math.Pow(1+lr, n) → +Inf for n in the thousands).
	// Collect all updates into a batch for atomic commit.
	var updates []AssocWeightUpdate
	// idx ties each pending callback to the update that must persist before it
	// may fire. OnWeightUpdate is NotifyCognitive in production, so a callback
	// for an update that did not land tells the trigger system about a weight
	// transition memory does not have.
	var callbacks []struct {
		idx int
		ws  [8]byte
		id  [16]byte
		old float64
		new float64
	}
	// readErrs collects pairs whose CURRENT weight could not be read. They never
	// enter updates, so they are invisible to the batch's own skip reporting;
	// they are folded into this function's returned error instead, which is what
	// Worker.Run counts in Stats().Errors.
	var readErrs []error

	for pair, stats := range pairs {
		const hebbianSignalEpsilon = 1e-9
		effectiveSignal := stats.signal
		// NOTE: stats.signal = Σ(scoreA_i × scoreB_i). Scores are clamped to [0,1] by
		// computeComponents in the activation engine, so effectiveSignal ≤ stats.count.
		// If effectiveSignal is negligible (all scores near zero), skip — no rational learning signal.
		if effectiveSignal < hebbianSignalEpsilon {
			continue
		}

		// Get current weight.
		//
		// A failed read is NOT a cold start. Skipping is the correct direction —
		// falling through would re-seed a live edge at the 0.01 cold-start weight,
		// which is the defect this branch exists to prevent — but the skip must
		// not be silent. This fault never reaches UpdateAssocWeightBatch, so the
		// AssocBatchSkipError machinery below never sees it; without reporting
		// here, a pair whose 0x14 record is corrupt (a PERMANENT, KEY-SCOPED
		// fault) would stop learning forever with nothing in Stats().Errors and
		// nothing in the logs. Report it through the same error this function
		// already returns, and log the pair at most once — the fault recurs in
		// every flush window, so an unguarded log line here is a flood.
		current, err := hw.store.GetAssocWeight(ctx, stats.ws, pair.a, pair.b)
		if err != nil {
			readErrs = append(readErrs, fmt.Errorf("pair %x->%x: %w", pair.a, pair.b, err))
			if hw.shouldLogReadFault(pair) {
				slog.Warn("hebbian: association weight read failed — pair skipped, not re-seeded; "+
					"further failures for this pair are counted but not logged",
					"vault_prefix", fmt.Sprintf("%x", stats.ws),
					"src", fmt.Sprintf("%x", pair.a),
					"dst", fmt.Sprintf("%x", pair.b),
					"error", err)
			}
			continue
		}

		// Seed cold-start associations: if weight is 0, initialize to 0.01
		if current <= 0 {
			current = 0.01
		}

		// log(current * (1+lr)^effectiveSignal) = log(current) + effectiveSignal * log(1+lr)
		logNew := math.Log(float64(current)) + effectiveSignal*math.Log(1.0+HebbianLearningRate)
		newWeight := float32(math.Min(1.0, math.Exp(logNew)))

		var countDelta uint32
		if stats.count > math.MaxUint32 {
			countDelta = math.MaxUint32
		} else {
			countDelta = uint32(stats.count)
		}

		// LTP: track co-activation count and enforce weight floor for potentiated pairs.
		// Event-level LTP config (from vault plasticity) takes precedence; fall back
		// to worker-level config for backward compatibility with direct construction.
		//
		// NOTE: The dream engine (consolidation/transitive.go) updates association
		// weights via direct store.UpdateAssocWeight() calls, bypassing HebbianWorker.
		// Dream can set weights below the LTP floor. This is a known interaction —
		// coordinating with dream is tracked separately.
		ltpCfg := stats.ltp
		if ltpCfg == nil {
			ltpCfg = hw.ltpCfg
		}
		if ltpCfg != nil && ltpCfg.Threshold > 0 {
			hw.ltpState.addCount(stats.ws, pair, countDelta, ltpCfg.Threshold)
			if hw.ltpState.isPotentiated(stats.ws, pair) && ltpCfg.WeightFloor > 0 {
				if newWeight < ltpCfg.WeightFloor {
					newWeight = ltpCfg.WeightFloor
				}
			}
		}

		// Carry the EVENT's own time to the edge rather than letting storage
		// stamp time.Now(): an event that waited in the worker's channel was
		// being stamped late, and an offline replay of historical events would
		// stamp all of them "now" and erase every interleaved decay pass.
		// Clamped to int32 because that is the on-disk width; a stamp outside
		// the representable range falls back to 0 = "stamp at write time".
		var lastAt int32
		if stats.at > 0 && stats.at <= math.MaxInt32 {
			lastAt = int32(stats.at)
		}

		updates = append(updates, AssocWeightUpdate{
			WS:              stats.ws,
			Src:             pair.a,
			Dst:             pair.b,
			Weight:          newWeight,
			CountDelta:      countDelta,
			LastActivatedAt: lastAt,
		})

		if hw.OnWeightUpdate != nil {
			callbacks = append(callbacks, struct {
				idx int
				ws  [8]byte
				id  [16]byte
				old float64
				new float64
			}{len(updates) - 1, stats.ws, pair.a, float64(current), float64(newWeight)})
		}
	}

	// Commit the updates. A store may apply the batch PARTIALLY: a pair whose
	// existing metadata cannot be read is skipped rather than overwritten with
	// fabricated defaults, and reported via an error that names the skipped
	// indices (storage.AssocBatchSkipError). Matching on the method rather than
	// the concrete type keeps this package free of a storage dependency.
	var batchErr error
	notPersisted := map[int]struct{}{}
	if len(updates) > 0 {
		if err := hw.store.UpdateAssocWeightBatch(ctx, updates); err != nil {
			batchErr = err
			var partial interface{ SkippedUpdates() []int }
			if errors.As(err, &partial) {
				skipped := partial.SkippedUpdates()
				for _, i := range skipped {
					notPersisted[i] = struct{}{}
				}
				slog.Error("hebbian: association weight updates skipped — existing metadata unreadable; "+
					"the rest of the batch was applied",
					"batch_size", len(updates),
					"skipped", len(skipped),
					"error", err)
			} else {
				// Nothing landed: no update in this batch is persisted.
				for i := range updates {
					notPersisted[i] = struct{}{}
				}
				slog.Error("hebbian: failed to persist association weights batch",
					"batch_size", len(updates),
					"error", err)
			}
		}
	}

	// Fire callbacks only for updates that actually persisted.
	for _, cb := range callbacks {
		if _, dropped := notPersisted[cb.idx]; dropped {
			continue
		}
		hw.OnWeightUpdate(cb.ws, cb.id, "association_weight", cb.old, cb.new)
	}

	// Fold the read failures into the same return. One mechanism covers both
	// halves of "this pair did not learn this pass": a pair skipped by the store
	// because its metadata was unreadable, and a pair skipped here because its
	// weight was unreadable. errors.Join keeps errors.As reaching the batch's
	// SkippedUpdates() through the combined error.
	if len(readErrs) > 0 {
		readErr := fmt.Errorf("hebbian: %d pair(s) skipped, current association weight unreadable: %w",
			len(readErrs), errors.Join(readErrs...))
		if batchErr != nil {
			batchErr = errors.Join(batchErr, readErr)
		} else {
			batchErr = readErr
		}
	}

	// Return the failure rather than swallowing it: Worker.Run counts a
	// non-nil process error in Stats().Errors, which is the only place a
	// persistence failure in this worker becomes observable. It does not stop
	// the worker.
	return batchErr
}

// readFaultLogCap bounds the suppression set. A read fault broad enough to
// exceed it is already reported through processBatch's error every window; the
// cap only stops an unbounded map from growing behind a transient, wide fault.
const readFaultLogCap = 4096

// shouldLogReadFault reports whether this pair's read failure has not been
// logged yet, recording it if so.
//
// A corrupt Pebble block is permanent and key-scoped, and the worker flushes
// every HebbianPassInterval, so the same pair fails in window after window.
// Logging it once turns a silent bug into an operator-visible one without
// trading it for a log line a minute, forever.
//
// Guarded because tests drive processBatch directly while the worker's own
// goroutine is live; in production it is called from that goroutine only.
func (hw *HebbianWorker) shouldLogReadFault(pair pairKey) bool {
	hw.readFaultMu.Lock()
	defer hw.readFaultMu.Unlock()
	if hw.loggedReadFaults == nil {
		hw.loggedReadFaults = make(map[pairKey]struct{})
	}
	if _, seen := hw.loggedReadFaults[pair]; seen {
		return false
	}
	if len(hw.loggedReadFaults) >= readFaultLogCap {
		return false
	}
	hw.loggedReadFaults[pair] = struct{}{}
	return true
}
