package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parseAdminFlags
// ---------------------------------------------------------------------------

func TestParseAdminFlags_Defaults(t *testing.T) {
	remaining, username, password, prompted := parseAdminFlags([]string{"delete", "my-vault", "--yes"})
	if username != "root" {
		t.Errorf("expected default username 'root', got %q", username)
	}
	if password != "" {
		t.Errorf("expected empty password, got %q", password)
	}
	if prompted {
		t.Error("expected prompted=false")
	}
	if len(remaining) != 3 || remaining[0] != "delete" {
		t.Errorf("expected remaining=[delete my-vault --yes], got %v", remaining)
	}
}

func TestParseAdminFlags_UserFlag(t *testing.T) {
	remaining, username, _, _ := parseAdminFlags([]string{"-u", "admin", "delete", "foo"})
	if username != "admin" {
		t.Errorf("expected username 'admin', got %q", username)
	}
	if len(remaining) != 2 || remaining[0] != "delete" || remaining[1] != "foo" {
		t.Errorf("unexpected remaining: %v", remaining)
	}
}

func TestParseAdminFlags_UserEqualsFlag(t *testing.T) {
	_, username, _, _ := parseAdminFlags([]string{"--user=bob", "list"})
	if username != "bob" {
		t.Errorf("expected username 'bob', got %q", username)
	}
}

func TestParseAdminFlags_PromptFlag(t *testing.T) {
	_, _, password, prompted := parseAdminFlags([]string{"-p", "delete", "foo"})
	if !prompted {
		t.Error("expected prompted=true with -p flag")
	}
	if password != "" {
		t.Errorf("expected empty password with bare -p, got %q", password)
	}
}

func TestParseAdminFlags_InlinePassword(t *testing.T) {
	_, _, password, prompted := parseAdminFlags([]string{"-psecret123", "delete", "foo"})
	if password != "secret123" {
		t.Errorf("expected password 'secret123', got %q", password)
	}
	if prompted {
		t.Error("expected prompted=false with inline password")
	}
}

func TestParseAdminFlags_PasswordEquals(t *testing.T) {
	_, _, password, _ := parseAdminFlags([]string{"--password=s3cr3t", "delete", "foo"})
	if password != "s3cr3t" {
		t.Errorf("expected password 's3cr3t', got %q", password)
	}
}

func TestParseAdminFlags_HostFlag(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	parseAdminFlags([]string{"-h", "10.0.1.5:9000", "list"})
	if vaultAdminBase != "http://10.0.1.5:9000" {
		t.Errorf("expected admin base 'http://10.0.1.5:9000', got %q", vaultAdminBase)
	}
	if vaultUIBase != "http://10.0.1.5:9001" {
		t.Errorf("expected UI base 'http://10.0.1.5:9001', got %q", vaultUIBase)
	}
}

func TestParseAdminFlags_HostEquals(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	parseAdminFlags([]string{"--host=myhost", "list"})
	if vaultAdminBase != "http://myhost:8475" {
		t.Errorf("expected admin base 'http://myhost:8475', got %q", vaultAdminBase)
	}
	if vaultUIBase != "http://myhost:8476" {
		t.Errorf("expected UI base 'http://myhost:8476', got %q", vaultUIBase)
	}
}

func TestParseAdminFlags_AllFlagsCombined(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	remaining, username, password, prompted := parseAdminFlags(
		[]string{"-u", "admin", "-psecret", "-h", "db.example.com:5000", "delete", "vault1", "--yes"})

	if username != "admin" {
		t.Errorf("username: got %q, want 'admin'", username)
	}
	if password != "secret" {
		t.Errorf("password: got %q, want 'secret'", password)
	}
	if prompted {
		t.Error("prompted should be false with inline password")
	}
	want := []string{"delete", "vault1", "--yes"}
	if len(remaining) != len(want) {
		t.Fatalf("remaining: got %v, want %v", remaining, want)
	}
	for i, w := range want {
		if remaining[i] != w {
			t.Errorf("remaining[%d]: got %q, want %q", i, remaining[i], w)
		}
	}
}

