package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
)

type rebuildEngine struct {
	MockEngine
	err   error
	calls []string
}

func (e *rebuildEngine) RebuildVectorIndex(_ context.Context, vault string) error {
	e.calls = append(e.calls, vault)
	return e.err
}

// TestHandleRebuildVectorIndex: the operator-triggered HNSW rebuild returns
// 202 at once, 409 while one runs, 404 for an unknown vault.
func TestHandleRebuildVectorIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"accepted", nil, http.StatusAccepted},
		{"in-progress", hnswpkg.ErrRebuildInProgress, http.StatusConflict},
		{"not-found", fmt.Errorf("x: %w", engine.ErrVaultNotFound), http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &rebuildEngine{err: tc.err}
			server := NewServer("localhost:8080", eng, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
			req := httptest.NewRequest("POST", "/api/admin/vaults/trail-logs/reindex-hnsw", nil)
			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if len(eng.calls) != 1 || eng.calls[0] != "trail-logs" {
				t.Errorf("engine called with %v", eng.calls)
			}
		})
	}
}
