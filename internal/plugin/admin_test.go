package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock PluginStore shared across admin and retroactive tests
// ---------------------------------------------------------------------------

type mockPluginStore struct {
	countResult     int64
	countErr        error
	scanResult      EngramIterator
	setFlagErr      error
	getFlagsResult  uint8
	getFlagsErr     error
	updateEmbedErr  error
	updateDigestErr error
	upsertEntityErr error
	linkErr         error
	coOccurErr      error
	upsertRelErr    error
	hnswInsertErr   error
	autoLinkErr     error

	setFlagCalls      int
	setFlags          []uint8
	updateEmbedCalls  int
	hnswInsertCalls   int
	autoLinkCalls     int
	upsertEntityCalls int
	linkCalls         int
	upsertRelCalls    int
	coOccurCalls      int
}

func (m *mockPluginStore) CountWithoutFlag(_ context.Context, _ uint8) (int64, error) {
	return m.countResult, m.countErr
}

func (m *mockPluginStore) ScanWithoutFlag(_ context.Context, _ uint8) EngramIterator {
	return m.scanResult
}

func (m *mockPluginStore) SetDigestFlag(_ context.Context, _ ULID, flag uint8) error {
	m.setFlagCalls++
	m.setFlags = append(m.setFlags, flag)
	return m.setFlagErr
}

func (m *mockPluginStore) GetDigestFlags(_ context.Context, _ ULID) (uint8, error) {
	return m.getFlagsResult, m.getFlagsErr
}

func (m *mockPluginStore) UpdateEmbedding(_ context.Context, _ ULID, _ []float32) error {
	m.updateEmbedCalls++
	return m.updateEmbedErr
}

func (m *mockPluginStore) UpdateDigest(_ context.Context, _ ULID, _ *EnrichmentResult) error {
	return m.updateDigestErr
}

func (m *mockPluginStore) UpsertEntity(_ context.Context, _ ExtractedEntity) error {
	m.upsertEntityCalls++
	return m.upsertEntityErr
}

func (m *mockPluginStore) LinkEngramToEntity(_ context.Context, _ ULID, _ string) error {
	m.linkCalls++
	return m.linkErr
}

func (m *mockPluginStore) UpsertRelationship(_ context.Context, _ ULID, _ ExtractedRelation) error {
	m.upsertRelCalls++
	return m.upsertRelErr
}

func (m *mockPluginStore) HNSWInsert(_ context.Context, _ ULID, _ []float32) error {
	m.hnswInsertCalls++
	return m.hnswInsertErr
}

func (m *mockPluginStore) AutoLinkByEmbedding(_ context.Context, _ ULID, _ []float32) error {
	m.autoLinkCalls++
	return m.autoLinkErr
}

func (m *mockPluginStore) IncrementEntityCoOccurrence(_ context.Context, _ ULID, _, _ string) error {
	m.coOccurCalls++
	return m.coOccurErr
}

// ---------------------------------------------------------------------------
// Mock EngramIterator
// ---------------------------------------------------------------------------

type mockIterator struct {
	engrams []*Engram
	index   int
	closed  bool
}

func (m *mockIterator) Next() bool {
	if m.index < len(m.engrams) {
		m.index++
		return true
	}
	return false
}

func (m *mockIterator) Engram() *Engram {
	if m.index == 0 || m.index > len(m.engrams) {
		return nil
	}
	return m.engrams[m.index-1]
}

func (m *mockIterator) Close() error {
	m.closed = true
	return nil
}

// ---------------------------------------------------------------------------
// Admin handler tests
// ---------------------------------------------------------------------------

func TestAdminHandler_MethodNotAllowed(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/plugins", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}

	var resp AdminResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.OK {
		t.Error("response should not be OK")
	}
}

