package engine

import (
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// EngineConfig holds all constructor parameters for Engine.
// All pointer fields except Store are optional (nil-safe) unless documented otherwise.
type EngineConfig struct {
	Store            *storage.PebbleStore
	AuthStore        *auth.Store // nil → use plasticity defaults
	FTSIndex         *fts.Index  // nil → full-text search disabled
	ActivationEngine *activation.ActivationEngine
	TriggerSystem    *trigger.TriggerSystem
	HebbianWorker    *cognitive.HebbianWorker                      // nil → no Hebbian learning
	ContradictWorker *cognitive.Worker[cognitive.ContradictItem]   // nil → no contradiction detection
	ConfidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate] // nil → no confidence decay
	Embedder         activation.Embedder                           // nil → no semantic search
	HNSWRegistry     *hnsw.Registry                                // nil → no HNSW indexes

	// EvolveRepairDelay overrides the startup delay before the one-shot evolve
	// entity-link repair pass (#622). nil → 60s plus jitter, matching the
	// prune worker. Tests set a small value to exercise the pass promptly.
	EvolveRepairDelay *time.Duration
}
