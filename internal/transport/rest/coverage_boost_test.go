package rest

// coverage_boost_test.go adds targeted tests for code paths that the existing
// test suite leaves uncovered or only partially covered. Each test identifies
// the specific branch or statement being exercised.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// applyRecallModePreset — 0% coverage, pure function
// ---------------------------------------------------------------------------

func TestApplyRecallModePreset_SetsThresholdWhenZero(t *testing.T) {
	req := &ActivateRequest{Threshold: 0}
	preset := auth.RecallModePreset{Threshold: 0.5}
	applyRecallModePreset(req, preset)
	if req.Threshold != 0.5 {
		t.Errorf("expected Threshold=0.5, got %v", req.Threshold)
	}
}

func TestApplyRecallModePreset_DoesNotOverrideNonZeroThreshold(t *testing.T) {
	req := &ActivateRequest{Threshold: 0.8}
	preset := auth.RecallModePreset{Threshold: 0.3}
	applyRecallModePreset(req, preset)
	if req.Threshold != 0.8 {
		t.Errorf("expected Threshold to stay 0.8, got %v", req.Threshold)
	}
}

func TestApplyRecallModePreset_SetsMaxHopsWhenZero(t *testing.T) {
	req := &ActivateRequest{MaxHops: 0}
	preset := auth.RecallModePreset{MaxHops: 4}
	applyRecallModePreset(req, preset)
	if req.MaxHops != 4 {
		t.Errorf("expected MaxHops=4, got %v", req.MaxHops)
	}
}

func TestApplyRecallModePreset_DoesNotOverrideNonZeroMaxHops(t *testing.T) {
	req := &ActivateRequest{MaxHops: 2}
	preset := auth.RecallModePreset{MaxHops: 5}
	applyRecallModePreset(req, preset)
	if req.MaxHops != 2 {
		t.Errorf("expected MaxHops to stay 2, got %v", req.MaxHops)
	}
}

func TestApplyRecallModePreset_SetsWeightsWhenNil(t *testing.T) {
	req := &ActivateRequest{Weights: nil}
	preset := auth.RecallModePreset{SemanticSimilarity: 0.8, FullTextRelevance: 0.2}
	applyRecallModePreset(req, preset)
	if req.Weights == nil {
		t.Fatal("expected Weights to be non-nil after applying preset")
	}
	if req.Weights.SemanticSimilarity != 0.8 {
		t.Errorf("expected SemanticSimilarity=0.8, got %v", req.Weights.SemanticSimilarity)
	}
	if req.Weights.FullTextRelevance != 0.2 {
		t.Errorf("expected FullTextRelevance=0.2, got %v", req.Weights.FullTextRelevance)
	}
}

func TestApplyRecallModePreset_DoesNotOverrideExistingWeights(t *testing.T) {
	existing := &mbp.Weights{SemanticSimilarity: 0.9}
	req := &ActivateRequest{Weights: existing}
	preset := auth.RecallModePreset{SemanticSimilarity: 0.5, FullTextRelevance: 0.4}
	applyRecallModePreset(req, preset)
	// SemanticSimilarity was already non-zero so it must not be overridden.
	if req.Weights.SemanticSimilarity != 0.9 {
		t.Errorf("expected SemanticSimilarity to stay 0.9, got %v", req.Weights.SemanticSimilarity)
	}
	// FullTextRelevance was zero so it should be set.
	if req.Weights.FullTextRelevance != 0.4 {
		t.Errorf("expected FullTextRelevance=0.4, got %v", req.Weights.FullTextRelevance)
	}
}

func TestApplyRecallModePreset_SetsRecency(t *testing.T) {
	req := &ActivateRequest{}
	preset := auth.RecallModePreset{Recency: 0.7}
	applyRecallModePreset(req, preset)
	if req.Weights == nil {
		t.Fatal("expected Weights non-nil")
	}
	if req.Weights.Recency != 0.7 {
		t.Errorf("expected Recency=0.7, got %v", req.Weights.Recency)
	}
}