func TestAdminHandler_InvalidJSON(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBufferString("{bad json"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_UnknownAction(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "restart"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_AddMissingProvider(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "add", Tier: "embed"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_AddMissingTier(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "add", Provider: "ollama://localhost:11434/model"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_AddInvalidProviderURL(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "add", Tier: "embed", Provider: "badscheme://x"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_AddFactoryError(t *testing.T) {
	factory := func(tier string, cfg PluginConfig) (Plugin, error) {
		return nil, errors.New("factory error")
	}
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, factory)

	body, _ := json.Marshal(AdminRequest{
		Action:   "add",
		Tier:     "embed",
		Provider: "ollama://localhost:11434/model",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected %d, got %d", http.StatusBadGateway, w.Code)
	}
}

func TestAdminHandler_AddEmbedSuccess(t *testing.T) {
	store := &mockPluginStore{countResult: 42}
	factory := func(tier string, cfg PluginConfig) (Plugin, error) {
		return &mockEmbedPlugin{
			mockPlugin: mockPlugin{name: "test-embed", tier: TierEmbed},
		}, nil
	}
	h := NewAdminHandler(NewRegistry(), store, factory)

	body, _ := json.Marshal(AdminRequest{
		Action:   "add",
		Tier:     "embed",
		Provider: "ollama://localhost:11434/model",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, w.Code)
	}

	var resp AdminResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Error("response should be OK")
	}
	if resp.PluginName != "test-embed" {
		t.Errorf("expected plugin name 'test-embed', got %q", resp.PluginName)
	}
	if resp.RetroactiveTotal != 42 {
		t.Errorf("expected retroactive total 42, got %d", resp.RetroactiveTotal)
	}

	// Clean up background processor
	h.mu.Lock()
	if proc, ok := h.procs["test-embed"]; ok {
		proc.Stop()
	}
	h.mu.Unlock()
}

func TestAdminHandler_AddEnrichSuccess(t *testing.T) {
	store := &mockPluginStore{countResult: 10}
	factory := func(tier string, cfg PluginConfig) (Plugin, error) {
		return &mockEnrichPlugin{
			mockPlugin: mockPlugin{name: "test-enrich", tier: TierEnrich},
		}, nil
	}
	h := NewAdminHandler(NewRegistry(), store, factory)

	body, _ := json.Marshal(AdminRequest{
		Action:   "add",
		Tier:     "enrich",
		Provider: "openai://gpt-4",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, w.Code)
	}

	h.mu.Lock()
	if proc, ok := h.procs["test-enrich"]; ok {
		proc.Stop()
	}
	h.mu.Unlock()
}

func TestAdminHandler_AddDuplicatePlugin(t *testing.T) {
	reg := NewRegistry()
	existing := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "dupe", tier: TierEmbed},
	}
	reg.Register(existing)

	factory := func(tier string, cfg PluginConfig) (Plugin, error) {
		return &mockEmbedPlugin{
			mockPlugin: mockPlugin{name: "dupe", tier: TierEmbed},
		}, nil
	}
	h := NewAdminHandler(reg, &mockPluginStore{}, factory)

	body, _ := json.Marshal(AdminRequest{
		Action:   "add",
		Tier:     "embed",
		Provider: "ollama://localhost:11434/model",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected %d, got %d", http.StatusConflict, w.Code)
	}
}

func TestAdminHandler_AddCountError(t *testing.T) {
	store := &mockPluginStore{countErr: errors.New("db error")}
	factory := func(tier string, cfg PluginConfig) (Plugin, error) {
		return &mockEmbedPlugin{
			mockPlugin: mockPlugin{name: "e", tier: TierEmbed},
		}, nil
	}
	h := NewAdminHandler(NewRegistry(), store, factory)

	body, _ := json.Marshal(AdminRequest{
		Action:   "add",
		Tier:     "embed",
		Provider: "ollama://localhost:11434/model",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Should still succeed; count error is non-fatal
	if w.Code != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, w.Code)
	}

	var resp AdminResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.RetroactiveTotal != 0 {
		t.Errorf("expected retroactive total 0 on count error, got %d", resp.RetroactiveTotal)
	}

	h.mu.Lock()
	if proc, ok := h.procs["e"]; ok {
		proc.Stop()
	}
	h.mu.Unlock()
}

func TestAdminHandler_RemoveSuccess(t *testing.T) {
	reg := NewRegistry()
	p := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "removeme", tier: TierEmbed},
	}
	reg.Register(p)

	h := NewAdminHandler(reg, &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "remove", Name: "removeme"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, w.Code)
	}

	var resp AdminResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK {
		t.Error("response should be OK")
	}
}

func TestAdminHandler_RemoveMissingName(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "remove"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestAdminHandler_RemoveNotFound(t *testing.T) {
	h := NewAdminHandler(NewRegistry(), &mockPluginStore{}, nil)

	body, _ := json.Marshal(AdminRequest{Action: "remove", Name: "nonexistent"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected %d, got %d", http.StatusConflict, w.Code)
	}
}

func TestAdminHandler_RemoveStopsProcessor(t *testing.T) {
	reg := NewRegistry()
	store := &mockPluginStore{countResult: 0}

	p := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "with-proc", tier: TierEmbed},
	}
	reg.Register(p)

	h := NewAdminHandler(reg, store, nil)

	// Manually start a processor to verify it's stopped on remove
	proc := NewRetroactiveProcessor(store, p, DigestEmbed)
	ctx, cancel := context.WithCancel(context.Background())
	proc.Start(ctx)
	h.mu.Lock()
	h.procs["with-proc"] = proc
	h.mu.Unlock()

	body, _ := json.Marshal(AdminRequest{Action: "remove", Name: "with-proc"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/plugins", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	cancel()

	if w.Code != http.StatusOK {
		t.Errorf("expected %d, got %d", http.StatusOK, w.Code)
	}

	h.mu.Lock()
	_, exists := h.procs["with-proc"]
	h.mu.Unlock()
	if exists {
		t.Error("processor should be removed from procs map")
	}
}

func TestNewAdminHandler(t *testing.T) {
	reg := NewRegistry()
	store := &mockPluginStore{}
	factory := func(tier string, cfg PluginConfig) (Plugin, error) { return nil, nil }

	h := NewAdminHandler(reg, store, factory)
	if h == nil {
		t.Fatal("NewAdminHandler returned nil")
	}
	if h.registry != reg {
		t.Error("registry not set")
	}
	if h.store != store {
		t.Error("store not set")
	}
	if h.procs == nil {
		t.Error("procs map not initialized")
	}
}
