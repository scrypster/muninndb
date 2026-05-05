package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
)

func makeSessionToken(t *testing.T, secret []byte) string {
	t.Helper()
	token, err := auth.NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("makeSessionToken: %v", err)
	}
	return token
}

func TestWriteOnlyFromContext(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"write", true},
		{"full", false},
		{"observe", false},
		{"", false},
	}
	for _, tc := range tests {
		ctx := context.WithValue(context.Background(), auth.ContextMode, tc.mode)
		if got := auth.WriteOnlyFromContext(ctx); got != tc.want {
			t.Errorf("mode=%q: WriteOnlyFromContext=%v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestWriteOnlyGuard_Blocks(t *testing.T) {
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
	handler := auth.WriteOnlyGuard(inner)

	req := httptest.NewRequest("GET", "/", nil)
	ctx := context.WithValue(req.Context(), auth.ContextMode, "write")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req.WithContext(ctx))

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
	if reached {
		t.Error("inner handler should not be called for write-only mode")
	}
}

func TestWriteOnlyGuard_PassesThrough(t *testing.T) {
	for _, mode := range []string{"full", "observe", ""} {
		mode := mode
		reached := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
		handler := auth.WriteOnlyGuard(inner)

		req := httptest.NewRequest("GET", "/", nil)
		if mode != "" {
			ctx := context.WithValue(req.Context(), auth.ContextMode, mode)
			req = req.WithContext(ctx)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		if !reached {
			t.Errorf("mode=%q: inner handler should be called", mode)
		}
	}
}

// Regression: after "write"→"full" change, admin session must still be able to read.
func TestVaultAuthWithAdminBypass_SetsFullMode(t *testing.T) {
	store := newTestStore(t)
	secret := []byte("test-secret")

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		mode, _ := r.Context().Value(auth.ContextMode).(string)
		if mode != "full" {
			t.Errorf("admin session mode = %q, want %q", mode, "full")
		}
		if auth.WriteOnlyFromContext(r.Context()) {
			t.Error("WriteOnlyFromContext must return false for admin session")
		}
	})

	handler := store.VaultAuthWithAdminBypass(secret, inner)
	req := httptest.NewRequest("GET", "/?vault=default", nil)
	token := makeSessionToken(t, secret)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: token})
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("inner handler was not called")
	}
}

func newTestStore(t *testing.T) *auth.Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return auth.NewStore(db)
}

func TestAuthMiddleware_PublicVaultNoKey(t *testing.T) {
	s := newTestStore(t)
	// Explicitly configure vault as public to allow unauthenticated access.
	// Unconfigured vaults now default to locked (fail-closed).
	s.SetVaultConfig(auth.VaultConfig{Name: "default", Public: true})

	var capturedMode string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedMode, _ = r.Context().Value(auth.ContextMode).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("public vault no key: expected 200, got %d", w.Code)
	}
	if capturedMode != auth.ModeFull {
		t.Errorf("public vault no key: expected mode %q, got %q", auth.ModeFull, capturedMode)
	}
}

func TestAuthMiddleware_UnconfiguredVaultNoKey(t *testing.T) {
	s := newTestStore(t)
	// No config stored — unconfigured vaults default to locked (fail-closed).

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unconfigured vault no key: expected 401 (fail-closed), got %d", w.Code)
	}
}

func TestAuthMiddleware_LockedVaultNoKey(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "secret", Public: false})

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=secret", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("locked vault no key: expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidKey(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "myv", Public: false})
	token, _, _ := s.GenerateAPIKey("myv", "agent", "observe", nil)

	var capturedMode string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedMode, _ = r.Context().Value(auth.ContextMode).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=myv", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid key: expected 200, got %d", w.Code)
	}
	if capturedMode != "observe" {
		t.Errorf("expected mode 'observe' in context, got %q", capturedMode)
	}
}

func TestAuthMiddleware_InvalidKey(t *testing.T) {
	s := newTestStore(t)

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	req.Header.Set("Authorization", "Bearer mk_thisisnotavalidtoken")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid key: expected 401, got %d", w.Code)
	}
}

func TestObserveFromContext_Defaults(t *testing.T) {
	// Without any context value, ObserveFromContext returns false
	req := httptest.NewRequest("GET", "/", nil)
	if auth.ObserveFromContext(req.Context()) {
		t.Error("expected ObserveFromContext to return false with no context value")
	}
}

func TestAdminSessionMiddleware_ValidCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-enough!")
	token, err := auth.NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	reached := false
	handler := auth.AdminSessionMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: token})
	w := httptest.NewRecorder()
	handler(w, req)

	if !reached {
		t.Error("expected next handler to be called with valid session cookie")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAdminSessionMiddleware_NoCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-enough!")

	handler := auth.AdminSessionMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("expected redirect (302) with no cookie, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestAdminSessionMiddleware_InvalidCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-long-enough!")

	handler := auth.AdminSessionMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: "bad.token"})
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("expected redirect (302) with invalid cookie, got %d", w.Code)
	}
}

