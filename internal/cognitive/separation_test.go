package cognitive

import (
	"context"
	"math"
	"testing"
)

// mockSeparationStore is a test double that returns pre-configured entity lists.
type mockSeparationStore struct {
	entities map[[16]byte][]string
}

func (m *mockSeparationStore) GetEngramEntities(_ context.Context, _ [8]byte, engramID [16]byte) ([]string, error) {
	return m.entities[engramID], nil
}

func id(n byte) [16]byte {
	var b [16]byte
	b[15] = n
	return b
}

// TestJaccardSimilarity verifies the Jaccard math for various set configurations.
func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		name   string
		setA   map[string]struct{}
		sliceB []string
		want   float64
	}{
		{
			name:   "both empty",
			setA:   map[string]struct{}{},
			sliceB: nil,
			want:   0.0,
		},
		{
			name:   "identical sets",
			setA:   map[string]struct{}{"auth": {}, "migration": {}},
			sliceB: []string{"auth", "migration"},
			want:   1.0,
		},
		{
			name:   "no overlap",
			setA:   map[string]struct{}{"auth": {}, "migration": {}},
			sliceB: []string{"billing", "payment"},
			want:   0.0,
		},
		{
			name:   "partial overlap",
			setA:   map[string]struct{}{"auth": {}, "migration": {}},
			sliceB: []string{"auth", "billing"},
			want:   1.0 / 3.0, // intersection=1 (auth), union=3 (auth,migration,billing)
		},
		{
			name:   "A empty B non-empty",
			setA:   map[string]struct{}{},
			sliceB: []string{"auth"},
			want:   0.0,
		},
		{
			name:   "A non-empty B empty",
			setA:   map[string]struct{}{"auth": {}},
			sliceB: nil,
			want:   0.0,
		},
		{
			name:   "B has duplicates",
			setA:   map[string]struct{}{"auth": {}, "migration": {}},
			sliceB: []string{"auth", "auth", "migration"},
			want:   1.0, // duplicates collapsed → identical sets
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := JaccardSimilarity(tt.setA, tt.sliceB)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("JaccardSimilarity = %f, want %f", got, tt.want)
			}
		})
	}
}

// TestSeparationScoring verifies that cross-context candidates get penalised.
func TestSeparationScoring(t *testing.T) {
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): {"auth", "migration"},       // same context as query
			id(2): {"billing", "payment"},       // different context
			id(3): {"auth", "billing"},           // partial overlap
			id(4): {},                            // no entities at all
		},
	}

	scorer := NewSeparationScorer(store, SeparationConfig{
		RepulsionAlpha:    0.3,
		ContextMismatchFn: "entity",
	})

	queryEntities := []string{"auth", "migration"}
	candidates := [][16]byte{id(1), id(2), id(3), id(4)}
	ws := [8]byte{}

	mults, err := scorer.ScoreSeparation(context.Background(), ws, queryEntities, candidates)
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}

	// id(1): jaccard=1.0, mult = 1.0 - 0.3*(1-1) = 1.0
	if math.Abs(mults[0]-1.0) > 1e-9 {
		t.Errorf("same context multiplier = %f, want 1.0", mults[0])
	}

	// id(2): jaccard=0.0, mult = 1.0 - 0.3*(1-0) = 0.7
	if math.Abs(mults[1]-0.7) > 1e-9 {
		t.Errorf("different context multiplier = %f, want 0.7", mults[1])
	}

	// id(3): jaccard = 1/3, mult = 1.0 - 0.3*(1-1/3) = 1.0 - 0.2 = 0.8
	expected3 := 1.0 - 0.3*(1.0-1.0/3.0)
	if math.Abs(mults[2]-expected3) > 1e-9 {
		t.Errorf("partial overlap multiplier = %f, want %f", mults[2], expected3)
	}

	// id(4): jaccard=0.0 (empty candidate vs non-empty query), mult = 0.7
	if math.Abs(mults[3]-0.7) > 1e-9 {
		t.Errorf("no-entity candidate multiplier = %f, want 0.7", mults[3])
	}
}

