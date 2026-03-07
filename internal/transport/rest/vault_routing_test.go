package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// vaultTrackingEngine wraps MockEngine and records the vault passed to every engine call
// that accepts a vault parameter.
type vaultTrackingEngine struct {
	MockEngine
	lastWriteVault              string
	lastWriteBatchVault         string
	lastActivateVault           string
	lastListVault               string
	lastReadVault               string
	lastForgetVault             string
	lastLinkVault               string
	lastStatVault               string
	lastGetEngramLinksVault     string
	lastGetBatchEngramLinksVault string
	lastGetSessionVault         string
	lastEvolveVault             string
	lastConsolidateVault        string
	lastDecideVault             string
	lastRestoreVault            string
	lastTraverseVault           string
	lastExplainVault            string
	lastUpdateStateVault        string
	lastUpdateTagsVault         string
	lastListDeletedVault        string
	lastRetryEnrichVault        string
	lastGetContradictionsVault      string
	lastResolveContradictionVault   string
	lastGetGuideVault               string
}

func (e *vaultTrackingEngine) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	e.lastWriteVault = req.Vault
	return e.MockEngine.Write(ctx, req)
}

func (e *vaultTrackingEngine) Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	e.lastActivateVault = req.Vault
	return e.MockEngine.Activate(ctx, req)
}

func (e *vaultTrackingEngine) ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	e.lastListVault = req.Vault
	return e.MockEngine.ListEngrams(ctx, req)
}

func (e *vaultTrackingEngine) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	e.lastReadVault = req.Vault
	return e.MockEngine.Read(ctx, req)
}

func (e *vaultTrackingEngine) Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error) {
	e.lastForgetVault = req.Vault
	return e.MockEngine.Forget(ctx, req)
}

func (e *vaultTrackingEngine) WriteBatch(ctx context.Context, reqs []*WriteRequest) ([]*WriteResponse, []error) {
	if len(reqs) > 0 {
		e.lastWriteBatchVault = reqs[0].Vault
	}
	return e.MockEngine.WriteBatch(ctx, reqs)
}

func (e *vaultTrackingEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*LinkResponse, error) {
	e.lastLinkVault = req.Vault
	return e.MockEngine.Link(ctx, req)
}

func (e *vaultTrackingEngine) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	e.lastStatVault = req.Vault
	return e.MockEngine.Stat(ctx, req)
}

func (e *vaultTrackingEngine) GetEngramLinks(ctx context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	e.lastGetEngramLinksVault = req.Vault
	return e.MockEngine.GetEngramLinks(ctx, req)
}

func (e *vaultTrackingEngine) GetBatchEngramLinks(ctx context.Context, req *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error) {
	e.lastGetBatchEngramLinksVault = req.Vault
	return e.MockEngine.GetBatchEngramLinks(ctx, req)
}

func (e *vaultTrackingEngine) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	e.lastGetSessionVault = req.Vault
	return e.MockEngine.GetSession(ctx, req)
}

func (e *vaultTrackingEngine) Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*EvolveResponse, error) {
	e.lastEvolveVault = vault
	return e.MockEngine.Evolve(ctx, vault, engramID, newContent, reason)
}

func (e *vaultTrackingEngine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResponse, error) {
	e.lastConsolidateVault = vault
	return e.MockEngine.Consolidate(ctx, vault, ids, mergedContent)
}

func (e *vaultTrackingEngine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResponse, error) {
	e.lastDecideVault = vault
	return e.MockEngine.Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
}

func (e *vaultTrackingEngine) Restore(ctx context.Context, vault, engramID string) (*RestoreResponse, error) {
	e.lastRestoreVault = vault
	return e.MockEngine.Restore(ctx, vault, engramID)
}

func (e *vaultTrackingEngine) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResponse, error) {
	e.lastTraverseVault = vault
	return e.MockEngine.Traverse(ctx, vault, req)
}

func (e *vaultTrackingEngine) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResponse, error) {
	e.lastExplainVault = vault
	return e.MockEngine.Explain(ctx, vault, req)
}

func (e *vaultTrackingEngine) UpdateState(ctx context.Context, vault, engramID, state, reason string) error {
	e.lastUpdateStateVault = vault
	return e.MockEngine.UpdateState(ctx, vault, engramID, state, reason)
}