func TestApplyRecallModePreset_SetsDisableACTR(t *testing.T) {
	req := &ActivateRequest{}
	preset := auth.RecallModePreset{DisableACTR: true}
	applyRecallModePreset(req, preset)
	if req.Weights == nil {
		t.Fatal("expected Weights non-nil")
	}
	if !req.Weights.DisableACTR {
		t.Error("expected DisableACTR to be true")
	}
}

func TestApplyRecallModePreset_ZeroPresetIsNoop(t *testing.T) {
	req := &ActivateRequest{Threshold: 0.4, MaxHops: 2}
	applyRecallModePreset(req, auth.RecallModePreset{}) // all zero values
	if req.Threshold != 0.4 {
		t.Errorf("expected Threshold unchanged at 0.4, got %v", req.Threshold)
	}
	if req.MaxHops != 2 {
		t.Errorf("expected MaxHops unchanged at 2, got %v", req.MaxHops)
	}
	if req.Weights != nil {
		t.Error("expected Weights to remain nil for zero preset")
	}
}

// ---------------------------------------------------------------------------
// handleActivate with mode preset — exercises applyRecallModePreset via HTTP
// ---------------------------------------------------------------------------

func TestActivate_WithSemanticMode(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default","mode":"semantic"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_WithRecentMode(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default","mode":"recent"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_WithBalancedMode(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default","mode":"balanced"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_WithDeepMode(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default","mode":"deep"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_InvalidMode(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default","mode":"nonexistent"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown mode, got %d: %s", w.Code, w.Body.String())
	}
}

func TestActivate_EngineError(t *testing.T) {
	eng := &activateErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["test"],"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d", w.Code)
	}
}

// activateErrorEngine returns an error from Activate.
type activateErrorEngine struct{ MockEngine }

func (e *activateErrorEngine) Activate(_ context.Context, _ *ActivateRequest) (*ActivateResponse, error) {
	return nil, errors.New("engine failure")
}

// ---------------------------------------------------------------------------
// handleGetEngram — error paths
// ---------------------------------------------------------------------------

func TestGetEngram_EngineError_Boost(t *testing.T) {
	eng := &readErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/some-id", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when Read fails, got %d: %s", w.Code, w.Body.String())
	}
}

// readErrorEngine returns errors from Read.
type readErrorEngine struct{ MockEngine }

func (e *readErrorEngine) Read(_ context.Context, _ *ReadRequest) (*ReadResponse, error) {
	return nil, engine.ErrEngramNotFound
}

// ---------------------------------------------------------------------------
// handleDeleteEngram — error path
// ---------------------------------------------------------------------------

func TestDeleteEngram_EngineError(t *testing.T) {
	eng := &forgetErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("DELETE", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when Forget fails, got %d", w.Code)
	}
}

// forgetErrorEngine returns errors from Forget.
type forgetErrorEngine struct{ MockEngine }

func (e *forgetErrorEngine) Forget(_ context.Context, _ *ForgetRequest) (*ForgetResponse, error) {
	return nil, errors.New("storage error")
}

// ---------------------------------------------------------------------------
// handleLink — 22% coverage; many branches untested
// ---------------------------------------------------------------------------

func TestHandleLink_Success(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"source_id":"id-a","target_id":"id-b","rel_type":1,"weight":0.9,"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/link", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp LinkResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Error("expected OK:true")
	}
}

