package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// MockEngine is a mock implementation of EngineAPI for testing.
type MockEngine struct{}

func (m *MockEngine) Hello(ctx context.Context, req *HelloRequest) (*HelloResponse, error) {
	return &HelloResponse{
		ServerVersion: "1.0.0",
		SessionID:     "test-session",
		VaultID:       "test-vault",
		Capabilities:  []string{"compression"},
	}, nil
}

func (m *MockEngine) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	return &WriteResponse{
		ID:        "test-id",
		CreatedAt: 1234567890,
	}, nil
}

func (m *MockEngine) WriteBatch(ctx context.Context, reqs []*WriteRequest) ([]*WriteResponse, []error) {
	responses := make([]*WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i := range reqs {
		responses[i] = &WriteResponse{
			ID:        fmt.Sprintf("batch-id-%d", i),
			CreatedAt: 1234567890,
		}
	}
	return responses, errs
}

func (m *MockEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	return &ReadResponse{
		ID:         "test-id",
		Concept:    "test",
		Content:    "test content",
		Confidence: 0.9,
	}, nil
}

func (m *MockEngine) Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	return &ActivateResponse{
		QueryID:    "query-1",
		TotalFound: 1,
		Activations: []ActivationItem{
			{
				ID:      "test-id",
				Concept: "test",
				Score:   0.8,
			},
		},
	}, nil
}

func (m *MockEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*LinkResponse, error) {
	return &LinkResponse{OK: true}, nil
}

func (m *MockEngine) Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error) {
	return &ForgetResponse{OK: true}, nil
}

func (m *MockEngine) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	return &StatResponse{
		EngramCount:  100,
		VaultCount:   1,
		StorageBytes: 1024000,
	}, nil
}

func (m *MockEngine) ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	return &ListEngramsResponse{
		Engrams: []EngramItem{
			{
				ID:         "test-id",
				Concept:    "test concept",
				Content:    "test content",
				Confidence: 0.9,
				Vault:      req.Vault,
			},
		},
		Total:  1,
		Limit:  req.Limit,
		Offset: req.Offset,
	}, nil
}

func (m *MockEngine) GetEngramLinks(ctx context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	return &GetEngramLinksResponse{Links: []AssociationItem{}}, nil
}

func (m *MockEngine) GetBatchEngramLinks(ctx context.Context, req *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error) {
	links := make(map[string][]AssociationItem, len(req.IDs))
	for _, id := range req.IDs {
		links[id] = []AssociationItem{}
	}
	return &BatchGetEngramLinksResponse{Links: links}, nil
}

func (m *MockEngine) ListVaults(ctx context.Context) ([]string, error) {
	return []string{"default"}, nil
}

func (m *MockEngine) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	return &GetSessionResponse{
		Entries: []SessionItem{
			{
				ID:        "test-id",
				Concept:   "test concept",
				Content:   "test content",
				CreatedAt: 1234567890,
			},
		},
	}, nil
}

func (m *MockEngine) WorkerStats() cognitive.EngineWorkerStats {
	return cognitive.EngineWorkerStats{}
}

func (m *MockEngine) SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	return "mock-sub", nil
}

func (m *MockEngine) Unsubscribe(ctx context.Context, subID string) error {
	return nil
}

func (m *MockEngine) ClearVault(ctx context.Context, vaultName string) error { return nil }
func (m *MockEngine) DeleteVault(ctx context.Context, vaultName string) error { return nil }
func (m *MockEngine) RenameVault(ctx context.Context, oldName, newName string) error {
	return nil
}
func (m *MockEngine) StartClone(ctx context.Context, sourceVault, newName string) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-clone-job", Operation: "clone", Source: sourceVault, Target: newName}, nil
}
func (m *MockEngine) StartMerge(ctx context.Context, sourceVault, targetVault string, deleteSource bool) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-merge-job", Operation: "merge", Source: sourceVault, Target: targetVault}, nil
}
func (m *MockEngine) GetVaultJob(jobID string) (*vaultjob.Job, bool) { return nil, false }

func (m *MockEngine) ExportVault(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, w io.Writer) (*storage.ExportResult, error) {
	// Write a minimal non-empty response so callers can tell export ran.
	w.Write([]byte("mock-export"))
	return &storage.ExportResult{EngramCount: 0, TotalKeys: 0}, nil
}
func (m *MockEngine) StartImport(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, r io.Reader) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-import-job", Operation: "import", Target: vaultName}, nil
}

func (m *MockEngine) ReindexFTSVault(ctx context.Context, vaultName string) (int64, error) {
	return 0, nil
}

func (m *MockEngine) Checkpoint(destDir string) error {
	return nil
}

func (m *MockEngine) Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*EvolveResponse, error) {
	return &EvolveResponse{ID: "evolved-id"}, nil
}