func (e *vaultTrackingEngine) UpdateTags(ctx context.Context, vault, engramID string, tags []string) error {
	e.lastUpdateTagsVault = vault
	return e.MockEngine.UpdateTags(ctx, vault, engramID, tags)
}

func (e *vaultTrackingEngine) ListDeleted(ctx context.Context, vault string, limit int) (*ListDeletedResponse, error) {
	e.lastListDeletedVault = vault
	return e.MockEngine.ListDeleted(ctx, vault, limit)
}

func (e *vaultTrackingEngine) RetryEnrich(ctx context.Context, vault, engramID string) (*RetryEnrichResponse, error) {
	e.lastRetryEnrichVault = vault
	return e.MockEngine.RetryEnrich(ctx, vault, engramID)
}

func (e *vaultTrackingEngine) GetContradictions(ctx context.Context, vault string) (*ContradictionsResponse, error) {
	e.lastGetContradictionsVault = vault
	return e.MockEngine.GetContradictions(ctx, vault)
}

func (e *vaultTrackingEngine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	e.lastResolveContradictionVault = vault
	return e.MockEngine.ResolveContradiction(ctx, vault, idA, idB)
}

func (e *vaultTrackingEngine) GetGuide(ctx context.Context, vault string) (string, error) {
	e.lastGetGuideVault = vault
	return e.MockEngine.GetGuide(ctx, vault)
}

// newVaultTrackingServer creates a Server with a vaultTrackingEngine and a
// public "default" vault. The store is returned so tests can configure auth.
func newVaultTrackingServer(t *testing.T) (*Server, *vaultTrackingEngine, *auth.Store) {
	t.Helper()
	eng := &vaultTrackingEngine{}
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	srv := NewServer("localhost:0", eng, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	return srv, eng, store
}

// TestVaultRouting_Write_DefaultVault verifies that POST /api/engrams with no
// vault param passes "default" to the engine.
func TestVaultRouting_Write_DefaultVault(t *testing.T) {
	srv, eng, _ := newVaultTrackingServer(t)

	body := strings.NewReader(`{"concept":"test","content":"hello"}`)
	req := httptest.NewRequest("POST", "/api/engrams", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastWriteVault != "default" {
		t.Errorf("engine Write vault: want %q, got %q", "default", eng.lastWriteVault)
	}
}

// TestVaultRouting_Write_ExplicitVault verifies that POST /api/engrams?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Write_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"concept":"test","content":"hello"}`)
	req := httptest.NewRequest("POST", "/api/engrams?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastWriteVault != "myvault" {
		t.Errorf("engine Write vault: want %q, got %q", "myvault", eng.lastWriteVault)
	}
}

// TestVaultRouting_Activate_ExplicitVault verifies that POST /api/activate?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Activate_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"context":["something"]}`)
	req := httptest.NewRequest("POST", "/api/activate?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastActivateVault != "myvault" {
		t.Errorf("engine Activate vault: want %q, got %q", "myvault", eng.lastActivateVault)
	}
}

// TestVaultRouting_ListEngrams_ExplicitVault verifies that GET /api/engrams?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_ListEngrams_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastListVault != "myvault" {
		t.Errorf("engine ListEngrams vault: want %q, got %q", "myvault", eng.lastListVault)
	}
}

// TestVaultAuth_LockedVaultRejectedAtEndpoint verifies that a locked vault
// rejects unauthenticated requests with 401 at the endpoint level.
func TestVaultAuth_LockedVaultRejectedAtEndpoint(t *testing.T) {
	srv, _, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "locked", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams?vault=locked", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("locked vault no key: want 401, got %d", w.Code)
	}
}

// TestVaultAuth_ValidKeyGrantsAccess verifies that a valid scoped API key
// passes auth and reaches the engine with the correct vault.
func TestVaultAuth_ValidKeyGrantsAccess(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "secured", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	token, _, err := store.GenerateAPIKey("secured", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams?vault=secured", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid key: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastListVault != "secured" {
		t.Errorf("engine vault: want %q, got %q", "secured", eng.lastListVault)
	}
}

// TestVaultAuth_KeyMismatchRejected verifies that a key scoped to vault-a
// cannot access vault-b, even through the full endpoint path.
func TestVaultAuth_KeyMismatchRejected(t *testing.T) {
	srv, _, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig vault-a: %v", err)
	}
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "vault-b", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig vault-b: %v", err)
	}
	token, _, err := store.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams?vault=vault-b", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("key mismatch: want 401, got %d", w.Code)
	}
}

