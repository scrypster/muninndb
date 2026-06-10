package rest

// final_boost_test.go targets specific uncovered branches to push coverage toward 75%.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/plugin"
)

// ---------------------------------------------------------------------------
// handleListAPIKeys — invalid vault name branch (66.7% → higher)
// ---------------------------------------------------------------------------

func TestListAPIKeys_InvalidVaultName_Boost(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	// Query with an invalid vault name (special chars not allowed).
	req := httptest.NewRequest("GET", "/api/admin/keys?vault=INVALID!", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid vault name, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleDeleteVault — ErrVaultJobActive path (81% → higher)
// ---------------------------------------------------------------------------

func TestHandleDeleteVault_JobActive(t *testing.T) {
	eng := &deleteVaultJobActiveEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("DELETE", "/api/admin/vaults/busy-vault", nil)
	req.Header.Set("X-Allow-Default", "true")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for active job, got %d: %s", w.Code, w.Body.String())
	}
}

type deleteVaultJobActiveEngine struct{ MockEngine }

func (e *deleteVaultJobActiveEngine) DeleteVault(_ context.Context, _ string) error {
	return engine.ErrVaultJobActive
}

// ---------------------------------------------------------------------------
// handleObservability — engine error path (77.8% → higher)
// ---------------------------------------------------------------------------

func TestHandleObservability_EngineError(t *testing.T) {
	eng := &observabilityErrorEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/observability", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on observability engine error, got %d: %s", w.Code, w.Body.String())
	}
}

type observabilityErrorEngine struct{ MockEngine }

func (e *observabilityErrorEngine) Observability(_ context.Context, _ string, _ int64) (*engine.ObservabilitySnapshot, error) {
	return nil, errors.New("observability unavailable")
}

// ---------------------------------------------------------------------------
// handleGetEngramLinks — empty id path (80% → higher)
// ---------------------------------------------------------------------------

func TestHandleGetEngramLinks_EmptyID(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams//links", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	server.handleGetEngramLinks(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty engram id, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleRestore — empty id path (77.8% → higher)
// ---------------------------------------------------------------------------

func TestHandleRestore_EmptyID(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams//restore", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	server.handleRestore(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty engram id, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleRetryEnrich — empty id path (77.8% → higher)
// ---------------------------------------------------------------------------

func TestHandleRetryEnrich_EmptyID(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams//retry-enrich", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	server.handleRetryEnrich(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty engram id, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleBackup — missing body path + output_dir already exists path
// ---------------------------------------------------------------------------

func TestHandleBackup_EmptyBody(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Send empty/invalid JSON body.
	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid body, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleBackup_MissingOutputDir(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing output_dir, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleBackup_OutputDirAlreadyExists(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	// Create the output dir before issuing the request.
	existingDir := filepath.Join(t.TempDir(), "already-exists")
	if err := os.MkdirAll(existingDir, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	body := `{"output_dir":"` + existingDir + `"}`
	req := httptest.NewRequest("POST", "/api/admin/backup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for existing output dir, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handlePlugins — non-nil registry with TierEnrich plugin (58.3% → higher)
// ---------------------------------------------------------------------------

func TestHandlePlugins_WithEnrichPlugin(t *testing.T) {
	registry := plugin.NewRegistry()
	mockPlugin := &testEnrichPlugin{name: "test-enricher"}
	if err := registry.Register(mockPlugin); err != nil {
		t.Fatalf("register plugin: %v", err)
	}

	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, registry, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/plugins", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// testEnrichPlugin is a minimal EnrichPlugin for testing.
type testEnrichPlugin struct {
	name string
}

func (p *testEnrichPlugin) Name() string                                        { return p.name }
func (p *testEnrichPlugin) Tier() plugin.PluginTier                             { return plugin.TierEnrich }
func (p *testEnrichPlugin) Init(_ context.Context, _ plugin.PluginConfig) error { return nil }
func (p *testEnrichPlugin) Close() error                                        { return nil }
func (p *testEnrichPlugin) Enrich(_ context.Context, _ *plugin.Engram) (*plugin.EnrichmentResult, error) {
	return &plugin.EnrichmentResult{}, nil
}

// ---------------------------------------------------------------------------
// handleClearVault — vault not found path
// ---------------------------------------------------------------------------

func TestHandleClearVault_NotFound_Boost(t *testing.T) {
	eng := &clearVaultNotFoundEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("DELETE", "/api/admin/vaults/nonexistent/entries", nil)
	req.Header.Set("X-Allow-Default", "true")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for vault not found, got %d: %s", w.Code, w.Body.String())
	}
}

type clearVaultNotFoundEngine struct{ MockEngine }

func (e *clearVaultNotFoundEngine) ClearVault(_ context.Context, _ string) error {
	return engine.ErrVaultNotFound
}
