package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func newBypassTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

// --- Session Token Tests ---

func TestSessionToken_TamperedPayload(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")
	token, err := NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("expected token with dot separator, got %q", token)
	}

	tampered := []byte(parts[0])
	tampered[0] ^= 0xFF
	tamperedToken := string(tampered) + "." + parts[1]

	if validateSessionToken(tamperedToken, secret) {
		t.Error("tampered payload should not validate")
	}
}

func TestSessionToken_TamperedHMAC(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")
	token, err := NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("expected token with dot separator, got %q", token)
	}

	tampered := []byte(parts[1])
	tampered[0] ^= 0xFF
	tamperedToken := parts[0] + "." + string(tampered)

	if validateSessionToken(tamperedToken, secret) {
		t.Error("tampered HMAC should not validate")
	}
}

func TestSessionToken_ExpiredToken(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")

	expiry := time.Now().Add(-1 * time.Hour).Unix()
	payload := "admin|" + strconv.FormatInt(expiry, 10)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := computeHMAC(encoded, secret)
	token := encoded + "." + sig

	if validateSessionToken(token, secret) {
		t.Error("expired token should not validate")
	}
}

func TestSessionToken_WrongSecret(t *testing.T) {
	secretA := []byte("secret-A-32-bytes-long-padding!!")
	secretB := []byte("secret-B-32-bytes-long-padding!!")

	token, err := NewSessionToken("admin", secretA)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	if validateSessionToken(token, secretB) {
		t.Error("token signed with secret A should not validate with secret B")
	}
}

func TestSessionToken_EmptySecret(t *testing.T) {
	emptySecret := []byte{}

	token, err := NewSessionToken("admin", emptySecret)
	if err != nil {
		t.Fatalf("NewSessionToken with empty secret should not panic: %v", err)
	}

	otherSecret := []byte("other-secret")
	if validateSessionToken(token, otherSecret) {
		t.Error("token created with empty secret should not validate with different secret")
	}

	if validateSessionToken("random.garbage", emptySecret) {
		t.Error("garbage token should not validate even with empty secret")
	}
}

func TestSessionToken_MalformedFormat(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")

	tests := []struct {
		name  string
		token string
	}{
		{"empty string", ""},
		{"no dot separator", "nodothere"},
		{"extra dots", "a.b.c"},
		{"dot only", "."},
		{"leading dot", ".something"},
		{"trailing dot", "something."},
		{"random bytes", string([]byte{0x00, 0x01, 0x02, 0xFF})},
		{"very long junk", strings.Repeat("A", 10000)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validateSessionToken(tt.token, secret) {
				t.Errorf("malformed token %q should not validate", tt.token)
			}
		})
	}
}

// --- API Key Tests ---

func TestAPIKey_InvalidPrefix(t *testing.T) {
	s := newBypassTestStore(t)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	token := "xx_" + base64.RawURLEncoding.EncodeToString(raw)

	if _, err := s.ValidateAPIKey(token); err == nil {
		t.Error("token without mk_ prefix should fail validation")
	}
}

func TestAPIKey_ValidFormatNonExistent(t *testing.T) {
	s := newBypassTestStore(t)

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	token := "mk_" + base64.RawURLEncoding.EncodeToString(raw)

	if _, err := s.ValidateAPIKey(token); err == nil {
		t.Error("well-formed but non-existent key should fail validation")
	}
}

func TestAPIKey_TruncatedToken(t *testing.T) {
	s := newBypassTestStore(t)

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	token := "mk_" + base64.RawURLEncoding.EncodeToString(raw)

	if _, err := s.ValidateAPIKey(token); err == nil {
		t.Error("truncated token (16 bytes instead of 32) should fail validation")
	}
}

func TestAPIKey_EmptyToken(t *testing.T) {
	s := newBypassTestStore(t)

	if _, err := s.ValidateAPIKey(""); err == nil {
		t.Error("empty token should fail validation")
	}
}

// --- Middleware Tests ---

func TestMiddleware_CrossVaultAccess(t *testing.T) {
	s := newBypassTestStore(t)
	s.SetVaultConfig(VaultConfig{Name: "private", Public: false})
	s.SetVaultConfig(VaultConfig{Name: "other", Public: false})

	token, _, err := s.GenerateAPIKey("private", "agent", "full", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Key was generated for "private" but request targets "other".
	// Vault scoping enforcement must reject this with 401.
	req := httptest.NewRequest("GET", "/api/engrams?vault=other", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("cross-vault access: key for %q accessing %q: expected 401, got %d", "private", "other", w.Code)
	}
}

func TestMiddleware_ObserveModeWriteAttempt(t *testing.T) {
	s := newBypassTestStore(t)
	s.SetVaultConfig(VaultConfig{Name: "vault", Public: false})

	token, _, err := s.GenerateAPIKey("vault", "reader", "observe", nil)
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	var capturedMode string
	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedMode, _ = r.Context().Value(ContextMode).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/engrams?vault=vault", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (key is valid), got %d", w.Code)
	}
	if capturedMode != "observe" {
		t.Errorf("observe-mode key should set context mode %q, got %q", "observe", capturedMode)
	}
}

func TestMiddleware_EmptyAuthHeader(t *testing.T) {
	s := newBypassTestStore(t)
	s.SetVaultConfig(VaultConfig{Name: "locked", Public: false})

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=locked", nil)
	req.Header.Set("Authorization", "")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty auth header on locked vault: expected 401, got %d", w.Code)
	}
}

func TestMiddleware_BearerWithoutToken(t *testing.T) {
	s := newBypassTestStore(t)
	s.SetVaultConfig(VaultConfig{Name: "locked", Public: false})

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("GET", "/api/engrams?vault=locked", nil)
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("'Bearer ' with no token: expected 401, got %d", w.Code)
	}
}

func TestMiddleware_SqlInjectionInVaultName(t *testing.T) {
	s := newBypassTestStore(t)

	handler := s.VaultAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	injections := []string{
		"'; DROP TABLE vaults; --",
		"\" OR 1=1 --",
		"vault UNION SELECT * FROM admin",
		"../../../etc/passwd",
		"default\x00evil",
	}

	for _, injection := range injections {
		t.Run(injection, func(t *testing.T) {
			target := "/api/engrams?vault=" + url.QueryEscape(injection)
			req := httptest.NewRequest("GET", target, nil)
			w := httptest.NewRecorder()
			handler(w, req)

			// Vault not configured — fail-closed returns 401, not a 500 or panic.
			if w.Code == http.StatusInternalServerError {
				t.Errorf("SQL injection vault name %q caused 500", injection)
			}
			if w.Code != http.StatusUnauthorized {
				t.Errorf("injection vault name %q: expected 401 (not found = locked), got %d", injection, w.Code)
			}
		})
	}
}