// TestVaultRouting_Read_ExplicitVault verifies that GET /api/engrams/{id}?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Read_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams/some-id?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// MockEngine.Read returns a valid ReadResponse with nil error; 200 is expected.
	// We care that the vault was correctly forwarded, not the HTTP status.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastReadVault != "myvault" {
		t.Errorf("engine Read vault: want %q, got %q", "myvault", eng.lastReadVault)
	}
}

// TestVaultRouting_Forget_ExplicitVault verifies that DELETE /api/engrams/{id}?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Forget_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/api/engrams/some-id?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastForgetVault != "myvault" {
		t.Errorf("engine Forget vault: want %q, got %q", "myvault", eng.lastForgetVault)
	}
}

// TestVaultRouting_WriteBatch_ExplicitVault verifies that POST /api/engrams/batch?vault=myvault
// passes "myvault" to every item in the batch.
func TestVaultRouting_WriteBatch_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"engrams":[{"concept":"test","content":"hello"}]}`)
	req := httptest.NewRequest("POST", "/api/engrams/batch?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastWriteBatchVault != "myvault" {
		t.Errorf("engine WriteBatch vault: want %q, got %q", "myvault", eng.lastWriteBatchVault)
	}
}

// TestVaultRouting_Link_ExplicitVault verifies that POST /api/link?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Link_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"source_id":"id1","target_id":"id2","rel_type":1}`)
	req := httptest.NewRequest("POST", "/api/link?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastLinkVault != "myvault" {
		t.Errorf("engine Link vault: want %q, got %q", "myvault", eng.lastLinkVault)
	}
}

// TestVaultRouting_Stat_ExplicitVault verifies that GET /api/stats?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Stat_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/stats?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastStatVault != "myvault" {
		t.Errorf("engine Stat vault: want %q, got %q", "myvault", eng.lastStatVault)
	}
}

// TestVaultRouting_GetEngramLinks_ExplicitVault verifies that GET /api/engrams/{id}/links?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_GetEngramLinks_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/engrams/some-id/links?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastGetEngramLinksVault != "myvault" {
		t.Errorf("engine GetEngramLinks vault: want %q, got %q", "myvault", eng.lastGetEngramLinksVault)
	}
}

// TestVaultRouting_GetBatchEngramLinks_ExplicitVault verifies that POST /api/engrams/links/batch?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_GetBatchEngramLinks_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"ids":["id1"]}`)
	req := httptest.NewRequest("POST", "/api/engrams/links/batch?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastGetBatchEngramLinksVault != "myvault" {
		t.Errorf("engine GetBatchEngramLinks vault: want %q, got %q", "myvault", eng.lastGetBatchEngramLinksVault)
	}
}

// TestVaultRouting_GetSession_ExplicitVault verifies that GET /api/session?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_GetSession_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/session?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastGetSessionVault != "myvault" {
		t.Errorf("engine GetSession vault: want %q, got %q", "myvault", eng.lastGetSessionVault)
	}
}

// TestVaultRouting_Evolve_ExplicitVault verifies that POST /api/engrams/{id}/evolve?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Evolve_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"new_content":"updated","reason":"improvement"}`)
	req := httptest.NewRequest("POST", "/api/engrams/some-id/evolve?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastEvolveVault != "myvault" {
		t.Errorf("engine Evolve vault: want %q, got %q", "myvault", eng.lastEvolveVault)
	}
}

// TestVaultRouting_Consolidate_ExplicitVault verifies that POST /api/consolidate?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Consolidate_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"ids":["id1","id2"],"merged_content":"merged"}`)
	req := httptest.NewRequest("POST", "/api/consolidate?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastConsolidateVault != "myvault" {
		t.Errorf("engine Consolidate vault: want %q, got %q", "myvault", eng.lastConsolidateVault)
	}
}

// TestVaultRouting_Decide_ExplicitVault verifies that POST /api/decide?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Decide_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"decision":"use postgres","rationale":"proven reliability"}`)
	req := httptest.NewRequest("POST", "/api/decide?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastDecideVault != "myvault" {
		t.Errorf("engine Decide vault: want %q, got %q", "myvault", eng.lastDecideVault)
	}
}

