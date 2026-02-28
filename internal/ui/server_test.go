package ui_test

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/logging"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/transport/rest"
	"github.com/scrypster/muninndb/internal/ui"
)

// mockEngine satisfies rest.EngineAPI for testing.
type mockEngine struct{}

func (m *mockEngine) Hello(ctx context.Context, req *rest.HelloRequest) (*rest.HelloResponse, error) {
	return &rest.HelloResponse{}, nil
}

func (m *mockEngine) Write(ctx context.Context, req *rest.WriteRequest) (*rest.WriteResponse, error) {
	return &rest.WriteResponse{}, nil
}

func (m *mockEngine) WriteBatch(ctx context.Context, reqs []*rest.WriteRequest) ([]*rest.WriteResponse, []error) {
	responses := make([]*rest.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i := range reqs {
		responses[i] = &rest.WriteResponse{}
	}
	return responses, errs
}

func (m *mockEngine) Read(ctx context.Context, req *rest.ReadRequest) (*rest.ReadResponse, error) {
	return &rest.ReadResponse{}, nil
}

func (m *mockEngine) Activate(ctx context.Context, req *rest.ActivateRequest) (*rest.ActivateResponse, error) {
	return &rest.ActivateResponse{}, nil
}

func (m *mockEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*rest.LinkResponse, error) {
	return &rest.LinkResponse{}, nil
}

func (m *mockEngine) Forget(ctx context.Context, req *rest.ForgetRequest) (*rest.ForgetResponse, error) {
	return &rest.ForgetResponse{}, nil
}

func (m *mockEngine) Stat(ctx context.Context, req *rest.StatRequest) (*rest.StatResponse, error) {
	return &rest.StatResponse{}, nil
}

func (m *mockEngine) ListEngrams(ctx context.Context, req *rest.ListEngramsRequest) (*rest.ListEngramsResponse, error) {
	return &rest.ListEngramsResponse{Engrams: []rest.EngramItem{}}, nil
}

func (m *mockEngine) GetEngramLinks(ctx context.Context, req *rest.GetEngramLinksRequest) (*rest.GetEngramLinksResponse, error) {
	return &rest.GetEngramLinksResponse{Links: []rest.AssociationItem{}}, nil
}

func (m *mockEngine) ListVaults(ctx context.Context) ([]string, error) {
	return []string{"default"}, nil
}

func (m *mockEngine) GetSession(ctx context.Context, req *rest.GetSessionRequest) (*rest.GetSessionResponse, error) {
	return &rest.GetSessionResponse{Entries: []rest.SessionItem{}}, nil
}

func (m *mockEngine) WorkerStats() cognitive.EngineWorkerStats {
	return cognitive.EngineWorkerStats{}
}

func (m *mockEngine) SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	return "mock-sub", nil
}

func (m *mockEngine) Unsubscribe(ctx context.Context, subID string) error {
	return nil
}

func (m *mockEngine) ClearVault(ctx context.Context, vaultName string) error  { return nil }
func (m *mockEngine) DeleteVault(ctx context.Context, vaultName string) error { return nil }
func (m *mockEngine) RenameVault(ctx context.Context, oldName, newName string) error {
	return nil
}
func (m *mockEngine) GetVaultJob(jobID string) (*vaultjob.Job, bool)          { return nil, false }
func (m *mockEngine) StartClone(ctx context.Context, sourceVault, newName string) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-clone-job", Operation: "clone", Source: sourceVault, Target: newName}, nil
}
func (m *mockEngine) StartMerge(ctx context.Context, sourceVault, targetVault string, deleteSource bool) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-merge-job", Operation: "merge", Source: sourceVault, Target: targetVault}, nil
}
func (m *mockEngine) ExportVault(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, w io.Writer) (*storage.ExportResult, error) {
	return &storage.ExportResult{}, nil
}
func (m *mockEngine) StartImport(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, r io.Reader) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-import-job", Operation: "import", Target: vaultName}, nil
}

func (m *mockEngine) ReindexFTSVault(ctx context.Context, vaultName string) (int64, error) {
	return 0, nil
}

func (m *mockEngine) Checkpoint(destDir string) error {
	return nil
}

func (m *mockEngine) Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*rest.EvolveResponse, error) {
	return &rest.EvolveResponse{ID: "evolved-id"}, nil
}
func (m *mockEngine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*rest.ConsolidateResponse, error) {
	return &rest.ConsolidateResponse{ID: "consolidated-id", Archived: ids}, nil
}
func (m *mockEngine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*rest.DecideResponse, error) {
	return &rest.DecideResponse{ID: "decision-id"}, nil
}
func (m *mockEngine) Restore(ctx context.Context, vault, engramID string) (*rest.RestoreResponse, error) {
	return &rest.RestoreResponse{ID: engramID, Concept: "restored", Restored: true, State: "active"}, nil
}
func (m *mockEngine) Traverse(ctx context.Context, vault string, req *rest.TraverseRequest) (*rest.TraverseResponse, error) {
	return &rest.TraverseResponse{}, nil
}
func (m *mockEngine) Explain(ctx context.Context, vault string, req *rest.ExplainRequest) (*rest.ExplainResponse, error) {
	return &rest.ExplainResponse{}, nil
}
func (m *mockEngine) UpdateState(ctx context.Context, vault, engramID, state, reason string) error {
	return nil
}
func (m *mockEngine) UpdateTags(ctx context.Context, vault, engramID string, tags []string) error {
	return nil
}
func (m *mockEngine) ListDeleted(ctx context.Context, vault string, limit int) (*rest.ListDeletedResponse, error) {
	return &rest.ListDeletedResponse{}, nil
}
func (m *mockEngine) RetryEnrich(ctx context.Context, vault, engramID string) (*rest.RetryEnrichResponse, error) {
	return &rest.RetryEnrichResponse{}, nil
}
func (m *mockEngine) GetContradictions(ctx context.Context, vault string) (*rest.ContradictionsResponse, error) {
	return &rest.ContradictionsResponse{}, nil
}