// TestSameContextNoPenalty verifies that candidates sharing exact entity context
// with the query receive multiplier 1.0 (no penalty).
func TestSameContextNoPenalty(t *testing.T) {
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): {"project-X", "auth", "kubernetes"},
			id(2): {"project-X", "auth", "kubernetes"},
		},
	}

	scorer := NewSeparationScorer(store, SeparationConfig{
		RepulsionAlpha:    0.5, // aggressive alpha
		ContextMismatchFn: "entity",
	})

	queryEntities := []string{"project-X", "auth", "kubernetes"}
	candidates := [][16]byte{id(1), id(2)}
	ws := [8]byte{}

	mults, err := scorer.ScoreSeparation(context.Background(), ws, queryEntities, candidates)
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}

	for i, m := range mults {
		if math.Abs(m-1.0) > 1e-9 {
			t.Errorf("candidate %d: multiplier = %f, want 1.0 (same context)", i, m)
		}
	}
}

// TestSeparationDisabled verifies that nil scorer is a no-op (caller must
// check for nil before calling). This test documents the expected integration
// pattern: when separation is disabled, the engine skips the scoring step.
func TestSeparationDisabled(t *testing.T) {
	// The integration contract is: if scorer == nil, skip separation.
	// This test verifies that a scorer with empty query entities produces no penalty,
	// which is the fallback behaviour when entity context is unavailable.
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): {"billing"},
		},
	}

	scorer := NewSeparationScorer(store, DefaultSeparationConfig())

	// Empty query entities → all multipliers should be 1.0.
	mults, err := scorer.ScoreSeparation(context.Background(), [8]byte{}, nil, [][16]byte{id(1)})
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}
	if len(mults) != 1 {
		t.Fatalf("expected 1 multiplier, got %d", len(mults))
	}
	if math.Abs(mults[0]-1.0) > 1e-9 {
		t.Errorf("empty query entities multiplier = %f, want 1.0", mults[0])
	}
}

// TestSeparationInvalidAlphaDefault verifies that out-of-range alpha defaults to 0.3.
func TestSeparationInvalidAlphaDefault(t *testing.T) {
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): {}, // no entities → jaccard=0.0
		},
	}

	// Alpha out of valid range → defaults to 0.3.
	scorer := NewSeparationScorer(store, SeparationConfig{
		RepulsionAlpha:    1.5, // invalid → clamped to 0.3 by scorer
		ContextMismatchFn: "entity",
	})

	mults, err := scorer.ScoreSeparation(context.Background(), [8]byte{}, []string{"auth"}, [][16]byte{id(1)})
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}

	// With alpha defaulted to 0.3: mult = 1.0 - 0.3*1.0 = 0.7
	if math.Abs(mults[0]-0.7) > 1e-9 {
		t.Errorf("clamped alpha multiplier = %f, want 0.7", mults[0])
	}
}

// TestSeparationClampFloor verifies the 0.1 floor clamp with high alpha and zero overlap.
func TestSeparationClampFloor(t *testing.T) {
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): {}, // no entities → jaccard=0.0
		},
	}

	// alpha=0.99, jaccard=0.0 → raw mult = 1.0 - 0.99*(1.0-0.0) = 0.01, clamped to 0.1
	scorer := NewSeparationScorer(store, SeparationConfig{
		RepulsionAlpha:    0.99,
		ContextMismatchFn: "entity",
	})

	mults, err := scorer.ScoreSeparation(context.Background(), [8]byte{}, []string{"auth"}, [][16]byte{id(1)})
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}

	if math.Abs(mults[0]-0.1) > 1e-9 {
		t.Errorf("floor clamp multiplier = %f, want 0.1", mults[0])
	}
}
