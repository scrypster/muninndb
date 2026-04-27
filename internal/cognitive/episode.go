package cognitive

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

const (
	episodePassInterval = 5 * time.Second
	episodeBufSize      = 2000
	episodeBatchSize    = 50
)

// EpisodeEvent is submitted to the EpisodeWorker when an engram is written.
type EpisodeEvent struct {
	WS        [8]byte
	EngramID  [16]byte
	Embedding []float32 // nil if no embedding available
	At        time.Time
}

// EpisodeStore is the storage interface for writing same_episode associations.
type EpisodeStore interface {
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, src, dst storage.ULID, assoc *storage.Association) error
}

// vaultEpisodeState tracks the last engram seen per vault for boundary detection.
type vaultEpisodeState struct {
	lastID        [16]byte
	lastEmbedding []float32
	lastAt        time.Time
}

// EpisodeWorker detects episode boundaries between consecutive writes
// via cosine similarity and creates same_episode associations for engrams
// within the same episode.
type EpisodeWorker struct {
	*Worker[EpisodeEvent]
	store  EpisodeStore
	config EpisodeConfig

	// Per-vault state for detecting boundaries. Key is [8]byte vault workspace.
	vaultState sync.Map // map[[8]byte]*vaultEpisodeState
}

// NewEpisodeWorker creates a new episode segmentation worker.
func NewEpisodeWorker(store EpisodeStore, config EpisodeConfig) *EpisodeWorker {
	ew := &EpisodeWorker{
		store:  store,
		config: config,
	}
	ew.Worker = NewWorker[EpisodeEvent](
		episodeBufSize, episodeBatchSize, episodePassInterval,
		ew.processBatch,
	)
	return ew
}

func (ew *EpisodeWorker) processBatch(ctx context.Context, batch []EpisodeEvent) error {
	// Group events by vault to preserve per-vault ordering.
	type vaultBatch struct {
		ws     [8]byte
		events []EpisodeEvent
	}
	ordered := make(map[[8]byte]*vaultBatch)
	var vaultOrder [][8]byte

	for _, ev := range batch {
		vb, ok := ordered[ev.WS]
		if !ok {
			vb = &vaultBatch{ws: ev.WS}
			ordered[ev.WS] = vb
			vaultOrder = append(vaultOrder, ev.WS)
		}
		vb.events = append(vb.events, ev)
	}

	for _, ws := range vaultOrder {
		vb := ordered[ws]
		ew.processVaultBatch(ctx, vb.ws, vb.events)
	}

	return nil
}

func (ew *EpisodeWorker) processVaultBatch(ctx context.Context, ws [8]byte, events []EpisodeEvent) {
	// Load or create per-vault state.
	stateVal, _ := ew.vaultState.LoadOrStore(ws, &vaultEpisodeState{})
	state := stateVal.(*vaultEpisodeState)

	for _, ev := range events {
		// Skip events with nil embeddings — we cannot compute similarity.
		if ev.Embedding == nil {
			continue
		}

		if state.lastEmbedding != nil {
			// Check time gap boundary.
			timeGap := ev.At.Sub(state.lastAt)
			isBoundary := timeGap >= ew.config.TimeGap

			// Check cosine similarity boundary.
			if !isBoundary {
				sim := cosineSimilarity(state.lastEmbedding, ev.Embedding)
				isBoundary = sim < ew.config.SimilarityThreshold
			}

			// If NOT a boundary, link the two engrams as same episode.
			if !isBoundary {
				assoc := &storage.Association{
					TargetID:  storage.ULID(ev.EngramID),
					RelType:   storage.RelSameEpisode,
					Weight:    ew.config.AssociationWeight,
					Confidence: 1.0,
					CreatedAt: ev.At,
				}
				if err := ew.store.WriteAssociation(ctx, ws,
					storage.ULID(state.lastID),
					storage.ULID(ev.EngramID),
					assoc,
				); err != nil {
					slog.Error("episode: failed to write same_episode association",
						"ws", fmt.Sprintf("%x", ws),
						"src", fmt.Sprintf("%x", state.lastID),
						"dst", fmt.Sprintf("%x", ev.EngramID),
						"error", err)
				}
			}
		}

		// Update state for next comparison. Defensive copy of embedding
		// since the slice may be reused after async Submit returns.
		state.lastID = ev.EngramID
		state.lastEmbedding = append([]float32(nil), ev.Embedding...)
		state.lastAt = ev.At
	}
}

// cosineSimilarity computes the cosine similarity between two vectors.
// Returns 0.0 if either vector is zero-length or the vectors differ in dimension.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}

	var dot, normA, normB float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom < 1e-12 {
		return 0.0
	}
	return dot / denom
}
