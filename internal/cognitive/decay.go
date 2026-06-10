package cognitive

import (
	"container/heap"
	"context"
	"math"
	"time"
)

const (
	DefaultFloor        = 0.05
	DefaultStability    = 14.0
	MaxStability        = 365.0
	StabilityGrowthRate = 20.0
	SpacingOptimal      = 7.0
	SpacingBonusFactor  = 0.5
	NegligibleDelta     = 0.001
)

// DecayStore is the storage interface required by the decay worker.
// ws is the 8-byte vault prefix used to scope all key operations.
type DecayStore interface {
	GetMetadataBatch(ctx context.Context, ws [8]byte, ids [][16]byte) ([]DecayMeta, error)
	UpdateRelevance(ctx context.Context, ws [8]byte, id [16]byte, relevance float32, stability float32) error
}

// DecayMeta is the metadata needed for decay computation.
type DecayMeta struct {
	ID          [16]byte
	LastAccess  time.Time
	AccessCount uint32
	Stability   float32
	Relevance   float32
}

// DecayCandidate is an item submitted to the decay worker.
type DecayCandidate struct {
	WS          [8]byte
	ID          [16]byte
	CreatedAt   time.Time // used to compute average spacing between accesses
	LastAccess  time.Time
	AccessCount uint32
	Stability   float32
	Relevance   float32 // current relevance at submission time; used as oldVal in OnDecayUpdate
}

// EbbinghausWithFloor computes the Ebbinghaus retention with a floor value.
func EbbinghausWithFloor(daysSinceAccess, stability, floor float64) float64 {
	if stability <= 0 {
		stability = DefaultStability
	}
	// Clamp clock skew: a LastAccess in the future yields a negative elapsed
	// time, which would make exp(-neg/stability) exceed 1 (retention is defined
	// on [floor, 1]). Treat a future access as "just accessed".
	if daysSinceAccess < 0 {
		daysSinceAccess = 0
	}
	r := math.Exp(-daysSinceAccess / stability)
	if r < floor {
		return floor
	}
	return r
}

// ComputeStability computes new stability from access count and spacing.
func ComputeStability(accessCount uint32, avgDaysBetweenAccesses float64) float64 {
	base := math.Log1p(float64(accessCount)) * StabilityGrowthRate
	spacing := math.Tanh(avgDaysBetweenAccesses / SpacingOptimal)
	stability := base * (1 + SpacingBonusFactor*spacing)
	if stability > MaxStability {
		stability = MaxStability
	}
	if stability < DefaultStability {
		stability = DefaultStability
	}
	return stability
}

// schedEntry is an entry in the decay schedule heap.
type schedEntry struct {
	id        [16]byte
	nextCheck time.Time
	index     int // heap index for O(log n) Fix()
}

// schedHeap is a min-heap ordered by nextCheck.
type schedHeap []*schedEntry

func (h schedHeap) Len() int           { return len(h) }
func (h schedHeap) Less(i, j int) bool { return h[i].nextCheck.Before(h[j].nextCheck) }
func (h schedHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *schedHeap) Push(x any) {
	e := x.(*schedEntry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *schedHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

// DecaySchedule manages when to next check each engram.
type DecaySchedule struct {
	h       schedHeap
	index   map[[16]byte]*schedEntry
	trigger chan struct{}
}

func NewDecaySchedule() *DecaySchedule {
	return &DecaySchedule{
		index:   make(map[[16]byte]*schedEntry),
		trigger: make(chan struct{}, 1),
	}
}

func (ds *DecaySchedule) Schedule(id [16]byte, nextCheck time.Time) {
	if e, ok := ds.index[id]; ok {
		e.nextCheck = nextCheck
		heap.Fix(&ds.h, e.index)
	} else {
		e := &schedEntry{id: id, nextCheck: nextCheck}
		heap.Push(&ds.h, e)
		ds.index[id] = e
	}
	// Notify if this is the new earliest
	select {
	case ds.trigger <- struct{}{}:
	default:
	}
}

func (ds *DecaySchedule) Next() (time.Time, bool) {
	if len(ds.h) == 0 {
		return time.Time{}, false
	}
	return ds.h[0].nextCheck, true
}

func (ds *DecaySchedule) PopDue() [16]byte {
	if len(ds.h) == 0 {
		return [16]byte{}
	}
	e := heap.Pop(&ds.h).(*schedEntry)
	delete(ds.index, e.id)
	return e.id
}

// DecayWorker applies Ebbinghaus decay to engrams.
type DecayWorker struct {
	*Worker[DecayCandidate]
	store    DecayStore
	schedule *DecaySchedule

	// OnDecayUpdate is called after each engram relevance update.
	// Used by the Engine to forward cognitive events to the trigger system.
	// Must not block.
	OnDecayUpdate func(ws [8]byte, id [16]byte, field string, oldVal, newVal float64)
}

// NewDecayWorker creates a new decay worker.
func NewDecayWorker(store DecayStore) *DecayWorker {
	dw := &DecayWorker{
		store:    store,
		schedule: NewDecaySchedule(),
	}
	dw.Worker = NewWorker[DecayCandidate](
		10000, 500, 5*time.Second,
		dw.processBatch,
	)
	return dw
}

func (dw *DecayWorker) processBatch(ctx context.Context, batch []DecayCandidate) error {
	now := time.Now()
	for _, c := range batch {
		daysSince := now.Sub(c.LastAccess).Hours() / 24.0
		newRelevance := EbbinghausWithFloor(daysSince, float64(c.Stability), DefaultFloor)

		// Average spacing = total lifespan / number of accesses.
		// Using lifespan (now - CreatedAt) divided by AccessCount gives the true
		// average interval between reviews, which drives the spacing-effect bonus.
		// The old formula used daysSince/(AccessCount+1) — time since last access
		// divided by count — which severely underestimated spacing for old engrams
		// accessed recently but rarely (e.g., 1-day daysSince / 3 accesses = 0.33d
		// instead of the correct 365d/3 = 121d for an engram created a year ago).
		lifespanDays := now.Sub(c.CreatedAt).Hours() / 24.0
		if lifespanDays < 0 { // clock skew: CreatedAt in the future
			lifespanDays = 0
		}
		divisor := float64(c.AccessCount)
		if divisor < 1 {
			divisor = 1
		}
		avgSpacing := lifespanDays / divisor
		newStability := ComputeStability(c.AccessCount, avgSpacing)

		// Capture oldRelevance before the update (carried from the activation path).
		oldRelevance := c.Relevance

		// Pass the vault prefix (c.WS) so the update targets the correct key space.
		if err := dw.store.UpdateRelevance(ctx, c.WS, c.ID, float32(newRelevance), float32(newStability)); err != nil {
			continue
		}
		if dw.OnDecayUpdate != nil {
			dw.OnDecayUpdate(c.WS, [16]byte(c.ID), "relevance", float64(oldRelevance), float64(newRelevance))
		}
	}
	return nil
}