func (m *mockEngine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	return nil
}
func (m *mockEngine) GetGuide(ctx context.Context, vault string) (string, error) {
	return "", nil
}

func (m *mockEngine) StartReembedVault(ctx context.Context, vaultName, modelName string) (*vaultjob.Job, error) {
	return &vaultjob.Job{ID: "mock-reembed-job", Operation: "reembed", Source: vaultName, Target: vaultName}, nil
}

func (m *mockEngine) CountEmbedded(ctx context.Context) int64 {
	return 0
}

func (m *mockEngine) Observability(ctx context.Context, version string, uptimeSeconds int64) (*engine.ObservabilitySnapshot, error) {
	return &engine.ObservabilitySnapshot{}, nil
}

func (m *mockEngine) GetProcessorStats() []plugin.RetroactiveStats {
	return nil
}

func makeMockFS() fs.FS {
	return fstest.MapFS{
		"static/dist/app.css":   &fstest.MapFile{Data: []byte("/* css */")},
		"static/logo.jpg":       &fstest.MapFile{Data: []byte("img")},
		"templates/index.html":  &fstest.MapFile{Data: []byte("<html><body>MuninnDB</body></html>")},
	}
}

func TestNewServer(t *testing.T) {
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv == nil {
		t.Fatal("expected non-nil server")
	}
}

func TestSPAHandler(t *testing.T) {
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty body")
	}
}

func TestStaticHandler(t *testing.T) {
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/static/dist/app.css", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for static file, got %d", w.Code)
	}
}

func TestSPAHandlerNonRoot(t *testing.T) {
	// All non-static paths should serve index.html (SPA catch-all)
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	paths := []string{"/dashboard", "/memories", "/graph", "/session", "/anything"}
	for _, p := range paths {
		req := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("path %s: expected 200, got %d", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), "MuninnDB") {
			t.Errorf("path %s: expected index.html content, got %q", p, w.Body.String())
		}
	}
}

func TestSSEResponseHeaders(t *testing.T) {
	// Test SSE headers using a real httptest server (needs actual streaming)
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Use a context with short timeout — we just want to check response headers
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Context deadline is fine — we already got the response headers
		if resp == nil {
			t.Skipf("could not establish SSE connection: %v", err)
			return
		}
	}
	if resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected 200 for SSE, got %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Errorf("expected Content-Type: text/event-stream, got %q", ct)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" {
			t.Errorf("expected Cache-Control: no-cache, got %q", resp.Header.Get("Cache-Control"))
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("expected Access-Control-Allow-Origin: *, got %q", resp.Header.Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestServerStartStop(t *testing.T) {
	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx, "127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestSPAHandlerMissingIndex(t *testing.T) {
	// FS with no index.html in templates/ — SPA handler should return 404
	badFS := fstest.MapFS{
		"static/dist/app.css": &fstest.MapFile{Data: []byte("/* css */")},
		"templates/.keep":     &fstest.MapFile{Data: []byte("")},
	}
	eng := &mockEngine{}
	srv, err := ui.NewServer(badFS, eng, http.NotFoundHandler(), nil, nil, logging.NewRingBuffer(10, nil), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when index.html missing, got %d", w.Code)
	}
}

// statsMockEngine returns configurable stat values for broadcaster tests.
type statsMockEngine struct {
	mockEngine
	engramCount int64
}

func (e *statsMockEngine) Stat(ctx context.Context, req *rest.StatRequest) (*rest.StatResponse, error) {
	return &rest.StatResponse{EngramCount: e.engramCount, VaultCount: 1}, nil
}

func TestHandleLogs_ReturnsSnapshot(t *testing.T) {
	rb := logging.NewRingBuffer(10, nil)
	rb.Add(logging.LogEntry{Level: "INFO", Msg: "startup", Attrs: map[string]string{}})
	rb.Add(logging.LogEntry{Level: "WARN", Msg: "something", Attrs: map[string]string{"k": "v"}})

	webFS := makeMockFS()
	eng := &mockEngine{}
	srv, err := ui.NewServer(webFS, eng, http.NotFoundHandler(), nil, nil, rb, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest("GET", "/logs", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var result []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 log entries, got %d", len(result))
	}
	if result[0]["msg"] != "startup" {
		t.Errorf("expected first entry msg=startup, got %q", result[0]["msg"])
	}
	if result[1]["msg"] != "something" {
		t.Errorf("expected second entry msg=something, got %q", result[1]["msg"])
	}
	if result[1]["level"] != "WARN" {
		t.Errorf("expected second entry level=WARN, got %q", result[1]["level"])
	}
}
