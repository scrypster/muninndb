package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// harbourServer stands in for a MuninnDB whose admin password has been changed
// from the default, and whose vault list is scoped the way the real one is: the
// whole list to an admin session, and one vault to anyone else. It serves both
// the login the shell signs in with and the REST vault list.
func harbourServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/auth/login"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["username"] == "harbourmaster" && body["password"] == "changed-pass" {
				http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "admin-session"})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/vaults":
			names := []string{"default"}
			if c, err := r.Cookie("muninn_session"); err == nil && c.Value == "admin-session" {
				names = []string{"default", "pilot-station", "customs-ledger"}
			}
			_ = json.NewEncoder(w).Encode(names)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestShellSignInKeepsTheSessionItSignedInWith pins that after the shell signs
// in with credentials the operator typed, it holds THAT login's session.
//
// It used to validate the typed credentials, throw the session away, and then
// "refresh" by logging in again with the default credentials, which is the
// login that had just failed. The shell was left with no session. That went
// unnoticed while the vault list was served to anyone; once the list is scoped
// to the caller (SEC-25), `show vaults` in a shell on a server whose password
// had been changed listed one vault and said nothing.
func TestShellSignInKeepsTheSessionItSignedInWith(t *testing.T) {
	ts := harbourServer(t)
	t.Setenv("MUNINNDB_ADMIN_URL", ts.URL)

	asked := 0
	cookie, err := replSignIn(ts.URL, func() (string, string, error) {
		asked++
		return "harbourmaster", "changed-pass", nil
	})
	if err != nil {
		t.Fatalf("replSignIn: %v", err)
	}
	if asked != 1 {
		t.Errorf("the operator was asked for credentials %d times; want once, after the default ones failed", asked)
	}
	if cookie != "admin-session" {
		t.Errorf("session after signing in with the operator's own credentials = %q; want the session that login returned", cookie)
	}

	r := &replState{sessionCookie: cookie}
	out := captureStdout(func() { r.cmdShowVaults() })
	for _, vault := range []string{"default", "pilot-station", "customs-ledger"} {
		if !strings.Contains(out, vault) {
			t.Errorf("show vaults does not list %q after an admin sign-in:\n%s", vault, out)
		}
	}
}

// TestShellSignInRefusesWrongCredentials: a wrong password is an error, not a
// shell with no session.
func TestShellSignInRefusesWrongCredentials(t *testing.T) {
	ts := harbourServer(t)
	cookie, err := replSignIn(ts.URL, func() (string, string, error) {
		return "harbourmaster", "not-the-password", nil
	})
	if err == nil || cookie != "" {
		t.Errorf("replSignIn with a wrong password = %q, %v; want an error and no session", cookie, err)
	}
}
