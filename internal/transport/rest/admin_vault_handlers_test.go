package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
)

// ---------------------------------------------------------------------------
// Custom engines for vault lifecycle handler tests
// ---------------------------------------------------------------------------

// vaultErrEngine overrides ClearVault and DeleteVault to return a configurable error.
type vaultErrEngine struct {
	MockEngine
	clearErr  error
	deleteErr error
}

func (e *vaultErrEngine) ClearVault(_ context.Context, _ string) error  { return e.clearErr }
func (e *vaultErrEngine) DeleteVault(_ context.Context, _ string) error { return e.deleteErr }

// cloneErrEngine overrides StartClone to return a configurable error.
type cloneErrEngine struct {
	MockEngine
	err error
}

func (e *cloneErrEngine) StartClone(_ context.Context, _, _ string) (*vaultjob.Job, error) {
	return nil, e.err
}

// mergeErrEngine overrides StartMerge to return a configurable error.
type mergeErrEngine struct {
	MockEngine
	err error
}

func (e *mergeErrEngine) StartMerge(_ context.Context, _, _ string, _ bool) (*vaultjob.Job, error) {
	return nil, e.err
}

// jobEngine overrides GetVaultJob to return a configurable job.
type jobEngine struct {
	MockEngine
	job *vaultjob.Job
	ok  bool
}

func (e *jobEngine) GetVaultJob(_ string) (*vaultjob.Job, bool) { return e.job, e.ok }

// newVaultTestServer builds a Server with no session secret (admin middleware passes through).
func newVaultTestServer(engine EngineAPI) *Server {
	return NewServer("localhost:0", engine, nil, nil, nil, EmbedInfo{}, nil, "", nil)
}

// serveVault is a shorthand to dispatch a request through the mux and return the recorder.
func serveVault(srv *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// handleDeleteVault
// ---------------------------------------------------------------------------

func TestHandleDeleteVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "DELETE", "/api/admin/vaults/myvault", nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteVault_Default_NoHeader(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "DELETE", "/api/admin/vaults/default", nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 without X-Allow-Default, got %d", w.Code)
	}
}

func TestHandleDeleteVault_Default_WithHeader(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "DELETE", "/api/admin/vaults/default", nil, map[string]string{"X-Allow-Default": "true"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 with X-Allow-Default, got %d", w.Code)
	}
}

func TestHandleDeleteVault_NotFound(t *testing.T) {
	eng := &vaultErrEngine{deleteErr: fmt.Errorf("vault %q: %w", "missing", engine.ErrVaultNotFound)}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "DELETE", "/api/admin/vaults/missing", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteVault_StorageError(t *testing.T) {
	eng := &vaultErrEngine{deleteErr: fmt.Errorf("pebble: io error")}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "DELETE", "/api/admin/vaults/myvault", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleClearVault
// ---------------------------------------------------------------------------

func TestHandleClearVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "POST", "/api/admin/vaults/myvault/clear", nil, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleClearVault_Default_NoHeader(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "POST", "/api/admin/vaults/default/clear", nil, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 without X-Allow-Default, got %d", w.Code)
	}
}

func TestHandleClearVault_Default_WithHeader(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "POST", "/api/admin/vaults/default/clear", nil, map[string]string{"X-Allow-Default": "true"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 with X-Allow-Default, got %d", w.Code)
	}
}

func TestHandleClearVault_NotFound(t *testing.T) {
	eng := &vaultErrEngine{clearErr: fmt.Errorf("vault %q: %w", "missing", engine.ErrVaultNotFound)}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "POST", "/api/admin/vaults/missing/clear", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleCloneVault
// ---------------------------------------------------------------------------

func TestHandleCloneVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]string{"new_name": "vault-copy"})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/clone", body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Fatal("expected non-empty job_id in response")
	}
}

func TestHandleCloneVault_EmptyNewName(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]string{"new_name": ""})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/clone", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty new_name, got %d", w.Code)
	}
}

func TestHandleCloneVault_NoBody(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/clone", []byte("not json"), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid body, got %d", w.Code)
	}
}

func TestHandleCloneVault_SelfClone(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]string{"new_name": "source"})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/clone", body, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for self-clone, got %d", w.Code)
	}
}

func TestHandleCloneVault_SourceNotFound(t *testing.T) {
	eng := &cloneErrEngine{err: fmt.Errorf("source vault %q: %w", "missing", engine.ErrVaultNotFound)}
	srv := newVaultTestServer(eng)
	body, _ := json.Marshal(map[string]string{"new_name": "copy"})
	w := serveVault(srv, "POST", "/api/admin/vaults/missing/clone", body, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCloneVault_EngineError(t *testing.T) {
	eng := &cloneErrEngine{err: fmt.Errorf("too many concurrent vault jobs (max 4)")}
	srv := newVaultTestServer(eng)
	body, _ := json.Marshal(map[string]string{"new_name": "copy"})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/clone", body, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleMergeVault
// ---------------------------------------------------------------------------

func TestHandleMergeVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]interface{}{"target": "dest-vault", "delete_source": false})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/merge-into", body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Fatal("expected non-empty job_id in response")
	}
}

