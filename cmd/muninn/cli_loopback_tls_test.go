package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newSelfSignedLoginServer returns a loopback TLS server (self-signed cert)
// that accepts {"username","password"} on /api/auth/login and sets a session
// cookie. Reaching it at all exercises the insecure-for-loopback client path:
// a verifying client fails the handshake against the self-signed cert.
func newSelfSignedLoginServer(t *testing.T, wantUser, wantPass string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" {
			http.NotFound(w, r)
			return
		}
		var creds map[string]string
		if err := json.NewDecoder(r.Body).Decode(&creds); err != nil ||
			creds["username"] != wantUser || creds["password"] != wantPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "test-session"})
		w.WriteHeader(http.StatusOK)
	}))
}

// TestLoginAdmin_LoopbackTLS: the admin login POST must work against a
// loopback TLS daemon with a self-signed cert (#443) and capture the cookie.
func TestLoginAdmin_LoopbackTLS(t *testing.T) {
	ts := newSelfSignedLoginServer(t, "root", "s3cret")
	defer ts.Close()
	oldUI, oldCookie := vaultUIBase, vaultCookie
	vaultUIBase, vaultCookie = ts.URL, ""
	defer func() { vaultUIBase, vaultCookie = oldUI, oldCookie }()

	if err := loginAdmin("root", "s3cret"); err != nil {
		t.Fatalf("loginAdmin over loopback TLS: %v", err)
	}
	if vaultCookie != "test-session" {
		t.Errorf("vaultCookie = %q, want %q", vaultCookie, "test-session")
	}
	if err := loginAdmin("root", "wrong"); err == nil {
		t.Error("bad password must still fail over loopback TLS")
	}
}

func TestShellValidateAdmin_LoopbackTLS(t *testing.T) {
	ts := newSelfSignedLoginServer(t, "root", "pw")
	defer ts.Close()
	if err := shellValidateAdmin(ts.URL, "root", "pw"); err != nil {
		t.Fatalf("shellValidateAdmin over loopback TLS: %v", err)
	}
	if err := shellValidateAdmin(ts.URL, "root", "nope"); err == nil {
		t.Error("bad password must still fail over loopback TLS")
	}
}

func TestAutoAuth_LoopbackTLS(t *testing.T) {
	ts := newSelfSignedLoginServer(t, "root", "password")
	defer ts.Close()
	cookie, err := autoAuth(ts.URL)
	if err != nil {
		t.Fatalf("autoAuth over loopback TLS: %v", err)
	}
	if cookie != "test-session" {
		t.Errorf("cookie = %q, want %q", cookie, "test-session")
	}
}

func TestMCPHealthCheck_LoopbackTLS(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	if !mcpHealthCheck(ts.URL) {
		t.Error("mcpHealthCheck must succeed against a loopback TLS server with a self-signed cert")
	}
}