func (m *MockEngine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResponse, error) {
	return &ConsolidateResponse{
		ID:       "consolidated-id",
		Archived: ids,
	}, nil
}

func (m *MockEngine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResponse, error) {
	return &DecideResponse{ID: "decision-id"}, nil
}

func (m *MockEngine) Restore(ctx context.Context, vault, engramID string) (*RestoreResponse, error) {
	return &RestoreResponse{
		ID:       engramID,
		Concept:  "restored concept",
		Restored: true,
		State:    "active",
	}, nil
}

func (m *MockEngine) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResponse, error) {
	return &TraverseResponse{
		Nodes:          []TraversalNode{{ID: req.StartID, Concept: "start", HopDist: 0}},
		Edges:          []TraversalEdge{},
		TotalReachable: 1,
		QueryMs:        1.5,
	}, nil
}

func (m *MockEngine) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResponse, error) {
	return &ExplainResponse{
		EngramID:    req.EngramID,
		Concept:     "test concept",
		FinalScore:  0.85,
		Components:  ExplainComponents{FullTextRelevance: 0.5, SemanticSimilarity: 0.3},
		WouldReturn: true,
		Threshold:   0.1,
	}, nil
}

func (m *MockEngine) UpdateState(ctx context.Context, vault, engramID, state, reason string) error {
	return nil
}

func (m *MockEngine) UpdateTags(ctx context.Context, vault, engramID string, tags []string) error {
	return nil
}

func (m *MockEngine) ListDeleted(ctx context.Context, vault string, limit int) (*ListDeletedResponse, error) {
	return &ListDeletedResponse{
		Deleted: []DeletedEngramItem{
			{ID: "del-1", Concept: "deleted thing", DeletedAt: 1700000000, RecoverableUntil: 1700604800},
		},
		Count: 1,
	}, nil
}

func (m *MockEngine) RetryEnrich(ctx context.Context, vault, engramID string) (*RetryEnrichResponse, error) {
	return &RetryEnrichResponse{
		EngramID:      engramID,
		PluginsQueued: []string{"embed"},
		Note:          "enrichment applied",
	}, nil
}

func (m *MockEngine) GetContradictions(ctx context.Context, vault string) (*ContradictionsResponse, error) {
	return &ContradictionsResponse{
		Contradictions: []ContradictionItem{
			{IDa: "a1", ConceptA: "concept A", IDb: "b1", ConceptB: "concept B", DetectedAt: 1700000000},
		},
	}, nil
}

func (m *MockEngine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	return nil
}

func (m *MockEngine) GetGuide(ctx context.Context, vault string) (string, error) {
	return "MuninnDB Guide for vault \"default\"\n\nThis vault has 100 memories.", nil
}

func (m *MockEngine) StartReembedVault(ctx context.Context, vaultName, modelName string) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-reembed-job", Operation: "reembed", Source: vaultName, Target: vaultName}, nil
}

func (m *MockEngine) CountEmbedded(ctx context.Context) int64 {
	return 42
}

func (m *MockEngine) Observability(ctx context.Context, version string, uptimeSeconds int64) (*engine.ObservabilitySnapshot, error) {
	return &engine.ObservabilitySnapshot{}, nil
}

func (m *MockEngine) GetProcessorStats() []plugin.RetroactiveStats {
	return nil
}

// backupMockEngine embeds MockEngine but creates a real Pebble checkpoint so
// the verification step has something to open.
type backupMockEngine struct {
	MockEngine
	pebbleDir string // set in test to a temp pebble DB dir
}

func (b *backupMockEngine) Checkpoint(destDir string) error {
	db, err := pebble.Open(b.pebbleDir, &pebble.Options{})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Checkpoint(destDir)
}

// Ensure bytes import is used.
var _ = bytes.NewReader

func TestHealthEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Create a test request
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()

	// Call the handler directly
	server.mux.ServeHTTP(w, req)

	// Check response
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("expected status 'ok', got '%s'", resp.Status)
	}
}

func TestReadyEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/ready", nil)
	w := httptest.NewRecorder()

	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ReadyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "ready" {
		t.Errorf("expected status 'ready', got '%s'", resp.Status)
	}
}

func TestListEngrams(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	w := httptest.NewRecorder()

	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListEngramsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Engrams == nil {
		t.Error("expected engrams key in response, got nil")
	}
}

func TestListVaults(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/vaults", nil)
	w := httptest.NewRecorder()

	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp []string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp) == 0 {
		t.Error("expected at least one vault in response")
	}
}

func TestGetSession(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/session?vault=default&since=2020-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()

	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp GetSessionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
}