func TestAdminAPIMiddleware_ValidCookie(t *testing.T) {
	s := newTestStore(t)
	secret := []byte("test-secret-32-bytes-long-enough!")
	token, err := auth.NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	reached := false
	handler := s.AdminAPIMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/admin/keys", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: token})
	w := httptest.NewRecorder()
	handler(w, req)

	if !reached {
		t.Error("expected next handler to be called with valid session cookie")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAdminAPIMiddleware_NoCookie(t *testing.T) {
	s := newTestStore(t)
	secret := []byte("test-secret-32-bytes-long-enough!")

	handler := s.AdminAPIMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/admin/keys", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with no cookie, got %d", w.Code)
	}
}

func TestAdminAPIMiddleware_InvalidCookie(t *testing.T) {
	s := newTestStore(t)
	secret := []byte("test-secret-32-bytes-long-enough!")

	handler := s.AdminAPIMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/admin/keys", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: "tampered.value"})
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with invalid cookie, got %d", w.Code)
	}
}

// TestAdminAPIMiddleware_VaultBearerTokenRejected verifies that a valid vault
// API key (Bearer token) is not accepted by AdminAPIMiddleware. Admin routes
// require a session cookie — vault keys must not grant admin access.
func TestAdminAPIMiddleware_VaultBearerTokenRejected(t *testing.T) {
	s := newTestStore(t)
	secret := []byte("test-secret-32-bytes-long-enough!")

	// Generate a real, valid vault API key.
	s.SetVaultConfig(auth.VaultConfig{Name: "default", Public: false})
	token, _, err := s.GenerateAPIKey("default", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	handlerCalled := false
	handler := s.AdminAPIMiddleware(secret, func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/admin/keys", nil)
	// Provide the vault Bearer token but no session cookie.
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("vault Bearer token on admin route: expected 401, got %d", w.Code)
	}
	if handlerCalled {
		t.Error("admin handler must not be called when only a vault Bearer token is present")
	}
}

// TestAuthMiddleware_VaultIsolation verifies that a key scoped to one vault
// cannot be used to access a different vault.
func TestAuthMiddleware_VaultIsolation(t *testing.T) {
	s := newTestStore(t)
	// Configure two vaults
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-b", Public: false})

	// Generate key scoped to vault-a only
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Key for vault-a should NOT work for vault-b
	req := httptest.NewRequest("GET", "/api/engrams?vault=vault-b", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("key for vault-a accessing vault-b: expected 401, got %d", w.Code)
	}
}

// TestAuthMiddleware_KeyVaultUsedDirectly verifies that for authenticated requests
// the vault is taken from the API key itself — no ?vault= query param required.
// This replaces the old "non-default key requires explicit vault" behaviour that
// was only enforceable because body/query were parsed before auth.
func TestAuthMiddleware_KeyVaultUsedDirectly(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	// No ?vault= param — vault comes from the key, not the URL.
	req := httptest.NewRequest("GET", "/api/engrams", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("key vault used directly: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "vault-a" {
		t.Errorf("captured vault: want %q, got %q", "vault-a", capturedVault)
	}
}

func TestAuthMiddleware_ValidKey_BodyVault(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader(`{"vault":"vault-a","concept":"c","content":"body"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("body vault with matching key: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "vault-a" {
		t.Errorf("captured vault: want %q, got %q", "vault-a", capturedVault)
	}
}

func TestAuthMiddleware_PublicVaultInBodyNoKey(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "public-vault", Public: true})

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(`{"vault":"public-vault","context":["hello"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("public body vault: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "public-vault" {
		t.Errorf("captured vault: want %q, got %q", "public-vault", capturedVault)
	}
}

// TestAuthMiddleware_QueryVaultMatchesKey verifies that ?vault= matching the key
// vault passes, and that body vault fields are irrelevant for authenticated requests
// (body is not parsed for vault routing when a Bearer token is present).
func TestAuthMiddleware_QueryVaultMatchesKey(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	// ?vault= matches key vault — body has a different vault field but that is ignored.
	req := httptest.NewRequest("POST", "/api/engrams?vault=vault-a", strings.NewReader(`{"vault":"vault-b","concept":"c","content":"body"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("query matches key vault: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "vault-a" {
		t.Errorf("captured vault: want %q, got %q", "vault-a", capturedVault)
	}
}

func TestAuthMiddleware_BatchBodyVault(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader(`{"engrams":[{"vault":"vault-a","concept":"c1","content":"body"},{"concept":"c2","content":"body"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("batch body vault with matching key: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "vault-a" {
		t.Errorf("captured vault: want %q, got %q", "vault-a", capturedVault)
	}
}

// TestAuthMiddleware_BatchBodyMixedVaults verifies that for authenticated requests,
// mixed vault fields inside the batch body are irrelevant to the middleware —
// body is not parsed for vault routing when a Bearer token is present.
// Cross-vault enforcement at the payload level is the responsibility of the handler.
func TestAuthMiddleware_BatchBodyMixedVaults(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	handlerCalled := false
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader(`{"engrams":[{"vault":"vault-a","concept":"c1","content":"body"},{"vault":"vault-b","concept":"c2","content":"body"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	// Middleware does not parse the body for authenticated requests, so mixed body
	// vaults are passed through to the handler (which enforces payload consistency).
	if w.Code != http.StatusOK {
		t.Fatalf("mixed batch body vaults with valid key: expected 200 (middleware skips body), got %d: %s", w.Code, w.Body.String())
	}
	if !handlerCalled {
		t.Error("inner handler should have been called")
	}
}

// TestAuthMiddleware_BodyVaultIgnoredForAuthenticatedRequests verifies that for
// authenticated requests the body is not read at all — regardless of Content-Type
// or body vault fields. Vault is taken entirely from the validated key.
func TestAuthMiddleware_BodyVaultIgnoredForAuthenticatedRequests(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedVault string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedVault, _ = r.Context().Value(auth.ContextVault).(string)
		w.WriteHeader(http.StatusOK)
	})

	// Body has a different vault and content-type is text/plain — none of that
	// matters; middleware never reads the body for authenticated requests.
	req := httptest.NewRequest("POST", "/api/engrams", strings.NewReader(`{"vault":"vault-b","concept":"c","content":"body"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("body vault ignored for authenticated requests: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if capturedVault != "vault-a" {
		t.Errorf("captured vault: want %q, got %q", "vault-a", capturedVault)
	}
}

// TestVaultAuthMiddleware_AuthenticatedSkipsBodyParse verifies that when a valid
// Bearer token is present, the middleware does NOT read the request body before
// validating the token — preventing unauthenticated DoS body-parse amplification.
func TestVaultAuthMiddleware_AuthenticatedSkipsBodyParse(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: false})
	token, _, err := s.GenerateAPIKey("myvault", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	// Build a 4 MB body to simulate an expensive parse target.
	largBody := bytes.Repeat([]byte(`{"vault":"myvault","concept":"x","content":"`+strings.Repeat("a", 100)+`"}`), 10000)

	handlerCalled := false
	var bodyAfterMiddleware []byte
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		// Body must still be readable by the downstream handler — middleware must
		// not consume it (or must restore it) when taking the authenticated path.
		bodyAfterMiddleware, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams?vault=myvault", bytes.NewReader(largBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("valid Bearer + large body: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !handlerCalled {
		t.Fatal("inner handler was not called")
	}
	// The body must be intact (not consumed by middleware).
	if len(bodyAfterMiddleware) != len(largBody) {
		t.Errorf("body was consumed by middleware: downstream got %d bytes, want %d", len(bodyAfterMiddleware), len(largBody))
	}

	// Also verify: an invalid token is rejected immediately — body irrelevant.
	req2 := httptest.NewRequest("POST", "/api/engrams", bytes.NewReader(largBody))
	req2.Header.Set("Authorization", "Bearer mk_thisisabadinvalidtoken")
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: expected 401, got %d", w2.Code)
	}
}

// TestAuthMiddleware_BatchBodyTopLevelVaultIgnoredForAuthenticatedRequests verifies
// that for authenticated requests, even conflicting vault fields within the batch
// body (top-level vs per-item) are irrelevant to the middleware. Payload validation
// is delegated to the handler.
func TestAuthMiddleware_BatchBodyTopLevelVaultIgnoredForAuthenticatedRequests(t *testing.T) {
	s := newTestStore(t)
	s.SetVaultConfig(auth.VaultConfig{Name: "vault-a", Public: false})
	token, _, err := s.GenerateAPIKey("vault-a", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	handlerCalled := false
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader(`{"vault":"vault-a","engrams":[{"vault":"vault-b","concept":"c1","content":"body"}]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)

	// Middleware does not parse the body, so internal vault conflicts in the
	// payload pass through to the handler for enforcement.
	if w.Code != http.StatusOK {
		t.Fatalf("batch top-level vault with valid key: expected 200 (middleware skips body), got %d: %s", w.Code, w.Body.String())
	}
	if !handlerCalled {
		t.Error("inner handler should have been called")
	}
}
