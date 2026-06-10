package brief

import (
	"context"
	"math"
	"testing"
)

// mockEmbeddingModel returns predictable embeddings for testing.
type mockEmbeddingModel struct {
	responses map[string][]float32
	dim       int
}

func (m *mockEmbeddingModel) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := m.responses[t]; ok {
			results[i] = v
		} else {
			results[i] = make([]float32, m.dim) // zero vector
		}
	}
	return results, nil
}

func (m *mockEmbeddingModel) Dim() int { return m.dim }

// TestSplitSentences verifies sentence splitting on period, question, exclamation boundaries.
func TestSplitSentences(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		maxLen      int
		expectedLen int
		expectedIn  []string
	}{
		{
			name:        "single sentence with period",
			text:        "Hello world. This is a test.",
			maxLen:      0,
			expectedLen: 2,
			expectedIn:  []string{"Hello world.", "This is a test."},
		},
		{
			name:        "question and exclamation",
			text:        "What is this? Great! That works.",
			maxLen:      0,
			expectedLen: 3,
			expectedIn:  []string{"What is this?", "Great!", "That works."},
		},
		{
			name:        "period followed by space",
			text:        "First. Second.",
			maxLen:      0,
			expectedLen: 2,
			expectedIn:  []string{"First.", "Second."},
		},
		{
			name:        "text without punctuation",
			text:        "No punctuation here",
			maxLen:      0,
			expectedLen: 1,
			expectedIn:  []string{"No punctuation here"},
		},
		{
			name:        "empty string",
			text:        "",
			maxLen:      0,
			expectedLen: 0,
			expectedIn:  []string{},
		},
		{
			name:        "truncation at word boundary",
			text:        "This is a long sentence that needs to be truncated. Next sentence.",
			maxLen:      20,
			expectedLen: 2,
			expectedIn:  []string{"This is a long."}, // should truncate to word boundary
		},
		{
			name:        "no truncation when maxLen is 0",
			text:        "This is a test. Another test.",
			maxLen:      0,
			expectedLen: 2,
			expectedIn:  []string{"This is a test.", "Another test."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Split(tt.text, tt.maxLen)
			if len(result) != tt.expectedLen {
				t.Errorf("expected %d sentences, got %d: %v", tt.expectedLen, len(result), result)
			}
			for _, expected := range tt.expectedIn {
				found := false
				for _, sentence := range result {
					if sentence == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected to find sentence %q, got %v", expected, result)
				}
			}
		})
	}
}

// TestCosineSimilarity verifies basic cosine similarity computation.
func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		a        []float32
		b        []float32
		expected float32
		delta    float32
	}{
		{
			name:     "identical vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{1, 0, 0},
			expected: 1.0,
			delta:    0.0001,
		},
		{
			name:     "orthogonal vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{0, 1, 0},
			expected: 0.0,
			delta:    0.0001,
		},
		{
			name:     "opposite vectors",
			a:        []float32{1, 0, 0},
			b:        []float32{-1, 0, 0},
			expected: -1.0,
			delta:    0.0001,
		},
		{
			name:     "zero vector",
			a:        []float32{0, 0, 0},
			b:        []float32{1, 0, 0},
			expected: 0.0,
			delta:    0.0001,
		},
		{
			name:     "45 degree angle (sqrt(2)/2)",
			a:        []float32{1, 1},
			b:        []float32{1, 0},
			expected: 1.0 / float32(math.Sqrt(2)),
			delta:    0.0001,
		},
		{
			name:     "mismatched lengths returns 0",
			a:        []float32{1, 2, 3},
			b:        []float32{1, 2},
			expected: 0.0,
			delta:    0.0001,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cosineSimilarity(tt.a, tt.b)
			if math.Abs(float64(result-tt.expected)) > float64(tt.delta) {
				t.Errorf("expected %f, got %f", tt.expected, result)
			}
		})
	}
}

// TestScorer_WithMockModel verifies scoring with a mock embedding model.
func TestScorer_WithMockModel(t *testing.T) {
	model := &mockEmbeddingModel{
		dim: 3,
		responses: map[string][]float32{
			"Sentence one about memory.": {1, 0, 0},
			"Sentence two about data.":   {0, 1, 0},
			"Sentence three about time.": {0.7, 0.7, 0},
		},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.5,
		MaxSentences: 2,
		MaxSentLen:   512,
	}

	content := "Sentence one about memory. Sentence two about data. Sentence three about time."
	contextEmbedding := []float32{1, 0, 0} // closer to first sentence

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) == 0 {
		t.Fatalf("expected non-empty result")
	}

	if len(result) > scorer.MaxSentences {
		t.Errorf("expected at most %d sentences, got %d", scorer.MaxSentences, len(result))
	}

	// First result should be highest scoring
	if result[0].Score <= 0 {
		t.Errorf("expected positive score for first result, got %f", result[0].Score)
	}
}

