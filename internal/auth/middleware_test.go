package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
)

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

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=default", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("public vault no key: expected 200, got %d", w.Code)
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
