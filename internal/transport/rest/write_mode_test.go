package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func withObserveCtx(r *http.Request) *http.Request {
	ctx := context.WithValue(r.Context(), auth.ContextMode, "observe")
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
		// POST/PUT mutation endpoints that return vault data — blocked to
		// prevent data exfiltration via response body (hardening rounds 1 & 2).
		{"Evolve", "POST", s.handleEvolve},
		{"Consolidate", "POST", s.handleConsolidateEngrams},
		{"Decide", "POST", s.handleDecide},
		{"Restore", "POST", s.handleRestore},
		{"RetryEnrich", "POST", s.handleRetryEnrich},
		// PUT mutation endpoints that return state/tag data — blocked in hardening round 2.
		{"SetState", "PUT", s.handleSetState},
		{"UpdateTags", "PUT", s.handleUpdateTags},
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

// TestWriteOnlyMode_ObserveModeCanRead verifies that observe-mode sessions
// pass through WriteOnlyGuard (observe-mode read access is preserved).
func TestWriteOnlyMode_ObserveModeCanRead(t *testing.T) {
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
			req = withObserveCtx(req)
			w := httptest.NewRecorder()
			auth.WriteOnlyGuard(tc.handler)(w, req)
			if w.Code == http.StatusForbidden {
				t.Errorf("%s: observe mode must not return 403", tc.name)
			}
		})
	}
}

func TestReadOnlyMode_MutatingHandlersBlocked(t *testing.T) {
	s := newWriteModeTestServer(t)

	cases := []struct {
		name    string
		method  string
		handler http.HandlerFunc
	}{
		{"CreateEngram", http.MethodPost, s.handleCreateEngram},
		{"BatchCreate", http.MethodPost, s.handleBatchCreate},
		{"Link", http.MethodPost, s.handleLink},
		{"DeleteEngram", http.MethodDelete, s.handleDeleteEngram},
		{"SetState", http.MethodPut, s.handleSetState},
		{"UpdateTags", http.MethodPut, s.handleUpdateTags},
		{"Evolve", http.MethodPost, s.handleEvolve},
		{"Consolidate", http.MethodPost, s.handleConsolidateEngrams},
		{"Decide", http.MethodPost, s.handleDecide},
		{"Restore", http.MethodPost, s.handleRestore},
		{"RetryEnrich", http.MethodPost, s.handleRetryEnrich},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/", nil)
			req = withObserveCtx(req)
			w := httptest.NewRecorder()
			auth.ReadOnlyGuard(tc.handler)(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestReadOnlyMode_SemanticReadHandlersPassThrough(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	s := newTestServer(t, store)

	observeToken, _, err := store.GenerateAPIKey("default", "observer", auth.ModeObserve, nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey observe: %v", err)
	}

	cases := []struct {
		name string
		path string
		body string
	}{
		{"Activate", "/api/activate?vault=default", `{"context":["x"]}`},
		{"BatchGetEngramLinks", "/api/engrams/links/batch?vault=default", `{"ids":["id1"]}`},
		{"Traverse", "/api/traverse?vault=default", `{"start_id":"root-id"}`},
		{"Explain", "/api/explain?vault=default", `{"engram_id":"some-id"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+observeToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code == http.StatusUnauthorized {
				t.Fatalf("observe key should authenticate semantic read endpoint, got 401: %s", w.Body.String())
			}
			if w.Code == http.StatusForbidden {
				t.Fatalf("semantic read endpoint must remain available to observe keys")
			}
		})
	}
}

func TestPublicVaultFullModeMutationsPassThrough(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	s := newTestServer(t, store)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"CreateEngram", http.MethodPost, "/api/engrams?vault=default", `{"concept":"test","content":"hello"}`},
		{"BatchCreate", http.MethodPost, "/api/engrams/batch?vault=default", `{"engrams":[{"concept":"a","content":"x"}]}`},
		{"DeleteEngram", http.MethodDelete, "/api/engrams/some-id?vault=default", ``},
		{"Link", http.MethodPost, "/api/link?vault=default", `{"source_id":"id1","target_id":"id2","rel_type":1}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code == http.StatusUnauthorized {
				t.Fatalf("public full-mode access should not require auth, got 401: %s", w.Body.String())
			}
			if w.Code == http.StatusForbidden {
				t.Fatalf("expected non-403 response, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestReadOnlyMode_PublicVaultSemanticReadEndpointsPassThrough(t *testing.T) {
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	s := newTestServer(t, store)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"Activate", "/api/activate?vault=default", `{"context":["x"]}`},
		{"BatchGetEngramLinks", "/api/engrams/links/batch?vault=default", `{"ids":["id1"]}`},
		{"Traverse", "/api/traverse?vault=default", `{"start_id":"root-id"}`},
		{"Explain", "/api/explain?vault=default", `{"engram_id":"some-id"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			if w.Code == http.StatusForbidden {
				t.Fatalf("expected non-403 response, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}


// TestWriteOnlyMode_FullModeCanMutateStateAndTags verifies that full-mode
// sessions can call PUT state and PUT tags (regression for hardening round 2).
func TestWriteOnlyMode_FullModeCanMutateStateAndTags(t *testing.T) {
	s := newWriteModeTestServer(t)

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"SetState", s.handleSetState},
		{"UpdateTags", s.handleUpdateTags},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/", nil)
			req = withFullCtx(req)
			w := httptest.NewRecorder()
			auth.ReadOnlyGuard(auth.WriteOnlyGuard(tc.handler))(w, req)
			if w.Code == http.StatusForbidden {
				t.Errorf("%s: full mode must not return 403", tc.name)
			}
		})
	}
}

// TestWriteOnlyGuard_UnknownModePassesThrough verifies that unrecognised or
// empty mode strings are treated as non-write-only (pass through the guard).
// WriteOnlyGuard uses exact-equality so garbage values are safe by default.
func TestWriteOnlyGuard_UnknownModePassesThrough(t *testing.T) {
	sentinel := http.StatusTeapot
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(sentinel)
	})
	guarded := auth.WriteOnlyGuard(inner)

	for _, mode := range []string{"", "unknown", "WRITE", "Write", "admin"} {
		t.Run("mode="+mode, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			ctx := context.WithValue(req.Context(), auth.ContextMode, mode)
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()
			guarded(w, req)
			if w.Code != sentinel {
				t.Errorf("mode %q: expected pass-through (%d), got %d", mode, sentinel, w.Code)
			}
		})
	}
}