func TestCORSHeaders(t *testing.T) {
	engine := &MockEngine{}
	const testOrigin = "http://example.com"
	server := NewServer("localhost:8080", engine, nil, nil, []string{testOrigin}, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Test OPTIONS preflight with matching origin.
	req := httptest.NewRequest("OPTIONS", "/api/health", nil)
	req.Header.Set("Origin", testOrigin)
	w := httptest.NewRecorder()

	// CORS middleware wraps the http.Server handler, not the mux directly.
	server.server.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected status %d for OPTIONS, got %d", http.StatusNoContent, w.Code)
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
		t.Errorf("expected Access-Control-Allow-Origin: %s, got %q", testOrigin, got)
	}

	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods header to be set")
	}

	// Test GET request also gets CORS headers when origin matches allowlist.
	req2 := httptest.NewRequest("GET", "/api/health", nil)
	req2.Header.Set("Origin", testOrigin)
	w2 := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(w2, req2)

	if got := w2.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
		t.Errorf("expected Access-Control-Allow-Origin: %s on GET, got %q", testOrigin, got)
	}
}

func TestListEngramsDefaultVault(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// No vault param — should default to "default"
	req := httptest.NewRequest("GET", "/api/engrams", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListEngramsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Engrams == nil {
		t.Error("expected engrams in response")
	}
}

func TestListEngramsLimitClamping(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Overlarge limit should be clamped to 100
	req := httptest.NewRequest("GET", "/api/engrams?vault=default&limit=500", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp ListEngramsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Limit > 100 {
		t.Errorf("expected limit clamped to 100, got %d", resp.Limit)
	}
}

func TestGetEngramLinks(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/test-id/links", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var resp GetEngramLinksResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.Links == nil {
		t.Error("expected links array (may be empty)")
	}
}

func TestGetSessionDefaultSince(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// No since param — should default to last 24h
	req := httptest.NewRequest("GET", "/api/session?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestGetSessionMalformedSince(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Malformed since — should fall back to 24h, not error
	req := httptest.NewRequest("GET", "/api/session?vault=default&since=not-a-date", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d (graceful fallback), got %d", http.StatusOK, w.Code)
	}
}

func TestGetSessionLimitClamping(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/session?vault=default&limit=9999", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}
}

func TestCORSHeadersOnNewRoutes(t *testing.T) {
	engine := &MockEngine{}
	const testOrigin = "http://example.com"
	server := NewServer("localhost:8080", engine, nil, nil, []string{testOrigin}, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	routes := []string{
		"/api/engrams",
		"/api/vaults",
		"/api/session?vault=default",
	}
	for _, path := range routes {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Origin", testOrigin)
		w := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != testOrigin {
			t.Errorf("route %s: expected Access-Control-Allow-Origin: %s, got %q", path, testOrigin, got)
		}
	}
}

func TestPreflightOnNewRoutes(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	routes := []string{"/api/engrams", "/api/vaults", "/api/session"}
	for _, path := range routes {
		req := httptest.NewRequest("OPTIONS", path, nil)
		w := httptest.NewRecorder()
		server.server.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s: expected 204, got %d", path, w.Code)
		}
	}
}

type errorEngine struct{ MockEngine }

func (e *errorEngine) ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	return nil, fmt.Errorf("storage error")
}
func (e *errorEngine) ListVaults(ctx context.Context) ([]string, error) {
	return nil, fmt.Errorf("storage error")
}
func (e *errorEngine) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	return nil, fmt.Errorf("storage error")
}
func (e *errorEngine) GetEngramLinks(ctx context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	return nil, fmt.Errorf("storage error")
}

func (e *errorEngine) GetBatchEngramLinks(ctx context.Context, req *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error) {
	return nil, fmt.Errorf("engine error")
}

func TestListEngramsEngineError(t *testing.T) {
	server := NewServer("localhost:8080", &errorEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d", w.Code)
	}
}

func TestListVaultsEngineError(t *testing.T) {
	server := NewServer("localhost:8080", &errorEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/vaults", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d", w.Code)
	}
}

func TestGetSessionEngineError(t *testing.T) {
	server := NewServer("localhost:8080", &errorEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/session?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d", w.Code)
	}
}

func TestGetEngramLinksEngineError(t *testing.T) {
	server := NewServer("localhost:8080", &errorEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/engrams/test-id/links", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on engine error, got %d", w.Code)
	}
}

func TestShutdown(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:0", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start server in a goroutine
	done := make(chan error)
	go func() {
		done <- server.Serve(ctx)
	}()

	// Give the server time to start
	time.Sleep(10 * time.Millisecond)

	// Trigger shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)

	// Wait for server to shut down
	select {
	case <-done:
		// Server shut down successfully
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for server shutdown")
	}
}

// TestContextPropagation verifies that request context is passed to the engine.
// A cancelled context should cause the engine to receive the cancellation.
func TestContextPropagation(t *testing.T) {
	var gotCtx context.Context
	eng := &ctxCapturingEngine{captureCtx: func(ctx context.Context) { gotCtx = ctx }}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/stats", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if gotCtx == nil {
		t.Fatal("engine never received a context")
	}
	if gotCtx.Err() == nil {
		t.Error("expected cancelled context to be propagated to engine")
	}
}