func TestHandleMergeVault_DeleteSource(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]interface{}{"target": "dest-vault", "delete_source": true})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/merge-into", body, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 with delete_source=true, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleMergeVault_EmptyTarget(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]interface{}{"target": "", "delete_source": false})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/merge-into", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty target, got %d", w.Code)
	}
}

func TestHandleMergeVault_SameSourceTarget(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body, _ := json.Marshal(map[string]interface{}{"target": "source", "delete_source": false})
	w := serveVault(srv, "POST", "/api/admin/vaults/source/merge-into", body, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for source==target, got %d", w.Code)
	}
}

func TestHandleMergeVault_SourceNotFound(t *testing.T) {
	eng := &mergeErrEngine{err: fmt.Errorf("source vault %q: %w", "missing", engine.ErrVaultNotFound)}
	srv := newVaultTestServer(eng)
	body, _ := json.Marshal(map[string]interface{}{"target": "dest", "delete_source": false})
	w := serveVault(srv, "POST", "/api/admin/vaults/missing/merge-into", body, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleVaultJobStatus
// ---------------------------------------------------------------------------

func TestHandleVaultJobStatus_MissingJobID(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "GET", "/api/admin/vaults/myvault/job-status", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing job_id, got %d", w.Code)
	}
}

func TestHandleVaultJobStatus_JobNotFound(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{}) // MockEngine.GetVaultJob returns (nil, false)
	w := serveVault(srv, "GET", "/api/admin/vaults/myvault/job-status?job_id=999", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVaultJobStatus_Success_ViaSource(t *testing.T) {
	job := &vaultjob.Job{ID: "42", Operation: "clone", Source: "vault-a", Target: "vault-b"}
	eng := &jobEngine{job: job, ok: true}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "GET", "/api/admin/vaults/vault-a/job-status?job_id=42", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var snap vaultjob.StatusSnapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.JobID != "42" {
		t.Fatalf("expected job_id 42, got %q", snap.JobID)
	}
	if snap.Operation != "clone" {
		t.Fatalf("expected operation clone, got %q", snap.Operation)
	}
}

func TestHandleVaultJobStatus_Success_ViaTarget(t *testing.T) {
	job := &vaultjob.Job{ID: "7", Operation: "clone", Source: "vault-a", Target: "vault-b"}
	eng := &jobEngine{job: job, ok: true}
	srv := newVaultTestServer(eng)
	// Querying via the target vault name should also work (scope includes both source and target).
	w := serveVault(srv, "GET", "/api/admin/vaults/vault-b/job-status?job_id=7", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when querying via target vault, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVaultJobStatus_WrongVaultScope(t *testing.T) {
	// Job belongs to vault-a → vault-b; querying via vault-c should return 404.
	job := &vaultjob.Job{ID: "5", Operation: "clone", Source: "vault-a", Target: "vault-b"}
	eng := &jobEngine{job: job, ok: true}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "GET", "/api/admin/vaults/vault-c/job-status?job_id=5", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for wrong vault scope, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleExportVault
// ---------------------------------------------------------------------------

func TestHandleExportVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "GET", "/api/admin/vaults/myvault/export", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if cd == "" {
		t.Error("expected Content-Disposition header")
	}
}

func TestHandleExportVault_InvalidName(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "GET", "/api/admin/vaults/INVALID_CAPS/export", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleImportVault
// ---------------------------------------------------------------------------

func TestHandleImportVault_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body := []byte("fake-archive-data")
	req := httptest.NewRequest("POST", "/api/admin/vaults/import?vault=newvault", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job_id"] == "" {
		t.Fatal("expected non-empty job_id in response")
	}
}

func TestHandleImportVault_MissingVaultName(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	body := []byte("fake-archive-data")
	req := httptest.NewRequest("POST", "/api/admin/vaults/import", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing vault name, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleExportVaultMarkdown
// ---------------------------------------------------------------------------

func TestHandleExportVaultMarkdown_Success(t *testing.T) {
	srv := newVaultTestServer(&MockEngine{})
	w := serveVault(srv, "GET", "/api/admin/vaults/testvault/export-markdown", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "testvault.markdown.tgz") {
		t.Errorf("expected Content-Disposition with testvault.markdown.tgz, got %q", cd)
	}
}

func TestHandleExportVaultMarkdown_NotFound(t *testing.T) {
	eng := &markdownExportNotFoundEngine{}
	srv := newVaultTestServer(eng)
	w := serveVault(srv, "GET", "/api/admin/vaults/missing/export-markdown", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// markdownExportNotFoundEngine returns ErrVaultNotFound for ListEngrams.
type markdownExportNotFoundEngine struct {
	MockEngine
}

func (e *markdownExportNotFoundEngine) ListEngrams(_ context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	return nil, fmt.Errorf("vault %q: %w", req.Vault, engine.ErrVaultNotFound)
}
