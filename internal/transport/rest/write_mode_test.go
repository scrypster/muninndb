package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

func withWriteOnlyCtx(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), auth.ContextMode, "write")
	ctx = context.WithValue(ctx, auth.ContextVault, "default")
	return r.WithContext(ctx)
}

func withFullCtx(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), auth.ContextMode, "full")
	ctx = context.WithValue(ctx, auth.ContextVault, "default")
	return r.WithContext(ctx)
}

func newWriteModeTestServer(t *testing.T) *Server {
	t.Helper()
	store := newTestAuthStore(t)
	return newTestServer(t, store)
}

// TestWriteOnlyMode_ReadHandlersBlocked verifies all guarded endpoints return
// 403 for write-only mode. The original 14 pure-read endpoints plus 5 POST
// mutation endpoints that echo engram data in their response bodies are all
// blocked to prevent data exfiltration through any response path.
func TestWriteOnlyMode_ReadHandlersBlocked(t *testing.T) {
	s := newWriteModeTestServer(t)

	cases := []struct {
		name    string
		method  string
		handler http.HandlerFunc
	}{
		// Original 14 read-only endpoints.
		{"GetEngram", "GET", s.handleGetEngram},
		{"Activate", "POST", s.handleActivate},
		{"ListEngrams", "GET", s.handleListEngrams},
		{"GetEngramLinks", "GET", s.handleGetEngramLinks},
		{"BatchGetEngramLinks", "POST", s.handleBatchGetEngramLinks},
		{"ListVaults", "GET", s.handleListVaults},
		{"GetSession", "GET", s.handleGetSession},
		{"Subscribe", "GET", s.handleSubscribe},
		{"Traverse", "POST", s.handleTraverse},
		{"Explain", "POST", s.handleExplain},
		{"ListDeleted", "GET", s.handleListDeleted},
		{"Contradictions", "GET", s.handleContradictions},
		{"Guide", "GET", s.handleGuide},
		{"Stats", "GET", s.handleStats},
		// POST mutation endpoints that return full engram payloads — blocked to
		// prevent data exfiltration via response body (hardening round 1).
		{"Evolve", "POST", s.handleEvolve},
		{"Consolidate", "POST", s.handleConsolidateEngrams},
		{"Decide", "POST", s.handleDecide},
		{"Restore", "POST", s.handleRestore},
		{"RetryEnrich", "POST", s.handleRetryEnrich},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", nil)
			req = withWriteOnlyCtx(req)
			w := httptest.NewRecorder()
			// Apply the guard the same way the router does (innermost wrapper).
			auth.WriteOnlyGuard(tc.handler)(w, req)
			if w.Code != http.StatusForbidden {
				t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestWriteOnlyMode_WriteHandlersNotBlocked verifies pure ingest/mutation
// endpoints that do NOT echo vault data are accessible with write-only keys
// (may fail for other reasons, but must NOT return 403).
func TestWriteOnlyMode_WriteHandlersNotBlocked(t *testing.T) {
	s := newWriteModeTestServer(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"CreateEngram", s.handleCreateEngram},
		{"BatchCreate", s.handleBatchCreate},
		{"Link", s.handleLink},
		{"DeleteEngram", s.handleDeleteEngram},
		{"SetState", s.handleSetState},
		{"UpdateTags", s.handleUpdateTags},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", nil)
			req = withWriteOnlyCtx(req)
			w := httptest.NewRecorder()
			// No WriteOnlyGuard — write handlers are not wrapped.
			tc.handler(w, req)
			if w.Code == http.StatusForbidden {
				t.Errorf("%s: must not return 403 for write-only mode", tc.name)
			}
		})
	}
}

// TestWriteOnlyMode_FullModeCanRead verifies full-mode sessions pass through
// read handlers (regression for "write"→"full" admin mode change).
func TestWriteOnlyMode_FullModeCanRead(t *testing.T) {
	s := newWriteModeTestServer(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"ListVaults", s.handleListVaults},
		{"Stats", s.handleStats},
		{"Guide", s.handleGuide},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req = withFullCtx(req)
			w := httptest.NewRecorder()
			auth.WriteOnlyGuard(tc.handler)(w, req)
			if w.Code == http.StatusForbidden {
				t.Errorf("%s: full mode should not return 403", tc.name)
			}
		})
	}
}