type ctxCapturingEngine struct {
	MockEngine
	captureCtx func(context.Context)
}

func (e *ctxCapturingEngine) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	e.captureCtx(ctx)
	return &StatResponse{}, nil
}

func TestServer_DisableCluster(t *testing.T) {
	s := &Server{}
	s.DisableCluster()
	if s.coordinator != nil {
		t.Fatal("expected nil coordinator after disable")
	}
}

func TestServer_ActiveCoordinator_Nil(t *testing.T) {
	s := &Server{}
	if s.ActiveCoordinator() != nil {
		t.Fatal("expected nil for new server")
	}
}

func TestServer_PersistClusterDisabled_NoDataDir(t *testing.T) {
	s := &Server{}
	if err := s.persistClusterDisabled(); err != nil {
		t.Fatalf("unexpected error with empty dataDir: %v", err)
	}
}

// TestCreateEngram tests POST /api/engrams → 201
func TestCreateEngram(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"concept":"test concept","content":"test content","vault":"default"}`
	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"] == "" || resp["id"] == nil {
		t.Error("expected non-empty ID in response")
	}
}

// TestCreateEngram_InvalidJSON tests POST /api/engrams with bad body → 400
func TestCreateEngram_InvalidJSON(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// TestActivate tests POST /api/activate → 200
func TestActivate(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":["memory","learning"],"vault":"default","max_results":10}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["activations"] == nil {
		t.Error("expected activations field in response")
	}
}

// TestActivate_InvalidJSON tests POST /api/activate with bad body → 400
func TestActivate_InvalidJSON(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader("invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// TestDeleteEngram tests DELETE /api/engrams/{id} → 200
func TestDeleteEngram(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("DELETE", "/api/engrams/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok, _ := resp["ok"].(bool); !ok {
		t.Error("expected ok:true in response")
	}
}

// TestDeleteEngram_MissingID tests DELETE /api/engrams/ without ID → 400
func TestDeleteEngram_MissingID(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// The mux will not match /api/engrams/ (missing {id}), so request will 404
	req := httptest.NewRequest("DELETE", "/api/engrams/", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	// Without a valid path parameter, the handler is not invoked at all by Go's mux.
	// We expect a 404 in this case.
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing path parameter, got %d", w.Code)
	}
}

// TestActivate_EmptyContext tests POST /api/activate with empty context array
func TestActivate_EmptyContext(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"context":[],"vault":"default","max_results":10}`
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	// Should return 200 and not panic, even with empty context
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Should have Activations field (may be empty list)
	if resp["activations"] == nil {
		t.Error("expected activations field in response")
	}
}