func TestHandleLink_InvalidJSON_Boost(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/link", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleLink_EngramNotFound(t *testing.T) {
	eng := &linkNotFoundEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"source_id":"id-a","target_id":"id-b"}`
	req := httptest.NewRequest("POST", "/api/link", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleLink_EngramSoftDeleted(t *testing.T) {
	eng := &linkSoftDeletedEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"source_id":"id-a","target_id":"id-b"}`
	req := httptest.NewRequest("POST", "/api/link", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleLink_GenericError(t *testing.T) {
	eng := &linkGenericErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"source_id":"id-a","target_id":"id-b"}`
	req := httptest.NewRequest("POST", "/api/link", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// Engines for link error cases.

type linkNotFoundEngine struct{ MockEngine }

func (e *linkNotFoundEngine) Link(_ context.Context, _ *mbp.LinkRequest) (*LinkResponse, error) {
	return nil, fmt.Errorf("link: %w", engine.ErrEngramNotFound)
}

type linkSoftDeletedEngine struct{ MockEngine }

func (e *linkSoftDeletedEngine) Link(_ context.Context, _ *mbp.LinkRequest) (*LinkResponse, error) {
	return nil, fmt.Errorf("link: %w", engine.ErrEngramSoftDeleted)
}

type linkGenericErrorEngine struct{ MockEngine }

func (e *linkGenericErrorEngine) Link(_ context.Context, _ *mbp.LinkRequest) (*LinkResponse, error) {
	return nil, errors.New("generic link error")
}

// ---------------------------------------------------------------------------
// handleStats — error path (60% → better coverage)
// ---------------------------------------------------------------------------

func TestHandleStats_EngineError(t *testing.T) {
	eng := &statErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/stats?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on stat error, got %d", w.Code)
	}
}

// statErrorEngine returns an error from Stat.
type statErrorEngine struct{ MockEngine }

func (e *statErrorEngine) Stat(_ context.Context, _ *StatRequest) (*StatResponse, error) {
	return nil, errors.New("stat failure")
}

// ---------------------------------------------------------------------------
// handleWorkerStats — 0% coverage
// ---------------------------------------------------------------------------

func TestHandleWorkerStats(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/workers", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleHello — error path (44.4%)
// ---------------------------------------------------------------------------

func TestHandleHello_InvalidJSON_Boost(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/hello", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleHello_EngineError(t *testing.T) {
	eng := &helloErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/hello", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

type helloErrorEngine struct{ MockEngine }

func (e *helloErrorEngine) Hello(_ context.Context, _ *HelloRequest) (*HelloResponse, error) {
	return nil, errors.New("auth failed")
}

// ---------------------------------------------------------------------------
// handleCreateEngram — engine error path
// ---------------------------------------------------------------------------

func TestCreateEngram_EngineError(t *testing.T) {
	eng := &writeErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"concept":"test","content":"test content"}`
	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on write error, got %d", w.Code)
	}
}

type writeErrorEngine struct{ MockEngine }

func (e *writeErrorEngine) Write(_ context.Context, _ *WriteRequest) (*WriteResponse, error) {
	return nil, errors.New("write failed")
}

// ---------------------------------------------------------------------------
// handleEvolve — error paths (75%)
// ---------------------------------------------------------------------------

func TestEvolveEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/test-id/evolve", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestEvolveEndpoint_EngineError(t *testing.T) {
	eng := &evolveErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"new_content":"updated","reason":"fix"}`
	req := httptest.NewRequest("POST", "/api/engrams/test-id/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type evolveErrorEngine struct{ MockEngine }

func (e *evolveErrorEngine) Evolve(_ context.Context, _, _, _, _ string) (*EvolveResponse, error) {
	return nil, errors.New("evolve failed")
}

// ---------------------------------------------------------------------------
// handleConsolidateEngrams — error paths (71.4%)
// ---------------------------------------------------------------------------

func TestConsolidateEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestConsolidateEndpoint_TooManyIDs(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	ids := make([]string, 51)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}
	body, _ := json.Marshal(map[string]any{
		"ids":            ids,
		"merged_content": "merged",
	})
	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for >50 IDs, got %d", w.Code)
	}
}

func TestConsolidateEndpoint_MissingMergedContent(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"ids":["id-1","id-2"]}`
	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing merged_content, got %d", w.Code)
	}
}