// TestVaultRouting_Restore_ExplicitVault verifies that POST /api/engrams/{id}/restore?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Restore_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/engrams/some-id/restore?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastRestoreVault != "myvault" {
		t.Errorf("engine Restore vault: want %q, got %q", "myvault", eng.lastRestoreVault)
	}
}

// TestVaultRouting_Traverse_ExplicitVault verifies that POST /api/traverse?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Traverse_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"start_id":"root-id"}`)
	req := httptest.NewRequest("POST", "/api/traverse?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastTraverseVault != "myvault" {
		t.Errorf("engine Traverse vault: want %q, got %q", "myvault", eng.lastTraverseVault)
	}
}

// TestVaultRouting_Explain_ExplicitVault verifies that POST /api/explain?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_Explain_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"engram_id":"some-id"}`)
	req := httptest.NewRequest("POST", "/api/explain?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastExplainVault != "myvault" {
		t.Errorf("engine Explain vault: want %q, got %q", "myvault", eng.lastExplainVault)
	}
}

// TestVaultRouting_UpdateState_ExplicitVault verifies that PUT /api/engrams/{id}/state?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_UpdateState_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"state":"active"}`)
	req := httptest.NewRequest("PUT", "/api/engrams/some-id/state?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastUpdateStateVault != "myvault" {
		t.Errorf("engine UpdateState vault: want %q, got %q", "myvault", eng.lastUpdateStateVault)
	}
}

// TestVaultRouting_UpdateTags_ExplicitVault verifies that PUT /api/engrams/{id}/tags?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_UpdateTags_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	body := strings.NewReader(`{"tags":["a","b"]}`)
	req := httptest.NewRequest("PUT", "/api/engrams/some-id/tags?vault=myvault", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastUpdateTagsVault != "myvault" {
		t.Errorf("engine UpdateTags vault: want %q, got %q", "myvault", eng.lastUpdateTagsVault)
	}
}

// TestVaultRouting_ListDeleted_ExplicitVault verifies that GET /api/deleted?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_ListDeleted_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/deleted?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastListDeletedVault != "myvault" {
		t.Errorf("engine ListDeleted vault: want %q, got %q", "myvault", eng.lastListDeletedVault)
	}
}

// TestVaultRouting_RetryEnrich_ExplicitVault verifies that POST /api/engrams/{id}/retry-enrich?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_RetryEnrich_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/engrams/some-id/retry-enrich?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastRetryEnrichVault != "myvault" {
		t.Errorf("engine RetryEnrich vault: want %q, got %q", "myvault", eng.lastRetryEnrichVault)
	}
}

// TestVaultRouting_GetContradictions_ExplicitVault verifies that GET /api/contradictions?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_GetContradictions_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/contradictions?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastGetContradictionsVault != "myvault" {
		t.Errorf("engine GetContradictions vault: want %q, got %q", "myvault", eng.lastGetContradictionsVault)
	}
}

// TestVaultRouting_GetGuide_ExplicitVault verifies that GET /api/guide?vault=myvault
// passes "myvault" to the engine.
func TestVaultRouting_GetGuide_ExplicitVault(t *testing.T) {
	srv, eng, store := newVaultTrackingServer(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/guide?vault=myvault", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastGetGuideVault != "myvault" {
		t.Errorf("engine GetGuide vault: want %q, got %q", "myvault", eng.lastGetGuideVault)
	}
}

// TestVaultRouting_ResolveContradiction_ExplicitVault verifies that
// POST /api/admin/contradictions/resolve passes the vault from the request body to the engine.
// Note: this is an admin endpoint; vault is not set via ?vault= query param but via the body's
// "vault" field, since withAdminMiddleware does not run VaultAuthMiddleware.
func TestVaultRouting_ResolveContradiction_ExplicitVault(t *testing.T) {
	srv, eng, _ := newVaultTrackingServer(t)

	// sessionSecret is "" in the test server, so admin auth is skipped.
	body := strings.NewReader(`{"vault":"myvault","id_a":"a1","id_b":"b1"}`)
	req := httptest.NewRequest("POST", "/api/admin/contradictions/resolve", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if eng.lastResolveContradictionVault != "myvault" {
		t.Errorf("engine ResolveContradiction vault: want %q, got %q", "myvault", eng.lastResolveContradictionVault)
	}
}
