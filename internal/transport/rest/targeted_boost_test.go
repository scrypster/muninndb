package rest

// targeted_boost_test.go covers specific uncovered branches to maximize coverage.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/plugin"
)

// ---------------------------------------------------------------------------
// handleGetEngram — missing id empty path (77.8% → higher)
// ---------------------------------------------------------------------------

func TestHandleGetEngram_EmptyID(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/engrams/x", nil)
	req.SetPathValue("id", "") // force empty
	w := httptest.NewRecorder()
	server.handleGetEngram(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty engram id, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleDeleteEngram — force empty id via SetPathValue (handler direct call)
// ---------------------------------------------------------------------------

func TestHandleDeleteEngram_EmptyID_Direct(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("DELETE", "/api/engrams/x", nil)
	req.SetPathValue("id", "")
	w := httptest.NewRecorder()
	server.handleDeleteEngram(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty engram id, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleMCPInfo — fallback path when SplitHostPort fails (81.8% → higher)
// ---------------------------------------------------------------------------

func TestHandleMCPInfo_WithMalformedAddr(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	// A malformed mcpAddr that will make net.SplitHostPort fail.
	server.mcpAddr = "not-a-valid-addr:with:too:many:colons"

	req := httptest.NewRequest("GET", "/api/admin/mcp", nil)
	w := httptest.NewRecorder()
	server.handleMCPInfo(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 even with malformed addr, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMCPInfo_WithWildcardHost(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	server.mcpAddr = "0.0.0.0:8750"

	req := httptest.NewRequest("GET", "/api/admin/mcp", nil)
	w := httptest.NewRecorder()
	server.handleMCPInfo(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for wildcard host, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleCloneVault — same name as source (returns 409 conflict)
// ---------------------------------------------------------------------------

func TestHandleCloneVault_SameNameAsSource(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewReader([]byte(`{"new_name":"source-vault"}`))
	req := httptest.NewRequest("POST", "/api/admin/vaults/source-vault/clone", body)
	req.SetPathValue("name", "source-vault")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleCloneVault(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for clone onto itself, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handlePutPluginConfig — success path (81.8% → higher)
// ---------------------------------------------------------------------------

func TestHandlePutPluginConfig_Success_Targeted(t *testing.T) {
	eng := &MockEngine{}
	dataDir := t.TempDir()
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dataDir, nil)

	body := bytes.NewReader([]byte(`{"embed":{"provider_url":"ollama://localhost:11434/test"}}`))
	req := httptest.NewRequest("PUT", "/api/admin/plugins/config", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handlePutPluginConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for valid config, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// recoveryMiddleware — ErrAbortHandler is re-panicked (87.5% → higher)
// ---------------------------------------------------------------------------

func TestRecoveryMiddleware_AbortHandlerRepanics(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	abortHandler := server.recoveryMiddleware(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("expected ErrAbortHandler re-panic, got %v", r)
		}
	}()

	abortHandler(w, req)
}

// ---------------------------------------------------------------------------
// handlePlugins — TierEmbed branch (83.3% → higher)
// ---------------------------------------------------------------------------

func TestHandlePlugins_WithEmbedPlugin(t *testing.T) {
	registry := plugin.NewRegistry()
	mockEmbed := &testEmbedPlugin{name: "test-embedder"}
	if err := registry.Register(mockEmbed); err != nil {
		t.Fatalf("register embed plugin: %v", err)
	}

	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, registry, "", nil)
	server.embedProvider = "test-provider"
	server.embedModel = "test-model"

	req := httptest.NewRequest("GET", "/api/admin/plugins", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// testEmbedPlugin is a minimal EmbedPlugin for testing.
type testEmbedPlugin struct {
	name string
}

func (p *testEmbedPlugin) Name() string                                        { return p.name }
func (p *testEmbedPlugin) Tier() plugin.PluginTier                             { return plugin.TierEmbed }
func (p *testEmbedPlugin) Init(_ context.Context, _ plugin.PluginConfig) error { return nil }
func (p *testEmbedPlugin) Close() error                                        { return nil }
func (p *testEmbedPlugin) Embed(_ context.Context, _ []string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}
func (p *testEmbedPlugin) Dimension() int    { return 2 }
func (p *testEmbedPlugin) MaxBatchSize() int { return 32 }

// ---------------------------------------------------------------------------
// HandleReplicationStatus/Lag/Promote — exported wrappers (0% → covered)
// ---------------------------------------------------------------------------

func TestHandleReplicationStatus_Exported(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/v1/replication/status", nil)
	w := httptest.NewRecorder()
	server.HandleReplicationStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (coordinator=nil returns disabled), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleReplicationLag_Exported(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/v1/replication/lag", nil)
	w := httptest.NewRecorder()
	server.HandleReplicationLag(w, req)

	// coordinator is nil → 503 ServiceUnavailable
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (cluster disabled), got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleSubscribe — SSE handler with context cancellation (0% → covered)
// This covers ~50 statements in the SSE subscribe path.
// ---------------------------------------------------------------------------

func TestHandleSubscribe_ContextCancelExit(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/subscribe?vault=default", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleSubscribe(w, req)
	}()

	// Cancel the context to let the SSE handler exit.
	cancel()
	<-done

	// The handler sets SSE headers and returns on context cancellation.
	contentType := w.Header().Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", contentType)
	}
}

func TestHandleSubscribe_WithParams(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	// Test with various query params to cover parameter parsing branches.
	req := httptest.NewRequest("GET", "/api/subscribe?vault=myvault&threshold=0.7&ttl=60&rate=5&on_write=true", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleSubscribe(w, req)
	}()

	cancel()
	<-done
}

func TestHandleSubscribe_InvalidParams(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	// Test with out-of-range threshold, negative ttl, and out-of-range rate.
	req := httptest.NewRequest("GET", "/api/subscribe?threshold=2.0&ttl=-10&rate=9999", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.handleSubscribe(w, req)
	}()

	cancel()
	<-done
}

func TestHandlePromoteReplica_Exported(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/v1/replication/promote", nil)
	w := httptest.NewRecorder()
	server.HandlePromoteReplica(w, req)

	// coordinator is nil → 503 ServiceUnavailable
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 (cluster disabled), got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ctxVault — vault-in-context path (66.7% → higher)
// ---------------------------------------------------------------------------

func TestCtxVault_WithVaultInContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), auth.ContextVault, "my-vault")
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	got := ctxVault(req)
	if got != "my-vault" {
		t.Errorf("expected 'my-vault', got %q", got)
	}
}

func TestCtxVault_WithEmptyContextValue(t *testing.T) {
	ctx := context.WithValue(context.Background(), auth.ContextVault, "")
	req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	got := ctxVault(req)
	if got != "default" {
		t.Errorf("expected 'default' for empty vault, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// handleConsolidate (consolidation_handlers.go) — empty vault path
// ---------------------------------------------------------------------------

func TestHandleConsolidate_EmptyVault(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	handler := server.handleConsolidate()
	req := httptest.NewRequest("POST", "/v1/vaults//consolidate", strings.NewReader(`{}`))
	req.SetPathValue("vault", "")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty vault, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleConsolidate_InvalidJSON_Targeted(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	handler := server.handleConsolidate()
	req := httptest.NewRequest("POST", "/v1/vaults/default/consolidate", strings.NewReader("{bad"))
	req.SetPathValue("vault", "default")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleEvolve — missing new_content or reason (87.5% → higher)
// ---------------------------------------------------------------------------

func TestHandleEvolve_MissingContent(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewReader([]byte(`{"new_content":"","reason":""}`))
	req := httptest.NewRequest("PUT", "/api/engrams/some-id/evolve", body)
	req.SetPathValue("id", "some-id")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleEvolve(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing content/reason, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleGetSession — with valid since param (94.1% → higher)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// handlePutVaultPlasticity — empty preset defaults to "default" (85.7% → higher)
// ---------------------------------------------------------------------------

func TestPutVaultPlasticity_EmptyPresetDefaultsToDefault(t *testing.T) {
	as := newTestAuthStore(t)
	server := newTestServer(t, as)

	// Omit preset entirely — should default to "default" and succeed.
	body := bytes.NewReader([]byte(`{}`))
	req := httptest.NewRequest("PUT", "/api/admin/vault/myvault/plasticity", body)
	req.SetPathValue("name", "myvault")
	w := httptest.NewRecorder()
	server.handlePutVaultPlasticity(as)(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for empty preset (defaults to 'default'), got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleSetVaultConfig — invalid vault name (85.7% → higher)
// ---------------------------------------------------------------------------

func TestSetVaultConfig_InvalidVaultName_Boost(t *testing.T) {
	as := newTestAuthStore(t)
	server := newTestServer(t, as)

	body := bytes.NewReader([]byte(`{"name":"INVALID!"}`))
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid vault name, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleGetSession — with valid since param (94.1% → higher)
// ---------------------------------------------------------------------------

func TestHandleGetSession_WithSinceParam(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/session?since=2026-01-01T00:00:00Z&limit=10", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleCreateAPIKey — label too long + invalid mode (89.3% → higher)
// ---------------------------------------------------------------------------

func TestCreateAPIKey_NoVaultDefaultsToDefault(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	// No "vault" field — should default to "default" and succeed.
	body := bytes.NewReader([]byte(`{"label":"no-vault-key","mode":"full"}`))
	req := httptest.NewRequest("POST", "/api/admin/keys", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 when vault defaults to 'default', got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAPIKey_LabelTooLong_Targeted(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	longLabel := strings.Repeat("x", 257)
	body := bytes.NewReader([]byte(`{"vault":"default","label":"` + longLabel + `","mode":"full"}`))
	req := httptest.NewRequest("POST", "/api/admin/keys", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for label too long, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateAPIKey_InvalidMode(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body := bytes.NewReader([]byte(`{"vault":"default","label":"test","mode":"invalid-mode"}`))
	req := httptest.NewRequest("POST", "/api/admin/keys", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid mode, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleMergeVault — invalid target name + same source=target (91.7% → higher)
// ---------------------------------------------------------------------------

func TestHandleMergeVault_InvalidTargetName(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewReader([]byte(`{"target":"INVALID!"}`))
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/merge-into", body)
	req.SetPathValue("name", "source")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleMergeVault(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid target name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMergeVault_SameSourceTarget_Targeted(t *testing.T) {
	eng := &MockEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewReader([]byte(`{"target":"source"}`))
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/merge-into", body)
	req.SetPathValue("name", "source")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.handleMergeVault(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for same source=target, got %d: %s", w.Code, w.Body.String())
	}
}