func TestConsolidateEndpoint_EngineError(t *testing.T) {
	eng := &consolidateErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"ids":["id-1","id-2"],"merged_content":"combined"}`
	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type consolidateErrorEngine struct{ MockEngine }

func (e *consolidateErrorEngine) Consolidate(_ context.Context, _ string, _ []string, _ string) (*ConsolidateResponse, error) {
	return nil, errors.New("consolidate failed")
}

// ---------------------------------------------------------------------------
// handleDecide — error paths (66.7%)
// ---------------------------------------------------------------------------

func TestDecideEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/decide", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestDecideEndpoint_EngineError(t *testing.T) {
	eng := &decideErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"decision":"go with X","rationale":"because Y"}`
	req := httptest.NewRequest("POST", "/api/decide", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type decideErrorEngine struct{ MockEngine }

func (e *decideErrorEngine) Decide(_ context.Context, _, _, _ string, _, _ []string) (*DecideResponse, error) {
	return nil, errors.New("decide failed")
}

// ---------------------------------------------------------------------------
// handleRestore — error paths (55.6%)
// ---------------------------------------------------------------------------

func TestRestoreEndpoint_EngineError(t *testing.T) {
	eng := &restoreErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/missing-id/restore", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type restoreErrorEngine struct{ MockEngine }

func (e *restoreErrorEngine) Restore(_ context.Context, _, _ string) (*RestoreResponse, error) {
	return nil, errors.New("restore failed")
}

// ---------------------------------------------------------------------------
// handleTraverse — error paths (69.6%)
// ---------------------------------------------------------------------------

func TestTraverseEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/traverse", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestTraverseEndpoint_EngineError(t *testing.T) {
	eng := &traverseErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"start_id":"node-1"}`
	req := httptest.NewRequest("POST", "/api/traverse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestTraverseEndpoint_ClampMaxHopsAndNodes(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// max_hops > 5 should be clamped to 5; max_nodes > 100 clamped to 100.
	body := `{"start_id":"node-1","max_hops":99,"max_nodes":999}`
	req := httptest.NewRequest("POST", "/api/traverse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with clamped params, got %d: %s", w.Code, w.Body.String())
	}
}

type traverseErrorEngine struct{ MockEngine }

func (e *traverseErrorEngine) Traverse(_ context.Context, _ string, _ *TraverseRequest) (*TraverseResponse, error) {
	return nil, errors.New("traverse failed")
}

// ---------------------------------------------------------------------------
// handleExplain — error paths (66.7%)
// ---------------------------------------------------------------------------

func TestExplainEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/explain", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestExplainEndpoint_EngineError(t *testing.T) {
	eng := &explainErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"engram_id":"eng-1","query":["test"]}`
	req := httptest.NewRequest("POST", "/api/explain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type explainErrorEngine struct{ MockEngine }

func (e *explainErrorEngine) Explain(_ context.Context, _ string, _ *ExplainRequest) (*ExplainResponse, error) {
	return nil, errors.New("explain failed")
}

// ---------------------------------------------------------------------------
// handleSetState — error paths (78.9%)
// ---------------------------------------------------------------------------

func TestSetStateEndpoint_InvalidJSON(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("PUT", "/api/engrams/test-id/state", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSetStateEndpoint_EngineError(t *testing.T) {
	eng := &setStateErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"state":"active"}`
	req := httptest.NewRequest("PUT", "/api/engrams/test-id/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type setStateErrorEngine struct{ MockEngine }

func (e *setStateErrorEngine) UpdateState(_ context.Context, _, _, _, _ string) error {
	return errors.New("state update failed")
}

// ---------------------------------------------------------------------------
// handleRetryEnrich — error path (77.8%)
// ---------------------------------------------------------------------------

func TestRetryEnrichEndpoint_EngineError(t *testing.T) {
	eng := &retryEnrichErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/test-id/retry-enrich", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type retryEnrichErrorEngine struct{ MockEngine }

func (e *retryEnrichErrorEngine) RetryEnrich(_ context.Context, _, _ string) (*RetryEnrichResponse, error) {
	return nil, errors.New("enrich failed")
}

// ---------------------------------------------------------------------------
// handleListDeleted — error path and limit clamping (90.9%)
// ---------------------------------------------------------------------------

func TestListDeletedEndpoint_LimitClamping(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/deleted?vault=default&limit=9999", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListDeletedEndpoint_EngineError(t *testing.T) {
	eng := &listDeletedErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/deleted?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type listDeletedErrorEngine struct{ MockEngine }

func (e *listDeletedErrorEngine) ListDeleted(_ context.Context, _ string, _ int) (*ListDeletedResponse, error) {
	return nil, errors.New("list deleted failed")
}

// ---------------------------------------------------------------------------
// handleGetEngramLinks — engine error path (80%)
// ---------------------------------------------------------------------------

func TestGetEngramLinks_EngineError(t *testing.T) {
	eng := &engramLinksErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/test-id/links", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type engramLinksErrorEngine struct{ MockEngine }

func (e *engramLinksErrorEngine) GetEngramLinks(_ context.Context, _ *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	return nil, errors.New("links error")
}

// ---------------------------------------------------------------------------
// handleGetSession — engine error path (94.1%)
// ---------------------------------------------------------------------------

func TestGetSession_EngineError(t *testing.T) {
	eng := &sessionErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/session?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

type sessionErrorEngine struct{ MockEngine }

func (e *sessionErrorEngine) GetSession(_ context.Context, _ *GetSessionRequest) (*GetSessionResponse, error) {
	return nil, errors.New("session error")
}

// ---------------------------------------------------------------------------
// recoveryMiddleware — panic path (50%)
// ---------------------------------------------------------------------------

func TestRecoveryMiddleware_PanicReturns500(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Wrap a panicking handler in the recovery middleware and invoke it directly.
	panickingHandler := server.recoveryMiddleware(func(w http.ResponseWriter, r *http.Request) {
		panic("deliberate test panic")
	})

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	panickingHandler(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on panic, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// lifecycleStateLabel — 0% coverage (called inside RESTEngineWrapper.Restore)
// ---------------------------------------------------------------------------

func TestLifecycleStateLabel_AllStates(t *testing.T) {
	tests := []struct {
		state storage.LifecycleState
		want  string
	}{
		{storage.StatePlanning, "planning"},
		{storage.StateActive, "active"},
		{storage.StatePaused, "paused"},
		{storage.StateBlocked, "blocked"},
		{storage.StateCompleted, "completed"},
		{storage.StateCancelled, "cancelled"},
		{storage.StateArchived, "archived"},
		{storage.StateSoftDeleted, "soft_deleted"},
		{storage.LifecycleState(255), "unknown(255)"},
	}
	for _, tc := range tests {
		got := lifecycleStateLabel(tc.state)
		if got != tc.want {
			t.Errorf("lifecycleStateLabel(%d) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ctxVault — hit the fallback branch (66.7%)
// ---------------------------------------------------------------------------

func TestCtxVault_FallbackToDefault(t *testing.T) {
	// A plain request with no vault stored in context returns "default".
	req := httptest.NewRequest("GET", "/", nil)
	got := ctxVault(req)
	if got != "default" {
		t.Errorf("expected 'default', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// handleListEngrams — negative offset clamping
// ---------------------------------------------------------------------------

func TestListEngrams_NegativeOffset(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams?vault=default&offset=-5", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for negative offset, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleHello — happy path (covers the success branch)
// ---------------------------------------------------------------------------

func TestHandleHello_Success(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/hello", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp HelloResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ServerVersion == "" {
		t.Error("expected non-empty ServerVersion")
	}
}

// ---------------------------------------------------------------------------
// handleStats — no vault param (exercises the empty-vault code path)
// ---------------------------------------------------------------------------

func TestHandleStats_NoVault(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp StatResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.EngramCount != 100 {
		t.Errorf("expected EngramCount=100, got %d", resp.EngramCount)
	}
}