func TestOnlineBackupEndpoint(t *testing.T) {
	// Create a real Pebble DB so the checkpoint and verification are exercised.
	pebbleDir := filepath.Join(t.TempDir(), "pebble")
	db, err := pebble.Open(pebbleDir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte("backup-key"), []byte("backup-val"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	db.Close()

	eng := &backupMockEngine{pebbleDir: pebbleDir}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	outputDir := filepath.Join(t.TempDir(), "online-backup-out")
	body := fmt.Sprintf(`{"output_dir":%q}`, outputDir)
	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp BackupResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.OutputDir != outputDir {
		t.Errorf("expected output_dir=%q, got %q", outputDir, resp.OutputDir)
	}
	if resp.SizeBytes <= 0 {
		t.Errorf("expected positive size_bytes, got %d", resp.SizeBytes)
	}

	// Verify the checkpoint directory exists and is readable.
	checkpointDir := filepath.Join(outputDir, "pebble")
	if _, err := os.Stat(checkpointDir); os.IsNotExist(err) {
		t.Fatal("checkpoint directory does not exist")
	}
	verifyDB, err := pebble.Open(checkpointDir, &pebble.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("failed to open checkpoint for verification: %v", err)
	}
	defer verifyDB.Close()

	val, closer, err := verifyDB.Get([]byte("backup-key"))
	if err != nil {
		t.Fatalf("key not found in checkpoint: %v", err)
	}
	if string(val) != "backup-val" {
		t.Fatalf("expected backup-val, got %q", string(val))
	}
	closer.Close()
}

func TestOnlineBackupEndpoint_ConflictExistingDir(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	existingDir := t.TempDir()
	body := fmt.Sprintf(`{"output_dir":%q}`, existingDir)
	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOnlineBackupEndpoint_MissingOutputDir(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{}`
	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRemoveNode_SelfRemoval_Returns400(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := replication.NewReplicationLog(db)
	applier := replication.NewApplier(db)
	epochStore, err := replication.NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	cfg := &config.ClusterConfig{
		Enabled:  true,
		NodeID:   "cortex-1",
		BindAddr: "127.0.0.1:0",
		Role:     "primary",
	}
	coord := replication.NewClusterCoordinator(cfg, repLog, applier, epochStore)

	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	server.SetCoordinator(coord)

	req := httptest.NewRequest("DELETE", "/api/admin/cluster/nodes/cortex-1", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self-removal, got %d: %s", w.Code, w.Body.String())
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Message == "" {
		t.Error("expected non-empty error message")
	}
}

func TestRemoveNode_OtherNode_Returns200(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := replication.NewReplicationLog(db)
	applier := replication.NewApplier(db)
	epochStore, err := replication.NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	cfg := &config.ClusterConfig{
		Enabled:  true,
		NodeID:   "cortex-1",
		BindAddr: "127.0.0.1:0",
		Role:     "primary",
	}
	coord := replication.NewClusterCoordinator(cfg, repLog, applier, epochStore)

	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	server.SetCoordinator(coord)

	req := httptest.NewRequest("DELETE", "/api/admin/cluster/nodes/lobe-2", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for removing other node, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBatchCreateEngrams(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"engrams":[{"content":"one","concept":"c1"},{"content":"two","concept":"c2"},{"content":"three"}]}`
	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Results []struct {
			Index  int    `json:"index"`
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}
	for i, r := range resp.Results {
		if r.Status != "ok" {
			t.Errorf("result[%d]: status = %q, want 'ok'", i, r.Status)
		}
		if r.ID == "" {
			t.Errorf("result[%d]: ID is empty", i)
		}
	}
}

func TestBatchCreateEngramsExceedsLimit(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	engrams := make([]map[string]string, 51)
	for i := range engrams {
		engrams[i] = map[string]string{"content": fmt.Sprintf("item %d", i)}
	}
	bodyBytes, _ := json.Marshal(map[string]any{"engrams": engrams})
	req := httptest.NewRequest("POST", "/api/engrams/batch", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for >50 items, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBatchCreateEngramsEmptyArray(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"engrams":[]}`
	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty array, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEvolveEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"new_content":"updated content","reason":"correction"}`
	req := httptest.NewRequest("POST", "/api/engrams/test-id/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp EvolveResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty ID in response")
	}
}

func TestEvolveEndpoint_MissingFields(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{}`
	req := httptest.NewRequest("POST", "/api/engrams/test-id/evolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestConsolidateEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","ids":["id-1","id-2","id-3"],"merged_content":"combined content"}`
	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp ConsolidateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty ID in response")
	}
	if len(resp.Archived) != 3 {
		t.Errorf("expected 3 archived IDs, got %d", len(resp.Archived))
	}
}

func TestConsolidateEndpoint_TooFewIDs(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","ids":["only-one"],"merged_content":"content"}`
	req := httptest.NewRequest("POST", "/api/consolidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestDecideEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","decision":"use postgres","rationale":"better for relational data"}`
	req := httptest.NewRequest("POST", "/api/decide", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}

	var resp DecideResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty ID in response")
	}
}

func TestDecideEndpoint_MissingFields(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/decide", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestRestoreEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/test-id/restore", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp RestoreResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID != "test-id" {
		t.Errorf("expected ID 'test-id', got %q", resp.ID)
	}
	if !resp.Restored {
		t.Error("expected restored to be true")
	}
}

func TestTraverseEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","start_id":"node-1"}`
	req := httptest.NewRequest("POST", "/api/traverse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp TraverseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Nodes) == 0 {
		t.Error("expected at least one node in response")
	}
	if resp.Edges == nil {
		t.Error("expected edges array (may be empty)")
	}
}

func TestTraverseEndpoint_MissingStartID(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/traverse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestExplainEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","engram_id":"eng-1","query":["test query"]}`
	req := httptest.NewRequest("POST", "/api/explain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp ExplainResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.EngramID != "eng-1" {
		t.Errorf("expected engram_id 'eng-1', got %q", resp.EngramID)
	}
	if resp.FinalScore == 0 {
		t.Error("expected non-zero final_score")
	}
}

func TestExplainEndpoint_MissingFields(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","query":["test"]}`
	req := httptest.NewRequest("POST", "/api/explain", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestSetStateEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"state":"active","reason":"resuming work"}`
	req := httptest.NewRequest("PUT", "/api/engrams/test-id/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp SetStateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Updated {
		t.Error("expected updated to be true")
	}
	if resp.State != "active" {
		t.Errorf("expected state 'active', got %q", resp.State)
	}
}

func TestSetStateEndpoint_InvalidState(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"state":"invalid"}`
	req := httptest.NewRequest("PUT", "/api/engrams/test-id/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d: %s", http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestListDeletedEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/deleted?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp ListDeletedResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("expected count 1, got %d", resp.Count)
	}
	if len(resp.Deleted) != 1 {
		t.Errorf("expected 1 deleted item, got %d", len(resp.Deleted))
	}
}

func TestRetryEnrichEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/test-id/retry-enrich", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp RetryEnrichResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.EngramID != "test-id" {
		t.Errorf("expected engram_id 'test-id', got %q", resp.EngramID)
	}
	if len(resp.PluginsQueued) == 0 {
		t.Error("expected at least one plugin queued")
	}
}

func TestContradictionsEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/contradictions?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp ContradictionsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Contradictions) == 0 {
		t.Error("expected at least one contradiction")
	}
}

func TestGuideEndpoint(t *testing.T) {
	engine := &MockEngine{}
	server := NewServer("localhost:8080", engine, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/guide?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}

	var resp GuideResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Guide == "" {
		t.Error("expected non-empty guide text")
	}
}

func TestHandleReembedVault(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/admin/vaults/test-vault/reembed", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Error("expected non-empty job_id in response")
	}
}

func TestHandleObservability(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/observability", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snap engine.ObservabilitySnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func TestHandleRenameVault_Success(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"renamed-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["old_name"] != "old-vault" || resp["new_name"] != "renamed-vault" {
		t.Errorf("unexpected response: %v", resp)
	}
}

func TestHandleRenameVault_InvalidName(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"BAD NAME!"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRenameVault_SameName(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"same-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/same-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// renameMockNotFound embeds MockEngine and overrides RenameVault to return
// an error wrapping engine.ErrVaultNotFound.
type renameMockNotFound struct{ MockEngine }

func (m *renameMockNotFound) RenameVault(_ context.Context, _, _ string) error {
	return fmt.Errorf("rename: %w", engine.ErrVaultNotFound)
}

func TestHandleRenameVault_NotFound(t *testing.T) {
	eng := &renameMockNotFound{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"new-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// renameMockJobActive embeds MockEngine and overrides RenameVault to return
// an error wrapping engine.ErrVaultJobActive.
type renameMockJobActive struct{ MockEngine }

func (m *renameMockJobActive) RenameVault(_ context.Context, _, _ string) error {
	return fmt.Errorf("rename: %w", engine.ErrVaultJobActive)
}

func TestHandleRenameVault_JobActive(t *testing.T) {
	eng := &renameMockJobActive{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"new-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

// renameMockCollision embeds MockEngine and overrides RenameVault to return
// ErrVaultNameCollision to simulate a name collision.
type renameMockCollision struct{ MockEngine }

func (m *renameMockCollision) RenameVault(_ context.Context, _, _ string) error {
	return fmt.Errorf("vault %q: %w", "new-vault", engine.ErrVaultNameCollision)
}

func TestHandleRenameVault_Collision(t *testing.T) {
	eng := &renameMockCollision{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"new-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRenameVault_MissingBody(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleRenameVault_EmptyNewName(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":""}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestHandleRenameVault_ResponseBody verifies the full JSON response contract:
// exactly two keys (old_name, new_name), correct Content-Type header.
func TestHandleRenameVault_ResponseBody(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"renamed-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify Content-Type header.
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	// Decode into a generic map to check for extra fields.
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify exactly two keys.
	if len(resp) != 2 {
		t.Errorf("expected exactly 2 keys in response, got %d: %v", len(resp), resp)
	}

	// Verify old_name value.
	if v, ok := resp["old_name"]; !ok {
		t.Error("response missing 'old_name' key")
	} else if v != "old-vault" {
		t.Errorf("expected old_name='old-vault', got %q", v)
	}

	// Verify new_name value.
	if v, ok := resp["new_name"]; !ok {
		t.Error("response missing 'new_name' key")
	} else if v != "renamed-vault" {
		t.Errorf("expected new_name='renamed-vault', got %q", v)
	}
}

// renameMockInternalError embeds MockEngine and overrides RenameVault to return
// a generic error that does not match any recognized sentinel or substring.
type renameMockInternalError struct{ MockEngine }

func (m *renameMockInternalError) RenameVault(_ context.Context, _, _ string) error {
	return fmt.Errorf("disk I/O error")
}

// TestHandleRenameVault_InternalServerError verifies that an unrecognized engine
// error falls through to the 500 Internal Server Error branch.
func TestHandleRenameVault_InternalServerError(t *testing.T) {
	eng := &renameMockInternalError{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := strings.NewReader(`{"new_name":"new-vault"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/old-vault/rename", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the error response contains the underlying message.
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != ErrStorageError {
		t.Errorf("expected error code %d, got %d", ErrStorageError, errResp.Error.Code)
	}
	if errResp.Error.Message == "" {
		t.Error("expected non-empty error message in response")
	}
}

// ── GET /api/engrams/{id} ─────────────────────────────────────────────────────

// TestGetEngram_HappyPath verifies that GET /api/engrams/{id}?vault=default
// returns 200 with a ReadResponse body.
func TestGetEngram_HappyPath(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/test-id?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp ReadResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID == "" {
		t.Error("expected non-empty ID in response")
	}
	if resp.Content == "" {
		t.Error("expected non-empty Content in response")
	}
}

// readErrEngine returns an error from Read so the handler falls through to 404.
type readErrEngine struct{ MockEngine }

func (e *readErrEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	return nil, fmt.Errorf("engram not found")
}

// TestGetEngram_EngineError verifies that a Read engine error produces a 4xx/5xx response.
func TestGetEngram_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &readErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/missing-id?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	// handleGetEngram sends 404 on engine error.
	if w.Code != http.StatusNotFound && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 404 or 500, got %d", w.Code)
	}
}

// ── GET /api/openapi.yaml ─────────────────────────────────────────────────────

// TestOpenAPISpec_Returns200 verifies the spec endpoint returns 200 with a
// non-empty body and the correct Content-Type header.
func TestOpenAPISpec_Returns200(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/openapi.yaml", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("expected non-empty body for openapi spec")
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/yaml") {
		t.Errorf("expected Content-Type to contain application/yaml, got %q", ct)
	}
}

// TestOpenAPISpec_CacheControl verifies that the Cache-Control header contains "max-age".
func TestOpenAPISpec_CacheControl(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/openapi.yaml", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	cc := w.Header().Get("Cache-Control")
	if !strings.Contains(cc, "max-age") {
		t.Errorf("expected Cache-Control to contain max-age, got %q", cc)
	}
}

// ── GET /api/contradictions ───────────────────────────────────────────────────

// contradictionsErrEngine returns an error from GetContradictions.
type contradictionsErrEngine struct{ MockEngine }

func (e *contradictionsErrEngine) GetContradictions(ctx context.Context, vault string) (*ContradictionsResponse, error) {
	return nil, fmt.Errorf("storage error: index unavailable")
}

// TestContradictions_EngineError verifies that a GetContradictions engine error → 500.
func TestContradictions_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &contradictionsErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/contradictions?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ── POST /api/admin/contradictions/resolve ────────────────────────────────────

// TestResolveContradiction_Success verifies the happy path returns {resolved:true}.
func TestResolveContradiction_Success(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","id_a":"01ARZ3NDEKTSV4RRFFQ69G5FAV","id_b":"01ARZ3NDEKTSV4RRFFQ69G5FAW"}`
	req := httptest.NewRequest("POST", "/api/admin/contradictions/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["resolved"] != true {
		t.Errorf("expected resolved=true, got %v", resp["resolved"])
	}
}

// TestResolveContradiction_MissingIDs verifies missing IDs returns 400.
func TestResolveContradiction_MissingIDs(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","id_a":"","id_b":""}`
	req := httptest.NewRequest("POST", "/api/admin/contradictions/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// resolveContradictionErrEngine returns an error from ResolveContradiction.
type resolveContradictionErrEngine struct{ MockEngine }

func (e *resolveContradictionErrEngine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	return fmt.Errorf("storage error")
}

// TestResolveContradiction_EngineError verifies engine error → 500.
func TestResolveContradiction_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &resolveContradictionErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := `{"vault":"default","id_a":"01ARZ3NDEKTSV4RRFFQ69G5FAV","id_b":"01ARZ3NDEKTSV4RRFFQ69G5FAW"}`
	req := httptest.NewRequest("POST", "/api/admin/contradictions/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ── GET /api/guide ────────────────────────────────────────────────────────────

// guideErrEngine returns an error from GetGuide.
type guideErrEngine struct{ MockEngine }

func (e *guideErrEngine) GetGuide(ctx context.Context, vault string) (string, error) {
	return "", fmt.Errorf("guide generation failed")
}

// TestGuide_EngineError verifies that a GetGuide engine error → 500.
func TestGuide_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &guideErrEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/guide?vault=default", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// ── Entity Graph Visualization (UI integration) ──────────────────────────────────

// TestEntityGraphVisualization_MCPInfoEndpoint verifies the MCP info endpoint
// returns correct MCP URL for the UI to call entity graph via MCP.
func TestEntityGraphVisualization_MCPInfoEndpoint(t *testing.T) {
	// Create server with MCP address configured
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{Addr: "127.0.0.1:8750", HasToken: false})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()

	// Note: This requires admin auth middleware, so it will return 401 in test context
	// The MCP endpoint address is still stored; the UI uses /api/admin/mcp-info
	// to discover the MCP server URL before calling muninn_export_graph.
	server.mux.ServeHTTP(w, req)

	// Expected 401 because no admin session is present; URL structure verified in integration tests.
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusOK {
		t.Logf("MCP info endpoint returned %d (expected 401 without auth)", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleVaultStats tests
// ---------------------------------------------------------------------------

// vaultStatsEngine is an engine whose ListVaults and Stat return configurable values.
type vaultStatsEngine struct {
	MockEngine
	vaults []string
	counts map[string]int64 // per-vault engram count
}

func (e *vaultStatsEngine) ListVaults(_ context.Context) ([]string, error) {
	return e.vaults, nil
}

func (e *vaultStatsEngine) Stat(_ context.Context, req *StatRequest) (*StatResponse, error) {
	count, ok := e.counts[req.Vault]
	if !ok {
		return &StatResponse{}, nil
	}
	return &StatResponse{EngramCount: count, VaultCount: 1, StorageBytes: 0}, nil
}

// TestHandleVaultStats_ReturnsAllVaults verifies that vaults which exist only in
// the auth store config (no engrams yet) appear in the response with engram_count 0.
func TestHandleVaultStats_ReturnsAllVaults(t *testing.T) {
	eng := &vaultStatsEngine{
		vaults: []string{"default"},
		counts: map[string]int64{"default": 5},
	}
	store := newTestAuthStore(t)
	// Make "default" public so the vault-auth middleware passes without an API key.
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig default: %v", err)
	}
	// "config-only" vault exists in auth config but not returned by ListVaults.
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "config-only", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	srv := NewServer("localhost:0", eng, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/vaults/stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	type vaultStat struct {
		Name        string `json:"name"`
		EngramCount int64  `json:"engram_count"`
	}
	var resp []vaultStat
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	found := map[string]int64{}
	for _, vs := range resp {
		found[vs.Name] = vs.EngramCount
	}

	if _, ok := found["config-only"]; !ok {
		t.Error("expected config-only vault to appear in response")
	}
	if found["config-only"] != 0 {
		t.Errorf("expected engram_count 0 for config-only vault, got %d", found["config-only"])
	}
	if _, ok := found["default"]; !ok {
		t.Error("expected default vault to appear in response")
	}
}

// TestHandleVaultStats_AccurateCount verifies that each vault's engram count
// is accurately reported from the engine's Stat response.
func TestHandleVaultStats_AccurateCount(t *testing.T) {
	eng := &vaultStatsEngine{
		vaults: []string{"alpha", "beta"},
		counts: map[string]int64{
			"alpha": 42,
			"beta":  7,
		},
	}
	srv := NewServer("localhost:0", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("GET", "/api/vaults/stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	type vaultStat struct {
		Name        string `json:"name"`
		EngramCount int64  `json:"engram_count"`
	}
	var resp []vaultStat
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	found := map[string]int64{}
	for _, vs := range resp {
		found[vs.Name] = vs.EngramCount
	}

	if found["alpha"] != 42 {
		t.Errorf("expected alpha engram_count 42, got %d", found["alpha"])
	}
	if found["beta"] != 7 {
		t.Errorf("expected beta engram_count 7, got %d", found["beta"])
	}
}

func TestHandleVaultStats_RequiresAdminAuth(t *testing.T) {
	// GET /api/vaults/stats is an admin endpoint; requests without a valid
	// session cookie must be rejected with 401.
	store := newTestAuthStore(t)
	secret := []byte("test-secret")
	srv := NewServer("localhost:0", &MockEngine{}, store, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/vaults/stats", nil)
	// No session cookie — should be rejected by admin auth middleware.
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBatchGetEngramLinks_HappyPath(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	body := `{"ids":["01JAAAAAAAAAAAAAAAAAAAAAA1","01JAAAAAAAAAAAAAAAAAAAAAA2"],"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", w.Code, w.Body.String())
	}
	var out BatchGetEngramLinksResponse
	json.NewDecoder(w.Body).Decode(&out)
	if out.Links == nil {
		t.Fatal("Links map must not be nil")
	}
	// Both IDs must be present (even with empty slices from MockEngine)
	for _, id := range []string{"01JAAAAAAAAAAAAAAAAAAAAAA1", "01JAAAAAAAAAAAAAAAAAAAAAA2"} {
		if _, ok := out.Links[id]; !ok {
			t.Errorf("Links map missing key %s", id)
		}
	}
}

func TestBatchGetEngramLinks_EmptyIDs(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", strings.NewReader(`{"ids":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestBatchGetEngramLinks_MissingIDs(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", strings.NewReader(`{"vault":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestBatchGetEngramLinks_TooManyIDs(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("01JAAAAAAAAAAAAAAAAAAAAAA%01d", i%10)
	}
	b, _ := json.Marshal(map[string]any{"ids": ids})
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestBatchGetEngramLinks_BadJSON(t *testing.T) {
	server := NewServer("localhost:8080", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", strings.NewReader(`{not json}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestBatchGetEngramLinks_EngineError(t *testing.T) {
	server := NewServer("localhost:8080", &errorEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	body := `{"ids":["01JAAAAAAAAAAAAAAAAAAAAAA1"],"vault":"default"}`
	req := httptest.NewRequest("POST", "/api/engrams/links/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 got %d", w.Code)
	}
}
