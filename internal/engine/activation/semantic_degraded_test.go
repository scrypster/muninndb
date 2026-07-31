package activation

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// warnCaptureHandler is a minimal slog.Handler that records every record's
// level/message/attrs so a test can assert a specific WARN was actually
// logged, not merely that behavior happened to change.
type warnCaptureHandler struct {
	mu      sync.Mutex
	records []string
}

func (h *warnCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *warnCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var sb strings.Builder
	sb.WriteString(r.Level.String())
	sb.WriteString(": ")
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})
	h.records = append(h.records, sb.String())
	return nil
}

func (h *warnCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCaptureHandler) WithGroup(string) slog.Handler      { return h }

func (h *warnCaptureHandler) contains(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rec := range h.records {
		if strings.Contains(rec, substr) {
			return true
		}
	}
	return false
}

// erroringEmbeddingsStore wraps internalStubStore and forces GetEmbeddings to
// fail, modeling a storage hiccup on the phase6 post-load cosine fallback
// (e.g. the batched 0x18-key read timing out or hitting a corrupt block).
type erroringEmbeddingsStore struct {
	*internalStubStore
	err error
}

func (s *erroringEmbeddingsStore) GetEmbeddings(_ context.Context, _ [8]byte, ids []storage.ULID) ([][]float32, error) {
	return nil, s.err
}

// TestPhase6Score_FallbackGetEmbeddingsError_SemanticDegraded is the RED-first
// repro for the "degrade silently" bug: when the phase6 post-load cosine
// fallback's GetEmbeddings call errors, candidates silently stayed at
// vectorScore=0 with NO warning and no signal anywhere in the result that
// semantic recall degraded.
//
// Before the fix: no SemanticDegraded field exists on ActivateResult at all
// (this test fails to compile / fails the assertion), and no WARN is logged
// for the fallback error -- the error is swallowed by `if ... err == nil`
// with no else branch. After the fix: phase6Score logs a WARN naming the
// vault, candidate count, and error, and result.SemanticDegraded is true.
//
// SemanticSimilarity must still be 0 (safe default, no crash) -- the fix is
// entirely about the loud signal, not changing the score value.
func TestPhase6Score_FallbackGetEmbeddingsError_SemanticDegraded(t *testing.T) {
	base := newInternalStubStore()
	store := &erroringEmbeddingsStore{internalStubStore: base, err: context.DeadlineExceeded}

	handler := &warnCaptureHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	e := newTestActivationEngine(store)
	defer e.Close()

	eng := &storage.Engram{
		Concept: "WidgetPipeline lifecycle", Content: "WidgetPipeline lifecycle state machine",
		Confidence: 1.0, Stability: 30.0, State: storage.StateActive,
	}
	// Embedding reachable ONLY via GetEmbeddings in production (ERF v2 split);
	// here GetEmbeddings is wired to always fail instead.
	store.addEngramSeparateEmbedding(eng, []float32{1, 0, 0})

	fused := []fusedCandidate{{id: eng.ID, rrfScore: 0.5, ftsScore: 1.0}}
	p1 := &phase1Result{
		queryStr:  "WidgetPipeline lifecycle state machine",
		embedding: []float32{1, 0, 0},
	}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		VaultID:    7,
		MaxResults: 10,
		Threshold:  0.01,
	}, [8]byte{}, fused, nil, p1)
	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}

	var found bool
	for _, a := range result.Activations {
		if a.Engram.ID == eng.ID {
			found = true
			if a.Components.SemanticSimilarity != 0 {
				t.Errorf("SemanticSimilarity = %v, want exactly 0 -- a failed fallback read must never "+
					"fabricate a score, only degrade loudly", a.Components.SemanticSimilarity)
			}
		}
	}
	if !found {
		t.Fatalf("FTS-only candidate did not survive to the final result set")
	}

	if !result.SemanticDegraded {
		t.Errorf("ActivateResult.SemanticDegraded = false, want true -- the phase6 post-load cosine " +
			"fallback failed (GetEmbeddings returned an error) and that must surface as a loud signal, " +
			"not silently leave vectorScore=0 with no trace")
	}

	if !handler.contains("phase6 post-load cosine fallback failed") {
		t.Errorf("expected a WARN log naming the phase6 fallback failure; got records: %v", handler.records)
	}
	if !handler.contains("vault=7") {
		t.Errorf("expected the WARN to include the vault ID; got records: %v", handler.records)
	}
}

// TestPhase1_ZeroVectorEmbed_SemanticDegraded is the RED-first repro for the
// second silent-degradation case: an embedder call that returns err == nil
// but an empty/all-zero vector for a non-trivial query. A normalized
// embedder (bge-small, L2-normed) never legitimately produces the zero
// vector for real text, so this shape must be treated as a degradation, not
// a valid (if uninformative) embedding.
func TestPhase1_ZeroVectorEmbed_SemanticDegraded(t *testing.T) {
	handler := &warnCaptureHandler{}
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(oldLogger)

	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()
	e.embedder = &zeroVectorEmbedder{}
	e.hnsw = &nilAwareHNSW{}

	p1, err := e.phase1(context.Background(), &ActivateRequest{
		VaultID: 9,
		Context: []string{"a real, non-trivial query"},
	})
	if err != nil {
		t.Fatalf("phase1: %v", err)
	}

	if !p1.semanticDegraded {
		t.Errorf("phase1Result.semanticDegraded = false, want true -- embedder returned an all-zero " +
			"vector with err==nil for a non-trivial query, which is a silent degradation, not a legitimate embedding")
	}
	if !handler.contains("embed backend returned empty/zero vector") {
		t.Errorf("expected a WARN log naming the zero-vector degradation; got records: %v", handler.records)
	}
}

// zeroVectorEmbedder always succeeds but returns an all-zero vector,
// simulating a broken/misconfigured embed backend that responds without
// erroring (e.g. a proxy returning a zeroed placeholder).
type zeroVectorEmbedder struct{}

func (zeroVectorEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	return []float32{0, 0, 0}, nil
}
func (zeroVectorEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

// nilAwareHNSW is a minimal HNSWIndex stub -- phase1 only computes an
// embedding at all when e.hnsw != nil, so a non-nil (if unused here) index is
// required to exercise the embed call path.
type nilAwareHNSW struct{}

func (nilAwareHNSW) Search(_ context.Context, _ [8]byte, _ []float32, _ int) ([]ScoredID, error) {
	return nil, nil
}