func TestParseAdminFlags_UserFlagAtEnd(t *testing.T) {
	// -u at the end with no following arg should not panic.
	remaining, username, _, _ := parseAdminFlags([]string{"list", "-u"})
	if username != "root" {
		t.Errorf("expected default username when -u has no value, got %q", username)
	}
	if len(remaining) != 1 || remaining[0] != "list" {
		t.Errorf("unexpected remaining: %v", remaining)
	}
}

// ---------------------------------------------------------------------------
// setHostPorts
// ---------------------------------------------------------------------------

func TestSetHostPorts_HostOnly(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	setHostPorts("myserver")
	if vaultAdminBase != "http://myserver:8475" {
		t.Errorf("admin base: got %q, want http://myserver:8475", vaultAdminBase)
	}
	if vaultUIBase != "http://myserver:8476" {
		t.Errorf("UI base: got %q, want http://myserver:8476", vaultUIBase)
	}
}

func TestSetHostPorts_HostAndPort(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	setHostPorts("10.0.0.1:9000")
	if vaultAdminBase != "http://10.0.0.1:9000" {
		t.Errorf("admin base: got %q, want http://10.0.0.1:9000", vaultAdminBase)
	}
	if vaultUIBase != "http://10.0.0.1:9001" {
		t.Errorf("UI base: got %q, want http://10.0.0.1:9001", vaultUIBase)
	}
}

func TestSetHostPorts_InvalidPort(t *testing.T) {
	oldAdmin, oldUI := vaultAdminBase, vaultUIBase
	defer func() { vaultAdminBase = oldAdmin; vaultUIBase = oldUI }()

	setHostPorts("host:notanumber")
	if vaultAdminBase != "http://host:notanumber" {
		t.Errorf("admin base: got %q", vaultAdminBase)
	}
	if vaultUIBase != "http://host:8476" {
		t.Errorf("UI base should fallback to :8476, got %q", vaultUIBase)
	}
}

// ---------------------------------------------------------------------------
// loginAdmin
// ---------------------------------------------------------------------------

func TestLoginAdmin_Success(t *testing.T) {
	oldUI, oldCookie := vaultUIBase, vaultCookie
	defer func() { vaultUIBase = oldUI; vaultCookie = oldCookie }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&req)
		if req.Username == "root" && req.Password == "password" {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "tok123"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	vaultUIBase = srv.URL

	if err := loginAdmin("root", "password"); err != nil {
		t.Fatalf("loginAdmin should succeed: %v", err)
	}
	if vaultCookie != "tok123" {
		t.Errorf("expected cookie 'tok123', got %q", vaultCookie)
	}
}

func TestLoginAdmin_InvalidCredentials(t *testing.T) {
	oldUI, oldCookie := vaultUIBase, vaultCookie
	defer func() { vaultUIBase = oldUI; vaultCookie = oldCookie }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	vaultUIBase = srv.URL

	if err := loginAdmin("root", "wrong"); err == nil {
		t.Error("loginAdmin should fail with bad credentials")
	}
}

func TestLoginAdmin_ConnectionError(t *testing.T) {
	oldUI := vaultUIBase
	defer func() { vaultUIBase = oldUI }()

	vaultUIBase = "http://127.0.0.1:19999"
	if err := loginAdmin("root", "password"); err == nil {
		t.Error("loginAdmin should fail on connection error")
	}
}

func TestLoginAdmin_NoCookie200(t *testing.T) {
	oldUI, oldCookie := vaultUIBase, vaultCookie
	defer func() { vaultUIBase = oldUI; vaultCookie = oldCookie }()
	vaultCookie = ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 but no cookie
	}))
	defer srv.Close()
	vaultUIBase = srv.URL

	if err := loginAdmin("root", "password"); err != nil {
		t.Fatalf("loginAdmin should succeed even without cookie: %v", err)
	}
	if vaultCookie != "" {
		t.Errorf("expected empty cookie when none set, got %q", vaultCookie)
	}
}

// ---------------------------------------------------------------------------
// addSessionCookie
// ---------------------------------------------------------------------------

func TestAddSessionCookie_WhenSet(t *testing.T) {
	oldCookie := vaultCookie
	defer func() { vaultCookie = oldCookie }()

	vaultCookie = "my-session-tok"
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	addSessionCookie(req)

	cookie, err := req.Cookie("muninn_session")
	if err != nil {
		t.Fatalf("expected cookie to be set: %v", err)
	}
	if cookie.Value != "my-session-tok" {
		t.Errorf("expected cookie value 'my-session-tok', got %q", cookie.Value)
	}
}

