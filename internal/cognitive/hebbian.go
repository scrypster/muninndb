package cognitive

import (
	"context"
	"log/slog"
	"math"
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
	// DecayAssocWeights multiplies all association weights for ws by decayFactor,
	// deleting entries that fall below minWeight. Returns count deleted.
	// archiveThreshold > 0 enables moving strong floor-hit edges to the 0x25 archive namespace.
	DecayAssocWeights(ctx context.Context, ws [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error)
	// UpdateAssocWeightBatch atomically updates multiple association weights in a single batch.
	UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error
}

// AssocWeightUpdate represents a single weight update for batching.
type AssocWeightUpdate struct {
	WS         [8]byte
	Src        [16]byte
	Dst        [16]byte
	Weight     float32
	CountDelta uint32 // number of co-activations observed for this pair in the batch
}

// CoActivationEvent records a set of engrams that were retrieved together.
type CoActivationEvent struct {
	WS      [8]byte
	At      time.Time
	Engrams []CoActivatedEngram
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
	store       HebbianStore
	db *pebble.DB // optional, reserved for future persistence

	// OnWeightUpdate is called after each association weight update.
	// Used by the Engine to forward cognitive events to the trigger system.
	// Must not block — the trigger system drops events if its channel is full.
	OnWeightUpdate func(ws [8]byte, id [16]byte, field string, oldVal, newVal float64)

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
	hw := &HebbianWorker{
		store:          store,
		db:             db,
		OnWeightUpdate: onWeightUpdate, // set BEFORE the background goroutine starts
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
	}
	pairs := make(map[pairKey]*pairStats)

	for _, event := range batch {
		for i := 0; i < len(event.Engrams); i++ {
			for j := i + 1; j < len(event.Engrams); j++ {
				key := canonicalPair(event.Engrams[i].ID, event.Engrams[j].ID)
				signal := event.Engrams[i].Score * event.Engrams[j].Score // geometric product
				if ps, ok := pairs[key]; ok {
					ps.count++
					ps.signal += signal
				} else {
					pairs[key] = &pairStats{count: 1, signal: signal, ws: event.WS}
				}
			}
		}
	}

	// Apply multiplicative updates in log-space to prevent float64 overflow
	// when effectiveSignal is large (math.Pow(1+lr, n) → +Inf for n in the thousands).
	// Collect all updates into a batch for atomic commit.
	var updates []AssocWeightUpdate
	var callbacks []struct {
		ws   [8]byte
		id   [16]byte
		old  float64
		new  float64
	}

	for pair, stats := range pairs {
		const hebbianSignalEpsilon = 1e-9
		effectiveSignal := stats.signal
		// NOTE: stats.signal = Σ(scoreA_i × scoreB_i). Scores are clamped to [0,1] by
		// computeComponents in the activation engine, so effectiveSignal ≤ stats.count.
		// If effectiveSignal is negligible (all scores near zero), skip — no rational learning signal.
		if effectiveSignal < hebbianSignalEpsilon {
			continue
		}

		// Get current weight
		current, err := hw.store.GetAssocWeight(ctx, stats.ws, pair.a, pair.b)
		if err != nil {
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

		updates = append(updates, AssocWeightUpdate{
			WS:         stats.ws,
			Src:        pair.a,
			Dst:        pair.b,
			Weight:     newWeight,
			CountDelta: countDelta,
		})

		if hw.OnWeightUpdate != nil {
			callbacks = append(callbacks, struct {
				ws   [8]byte
				id   [16]byte
				old  float64
				new  float64
			}{stats.ws, pair.a, float64(current), float64(newWeight)})
		}
	}

	// Atomically commit all updates in a single batch
	if len(updates) > 0 {
		if err := hw.store.UpdateAssocWeightBatch(ctx, updates); err != nil {
			slog.Error("hebbian: failed to persist association weights batch",
				"batch_size", len(updates),
				"error", err)
		}
	}

	// Fire callbacks after batch commit succeeds
	for _, cb := range callbacks {
		hw.OnWeightUpdate(cb.ws, cb.id, "association_weight", cb.old, cb.new)
	}

	return nil
}
