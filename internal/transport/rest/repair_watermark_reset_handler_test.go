package rest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
)

// TestHandleResetRepairWatermark_OK is the #761 REST-surface reproduction:
// an operator must be able to reset a per-vault repair watermark over HTTP.
func TestHandleResetRepairWatermark_OK(t *testing.T) {
	eng := &resetWatermarkOKEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewBufferString(`{"which":"evolve"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/some-vault/repair-watermark/reset", body)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !eng.called {
		t.Fatal("ResetRepairWatermark was never called")
	}
	if eng.gotWhich != engine.RepairWatermarkEvolve {
		t.Errorf("which passed through: got %q, want %q", eng.gotWhich, engine.RepairWatermarkEvolve)
	}
}

func TestHandleResetRepairWatermark_VaultNotFound(t *testing.T) {
	eng := &resetWatermarkNotFoundEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body := bytes.NewBufferString(`{"which":"all"}`)
	req := httptest.NewRequest("POST", "/api/admin/vaults/missing-vault/repair-watermark/reset", body)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	// A route that does not exist ALSO answers 404 ("404 page not found"),
	// which would make this assertion pass for the wrong reason — the route
	// must actually be registered and dispatch into the handler's own
	// ErrVaultNotFound path, not Go's http.ServeMux catch-all.
	if !bytes.Contains(w.Body.Bytes(), []byte("vault not found")) {
		t.Errorf("body must carry the handler's own not-found error, got: %s", w.Body.String())
	}
}

func TestHandleResetRepairWatermark_MissingWhich(t *testing.T) {
	eng := &resetWatermarkOKEngine{}
	server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/admin/vaults/some-vault/repair-watermark/reset", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing which, got %d: %s", w.Code, w.Body.String())
	}
	if eng.called {
		t.Error("engine must not be called when which is missing")
	}
}

type resetWatermarkOKEngine struct {
	MockEngine
	called   bool
	gotWhich engine.RepairWatermarkKind
}

func (e *resetWatermarkOKEngine) ResetRepairWatermark(_ context.Context, _ string, which engine.RepairWatermarkKind) error {
	e.called = true
	e.gotWhich = which
	return nil
}

type resetWatermarkNotFoundEngine struct{ MockEngine }

func (e *resetWatermarkNotFoundEngine) ResetRepairWatermark(_ context.Context, _ string, _ engine.RepairWatermarkKind) error {
	return fmt.Errorf("reset repair watermark: %w", engine.ErrVaultNotFound)
}