func TestAddSessionCookie_WhenEmpty(t *testing.T) {
	oldCookie := vaultCookie
	defer func() { vaultCookie = oldCookie }()

	vaultCookie = ""
	req, _ := http.NewRequest("GET", "http://example.com/test", nil)
	addSessionCookie(req)

	if _, err := req.Cookie("muninn_session"); err == nil {
		t.Error("expected no cookie when vaultCookie is empty")
	}
}

// ---------------------------------------------------------------------------
// runVaultList
// ---------------------------------------------------------------------------

func TestRunVaultList_ShowsAllVaults(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{"default", "work", "personal"})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{})
	})
	if !strings.Contains(out, "default") || !strings.Contains(out, "work") || !strings.Contains(out, "personal") {
		t.Errorf("expected all vaults in output, got: %q", out)
	}
	if !strings.Contains(out, "3 vault(s)") {
		t.Errorf("expected '3 vault(s)' count, got: %q", out)
	}
}

func TestRunVaultList_PatternFilter(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{"default", "proof-test-1", "proof-test-2", "work"})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{"--pattern", "proof-*"})
	})
	if !strings.Contains(out, "proof-test-1") || !strings.Contains(out, "proof-test-2") {
		t.Errorf("expected matched vaults, got: %q", out)
	}
	if strings.Contains(out, "default") || strings.Contains(out, "work") {
		t.Errorf("expected non-matching vaults to be filtered, got: %q", out)
	}
	if !strings.Contains(out, "2 of 4") {
		t.Errorf("expected '2 of 4' count, got: %q", out)
	}
}

func TestRunVaultList_PatternEqualsForm(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{"alpha", "beta"})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{"--pattern=alpha"})
	})
	if !strings.Contains(out, "alpha") {
		t.Errorf("expected 'alpha' in output, got: %q", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("'beta' should be filtered out, got: %q", out)
	}
}

func TestRunVaultList_PositionalPattern(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{"work", "work-backup"})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{"work*"})
	})
	if !strings.Contains(out, "work") || !strings.Contains(out, "work-backup") {
		t.Errorf("expected both work vaults, got: %q", out)
	}
}

func TestRunVaultList_EmptyResult(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]string{})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{})
	})
	if !strings.Contains(out, "No vaults found") {
		t.Errorf("expected 'No vaults found', got: %q", out)
	}
}

func TestRunVaultList_ConnectionError(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	vaultAdminBase = "http://127.0.0.1:19999"
	out := captureStdout(func() {
		runVaultList([]string{})
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error message, got: %q", out)
	}
}

func TestRunVaultList_ServerError(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{})
	})
	if !strings.Contains(out, "Error: HTTP 500") {
		t.Errorf("expected HTTP 500 error, got: %q", out)
	}
}

func TestRunVaultList_WrappedResponse(t *testing.T) {
	oldBase := vaultAdminBase
	defer func() { vaultAdminBase = oldBase }()

	// Some API versions may return {"vaults": [...]}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string][]string{"vaults": {"v1", "v2"}})
	}))
	defer srv.Close()
	vaultAdminBase = srv.URL

	out := captureStdout(func() {
		runVaultList([]string{})
	})
	if !strings.Contains(out, "v1") || !strings.Contains(out, "v2") {
		t.Errorf("expected wrapped vaults in output, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// Dispatch prefix matching (vault:* and cluster:*)
// ---------------------------------------------------------------------------

func TestParseSubcommand_VaultWithSubcommand(t *testing.T) {
	sub := parseSubcommand([]string{"vault", "list"})
	if sub != "vault:list" {
		t.Errorf("expected 'vault:list', got %q", sub)
	}
}

func TestParseSubcommand_VaultBare(t *testing.T) {
	sub := parseSubcommand([]string{"vault"})
	if sub != "vault" {
		t.Errorf("expected 'vault', got %q", sub)
	}
}

func TestParseSubcommand_VaultWithFlags(t *testing.T) {
	// When second arg is a flag, only the first word is returned.
	sub := parseSubcommand([]string{"vault", "-h"})
	if sub != "vault" {
		t.Errorf("expected 'vault' (flag not joined), got %q", sub)
	}
}
