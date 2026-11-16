package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// vaultListServer stands in for the real server's scoped vault list (SEC-25).
// An admin session is told every vault. A caller with no credential is told the
// ONE vault its request is on, which is `default`, and only if that vault is
// public: another public vault is not listed, and when `default` is locked the
// answer is 401.
func vaultListServer(t *testing.T, loginOK, defaultPublic bool) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/auth/login"):
			if !loginOK {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "admin-session"})
		case r.URL.Path == "/api/vaults":
			w.Header().Set("Content-Type", "application/json")
			if c, err := r.Cookie("muninn_session"); err == nil && c.Value == "admin-session" {
				// harbour-notices is public too; only an admin is told so.
				_ = json.NewEncoder(w).Encode([]string{"default", "harbour-notices", "pilot-station"})
				return
			}
			if !defaultPublic {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"VAULT_LOCKED","error":"vault \"default\" requires an API key"}`))
				return
			}
			_ = json.NewEncoder(w).Encode([]string{"default"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
	vaultAdminBase, vaultUIBase, vaultCookie = ts.URL, ts.URL, ""
	t.Cleanup(func() {
		ts.Close()
		vaultAdminBase, vaultUIBase, vaultCookie = oldAdmin, oldUI, oldCookie
	})
}

// exitStatus runs f with the process exit replaced, and returns the status f
// asked to exit with (zero if it did not ask) and what it printed.
func exitStatus(t *testing.T, f func()) (code int, out string) {
	t.Helper()
	old := osExit
	osExit = func(c int) { code = c }
	defer func() { osExit = old }()
	out = captureStdout(f)
	return code, out
}

// TestVaultList_WhatItSaysIsTrue pins what `muninn vault list` tells the
// operator about the list it printed, in each situation it can be in. The
// earlier text, "only vaults open to anyone are shown", was wrong twice: a
// second public vault is open to anyone and is not shown, and the same line was
// printed when the server could not be reached and nothing had been listed.
func TestVaultList_WhatItSaysIsTrue(t *testing.T) {
	const howTo = "muninn vault list -u <admin user> -p"

	t.Run("signed in: every vault, and nothing more to say", func(t *testing.T) {
		vaultListServer(t, true, true)
		code, out := exitStatus(t, func() { runVault([]string{"list"}) })
		if code != 0 {
			t.Errorf("exit status %d; want 0", code)
		}
		for _, vault := range []string{"default", "harbour-notices", "pilot-station"} {
			if !strings.Contains(out, vault) {
				t.Errorf("vault %q is missing:\n%s", vault, out)
			}
		}
		if strings.Contains(out, "Not signed in") || strings.Contains(out, howTo) {
			t.Errorf("signed in, yet the output talks about signing in:\n%s", out)
		}
	})

	t.Run("not signed in: the default vault only, and it says other public vaults are missing too", func(t *testing.T) {
		vaultListServer(t, false, true)
		code, out := exitStatus(t, func() { runVault([]string{"list"}) })
		if code != 0 {
			t.Errorf("exit status %d; want 0: a list was printed, and it says what it is", code)
		}
		if !strings.Contains(out, "  default\n") || strings.Contains(out, "harbour-notices") {
			t.Fatalf("want exactly the default vault listed:\n%s", out)
		}
		if strings.Contains(out, "only vaults open to anyone are shown") {
			t.Errorf("the output still claims every public vault is shown; harbour-notices is public and is not:\n%s", out)
		}
		for _, want := range []string{"Not signed in as admin", "default vault", "public ones included", howTo} {
			if !strings.Contains(out, want) {
				t.Errorf("the output does not say %q:\n%s", want, out)
			}
		}
	})

	t.Run("not signed in and the default vault is locked: nothing is listed, and it says why", func(t *testing.T) {
		vaultListServer(t, false, false)
		code, out := exitStatus(t, func() { runVault([]string{"list"}) })
		if code == 0 {
			t.Errorf("exit status 0 with nothing listed: a script would read that as a server with no vaults")
		}
		if strings.Contains(out, "vault(s)") || strings.Contains(out, "only vaults open to anyone are shown") {
			t.Errorf("nothing was listed, yet the output describes a list:\n%s", out)
		}
		for _, want := range []string{"Not signed in as admin", "default vault is not public", howTo} {
			if !strings.Contains(out, want) {
				t.Errorf("the output does not say %q:\n%s", want, out)
			}
		}
	})

	t.Run("server not reachable: the connection error, and no word about sessions or lists", func(t *testing.T) {
		// Port 0 is never a listener: the dial fails without reaching anything.
		const dead = "http://127.0.0.1:0"
		oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
		vaultAdminBase, vaultUIBase, vaultCookie = dead, dead, ""
		t.Cleanup(func() { vaultAdminBase, vaultUIBase, vaultCookie = oldAdmin, oldUI, oldCookie })

		code, out := exitStatus(t, func() { runVault([]string{"list"}) })
		if code == 0 {
			t.Errorf("exit status 0 although the server was not reached")
		}
		if !strings.Contains(out, "Error connecting to MuninnDB") {
			t.Errorf("no connection error:\n%s", out)
		}
		for _, wrong := range []string{"are shown", "is shown", "Not signed in", howTo} {
			if strings.Contains(out, wrong) {
				t.Errorf("nothing was listed and the server was never reached, yet the output says %q:\n%s", wrong, out)
			}
		}
	})
}

// TestVaultUsageDoesNotPromiseEveryVault: the usage text for `vault list` says
// what the command does, which without an admin session is not "all vaults".
func TestVaultUsageDoesNotPromiseEveryVault(t *testing.T) {
	out := captureStdout(printVaultUsage)
	if strings.Contains(out, "List all vaults") {
		t.Errorf("`muninn vault` usage still says \"List all vaults\":\n%s", out)
	}
	if !strings.Contains(out, "every vault when signed in as admin") {
		t.Errorf("`muninn vault` usage does not say when the list is complete:\n%s", out)
	}
}

// TestShowVaults_SignsInAndSaysWhenItCouldNot: `muninn show vaults`, outside
// the shell, asked for the vault list with no session, so once the list was
// scoped (SEC-25) it printed one vault under help text that said "List all
// vaults". It now signs in when the default credentials work, and says the list
// is not every vault when they do not.
func TestShowVaults_SignsInAndSaysWhenItCouldNot(t *testing.T) {
	t.Run("default credentials work: every vault", func(t *testing.T) {
		vaultListServer(t, true, true)
		t.Setenv("MUNINNDB_ADMIN_URL", vaultAdminBase)
		t.Setenv("MUNINNDB_DATA", t.TempDir())
		code, out := exitStatus(t, runShowVaults)
		if code != 0 {
			t.Errorf("exit status %d; want 0", code)
		}
		for _, vault := range []string{"default", "harbour-notices", "pilot-station"} {
			if !strings.Contains(out, vault) {
				t.Errorf("vault %q is missing:\n%s", vault, out)
			}
		}
		if strings.Contains(out, "Not signed in") {
			t.Errorf("signed in, yet the output says it is not:\n%s", out)
		}
	})
	t.Run("default credentials refused: the short list, and it says so", func(t *testing.T) {
		vaultListServer(t, false, true)
		t.Setenv("MUNINNDB_ADMIN_URL", vaultAdminBase)
		t.Setenv("MUNINNDB_DATA", t.TempDir())
		_, out := exitStatus(t, runShowVaults)
		if !strings.Contains(out, "default") || strings.Contains(out, "pilot-station") {
			t.Fatalf("want only the default vault:\n%s", out)
		}
		for _, want := range []string{"Not signed in as admin, so this is not every vault", "muninn vault list -u <admin user> -p"} {
			if !strings.Contains(out, want) {
				t.Errorf("the output does not say %q:\n%s", want, out)
			}
		}
	})
}

// TestVaultList_ExitStatus pins what a script can rely on: the status is zero
// only when a list was printed and nothing the user asked for failed. It used
// to be zero whatever happened, so an unreachable server and a refused password
// both read as success.
func TestVaultList_ExitStatus(t *testing.T) {
	t.Run("a password the server refuses is a failure, even though the short list is printed", func(t *testing.T) {
		vaultListServer(t, false, true)
		code, out := exitStatus(t, func() { runVault([]string{"-u", "harbourmaster", "--password=not-the-password", "list"}) })
		if code == 0 {
			t.Errorf("exit status 0 after the given credentials were refused:\n%s", out)
		}
		if !strings.Contains(out, "Not signed in as admin") {
			t.Errorf("the output does not say the list is the short one:\n%s", out)
		}
	})
	t.Run("show vaults: nothing listed is a failure, and no vault is invented", func(t *testing.T) {
		vaultListServer(t, false, false)
		t.Setenv("MUNINNDB_ADMIN_URL", vaultAdminBase)
		t.Setenv("MUNINNDB_DATA", t.TempDir())
		code, out := exitStatus(t, runShowVaults)
		if code == 0 {
			t.Errorf("exit status 0 with nothing listed")
		}
		if strings.Contains(out, "(built-in)") || strings.Contains(out, "  default") {
			t.Errorf("the server listed nothing, yet the output names a vault:\n%s", out)
		}
		if !strings.Contains(out, "default vault is not public") {
			t.Errorf("the output does not say why nothing is listed:\n%s", out)
		}
	})
}