// TestScorer_FallbackWhenNoModel verifies fallback behavior when model is nil.
func TestScorer_FallbackWhenNoModel(t *testing.T) {
	scorer := &Scorer{
		Model:        nil,
		Threshold:    0.72,
		MaxSentences: 2,
		MaxSentLen:   512,
	}

	content := "First sentence. Second sentence. Third sentence."
	contextEmbedding := []float32{1, 0, 0}

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) == 0 {
		t.Fatalf("expected non-empty result")
	}

	if len(result) != scorer.MaxSentences {
		t.Errorf("expected %d sentences, got %d", scorer.MaxSentences, len(result))
	}

	// Fallback should return sentences in order with zero scores
	if result[0].Text != "First sentence." {
		t.Errorf("expected first sentence to be 'First sentence.', got %q", result[0].Text)
	}
	if result[0].Score != 0 {
		t.Errorf("expected fallback score to be 0, got %f", result[0].Score)
	}
}

// TestScorer_FallbackOnError verifies fallback when embedding fails.
func TestScorer_FallbackOnError(t *testing.T) {
	// Create a model that returns mismatched dimensions to trigger dimension mismatch
	model := &mockEmbeddingModel{
		dim: 3,
		responses: map[string][]float32{
			"First sentence": {1, 0}, // wrong dimension
		},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.72,
		MaxSentences: 2,
		MaxSentLen:   512,
	}

	content := "First sentence. Second sentence."
	contextEmbedding := []float32{1, 0, 0}

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With dimension mismatch, should still return fallback
	if len(result) == 0 {
		t.Fatalf("expected non-empty result on fallback")
	}
}

// TestScorer_EmptyContent verifies behavior with empty content.
func TestScorer_EmptyContent(t *testing.T) {
	model := &mockEmbeddingModel{
		dim:       3,
		responses: map[string][]float32{},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.72,
		MaxSentences: 3,
		MaxSentLen:   512,
	}

	content := ""
	contextEmbedding := []float32{1, 0, 0}

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != nil {
		t.Errorf("expected nil result for empty content, got %v", result)
	}
}

// TestScorer_NoContextEmbedding verifies fallback when context embedding is empty.
func TestScorer_NoContextEmbedding(t *testing.T) {
	model := &mockEmbeddingModel{
		dim:       3,
		responses: map[string][]float32{},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.72,
		MaxSentences: 2,
		MaxSentLen:   512,
	}

	content := "First sentence. Second sentence. Third sentence."
	contextEmbedding := []float32{} // empty

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != scorer.MaxSentences {
		t.Errorf("expected %d sentences, got %d", scorer.MaxSentences, len(result))
	}

	// Should return first N sentences with zero score
	if result[0].Score != 0 {
		t.Errorf("expected fallback score to be 0, got %f", result[0].Score)
	}
}

// TestScoredSentence_TokenCount verifies token count computation.
func TestScoredSentence_TokenCount(t *testing.T) {
	model := &mockEmbeddingModel{
		dim: 1,
		responses: map[string][]float32{
			"One":           {1},
			"One two three": {1},
		},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.0, // Accept all
		MaxSentences: 10,
		MaxSentLen:   512,
	}

	content := "One. One two three."
	contextEmbedding := []float32{1}

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) < 2 {
		t.Fatalf("expected at least 2 sentences")
	}

	if result[0].TokenCount != 1 {
		t.Errorf("expected token count 1, got %d", result[0].TokenCount)
	}

	if result[1].TokenCount != 3 {
		t.Errorf("expected token count 3, got %d", result[1].TokenCount)
	}
}

// TestScorer_ThresholdFiltering verifies that sentences below threshold are excluded or returns fallback.
func TestScorer_ThresholdFiltering(t *testing.T) {
	model := &mockEmbeddingModel{
		dim: 3,
		responses: map[string][]float32{
			"Very similar text.":     {1, 0, 0},
			"Somewhat different.":    {0.5, 0.5, 0},
			"Completely orthogonal.": {0, 1, 0},
			"Opposite direction.":    {-1, 0, 0},
		},
	}

	scorer := &Scorer{
		Model:        model,
		Threshold:    0.7, // high threshold
		MaxSentences: 10,
		MaxSentLen:   512,
	}

	content := "Very similar text. Somewhat different. Completely orthogonal. Opposite direction."
	contextEmbedding := []float32{1, 0, 0}

	result, err := scorer.Score(context.Background(), content, contextEmbedding)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return top-N even if below threshold (when no sentences meet threshold)
	if len(result) == 0 {
		t.Fatalf("expected non-empty result (fallback to top-N)")
	}

	// All results should be sorted by descending score
	for i := 1; i < len(result); i++ {
		if result[i].Score > result[i-1].Score {
			t.Errorf("results not sorted descending at index %d", i)
		}
	}
}
