package cognitive

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// mockEpisodeStore records WriteAssociation calls for test assertions.
type mockEpisodeStore struct {
	mu     sync.Mutex
	assocs []writtenAssoc
}

type writtenAssoc struct {
	ws  [8]byte
	src storage.ULID
	dst storage.ULID
	rel storage.RelType
	w   float32
}

func (m *mockEpisodeStore) WriteAssociation(_ context.Context, ws [8]byte, src, dst storage.ULID, assoc *storage.Association) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assocs = append(m.assocs, writtenAssoc{
		ws:  ws,
		src: src,
		dst: dst,
		rel: assoc.RelType,
		w:   assoc.Weight,
	})
	return nil
}

func (m *mockEpisodeStore) getAssocs() []writtenAssoc {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]writtenAssoc, len(m.assocs))
	copy(out, m.assocs)
	return out
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
		tol  float64
	}{
		{
			name: "identical vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{1, 0, 0},
			want: 1.0,
			tol:  1e-9,
		},
		{
			name: "orthogonal vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{0, 1, 0},
			want: 0.0,
			tol:  1e-9,
		},
		{
			name: "opposite vectors",
			a:    []float32{1, 0},
			b:    []float32{-1, 0},
			want: -1.0,
			tol:  1e-9,
		},
		{
			name: "45 degree angle",
			a:    []float32{1, 0},
			b:    []float32{1, 1},
			want: 1.0 / math.Sqrt(2),
			tol:  1e-6,
		},
		{
			name: "zero vector a",
			a:    []float32{0, 0, 0},
			b:    []float32{1, 2, 3},
			want: 0.0,
			tol:  1e-9,
		},
		{
			name: "mismatched dimensions",
			a:    []float32{1, 2},
			b:    []float32{1, 2, 3},
			want: 0.0,
			tol:  1e-9,
		},
		{
			name: "empty vectors",
			a:    []float32{},
			b:    []float32{},
			want: 0.0,
			tol:  1e-9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > tt.tol {
				t.Errorf("cosineSimilarity(%v, %v) = %v, want %v (tol %v)", tt.a, tt.b, got, tt.want, tt.tol)
			}
		})
	}
}

func TestEpisodeBoundaryDetection(t *testing.T) {
	store := &mockEpisodeStore{}
	cfg := DefaultEpisodeConfig()
	cfg.SimilarityThreshold = 0.5

	ew := NewEpisodeWorker(store, cfg)

	ws := [8]byte{0x01}
	now := time.Now()

	// Three events: first two similar (same direction), third dissimilar.
	// Similar embeddings → same episode → association created.
	// Dissimilar → boundary → no association.
	events := []EpisodeEvent{
		{WS: ws, EngramID: [16]byte{1}, Embedding: []float32{1, 0, 0}, At: now},
		{WS: ws, EngramID: [16]byte{2}, Embedding: []float32{0.9, 0.1, 0}, At: now.Add(time.Second)},
		{WS: ws, EngramID: [16]byte{3}, Embedding: []float32{0, 0, 1}, At: now.Add(2 * time.Second)},
	}

	err := ew.processBatch(context.Background(), events)
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	assocs := store.getAssocs()
	// Event 1→2: similar (cos ≈ 0.994), should produce association.
	// Event 2→3: dissimilar (cos ≈ 0.0), should NOT produce association.
	if len(assocs) != 1 {
		t.Fatalf("expected 1 association, got %d", len(assocs))
	}

	if assocs[0].src != storage.ULID([16]byte{1}) || assocs[0].dst != storage.ULID([16]byte{2}) {
		t.Errorf("expected association between engram 1 and 2, got src=%x dst=%x", assocs[0].src, assocs[0].dst)
	}
	if assocs[0].rel != storage.RelSameEpisode {
		t.Errorf("expected RelSameEpisode, got %v", assocs[0].rel)
	}
}

func TestNoFalseBoundary(t *testing.T) {
	store := &mockEpisodeStore{}
	cfg := DefaultEpisodeConfig()
	cfg.SimilarityThreshold = 0.5

	ew := NewEpisodeWorker(store, cfg)

	ws := [8]byte{0x02}
	now := time.Now()

	// All embeddings are similar → no boundaries → all consecutive pairs linked.
	events := []EpisodeEvent{
		{WS: ws, EngramID: [16]byte{1}, Embedding: []float32{1, 0, 0}, At: now},
		{WS: ws, EngramID: [16]byte{2}, Embedding: []float32{1, 0.1, 0}, At: now.Add(time.Second)},
		{WS: ws, EngramID: [16]byte{3}, Embedding: []float32{1, 0.05, 0.05}, At: now.Add(2 * time.Second)},
	}

	err := ew.processBatch(context.Background(), events)
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	assocs := store.getAssocs()
	if len(assocs) != 2 {
		t.Fatalf("expected 2 associations (all within same episode), got %d", len(assocs))
	}
}

func TestTimeGapBoundary(t *testing.T) {
	store := &mockEpisodeStore{}
	cfg := DefaultEpisodeConfig()
	cfg.SimilarityThreshold = 0.1 // very low, so similarity alone wouldn't trigger boundary
	cfg.TimeGap = 10 * time.Minute

	ew := NewEpisodeWorker(store, cfg)

	ws := [8]byte{0x03}
	now := time.Now()

	// Two similar embeddings but with a large time gap → boundary forced by time.
	events := []EpisodeEvent{
		{WS: ws, EngramID: [16]byte{1}, Embedding: []float32{1, 0, 0}, At: now},
		{WS: ws, EngramID: [16]byte{2}, Embedding: []float32{1, 0, 0}, At: now.Add(15 * time.Minute)},
	}

	err := ew.processBatch(context.Background(), events)
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	assocs := store.getAssocs()
	if len(assocs) != 0 {
		t.Fatalf("expected 0 associations (time gap boundary), got %d", len(assocs))
	}
}

func TestNilEmbeddingSkipped(t *testing.T) {
	store := &mockEpisodeStore{}
	cfg := DefaultEpisodeConfig()
	cfg.SimilarityThreshold = 0.5

	ew := NewEpisodeWorker(store, cfg)

	ws := [8]byte{0x04}
	now := time.Now()

	// Second event has nil embedding → skipped, no comparison possible.
	// Third event should compare to first (the last valid one).
	events := []EpisodeEvent{
		{WS: ws, EngramID: [16]byte{1}, Embedding: []float32{1, 0, 0}, At: now},
		{WS: ws, EngramID: [16]byte{2}, Embedding: nil, At: now.Add(time.Second)},
		{WS: ws, EngramID: [16]byte{3}, Embedding: []float32{1, 0.1, 0}, At: now.Add(2 * time.Second)},
	}

	err := ew.processBatch(context.Background(), events)
	if err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	assocs := store.getAssocs()
	// Event 2 skipped (nil embedding). Event 3 compared to event 1: similar → linked.
	if len(assocs) != 1 {
		t.Fatalf("expected 1 association (nil embedding skipped), got %d", len(assocs))
	}
	if assocs[0].src != storage.ULID([16]byte{1}) || assocs[0].dst != storage.ULID([16]byte{3}) {
		t.Errorf("expected association between engram 1 and 3, got src=%x dst=%x", assocs[0].src, assocs[0].dst)
	}
}
