package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scopedVaultServer stands in for a server that lists every vault only to the
// admin session, as the real one does (SEC-25): with the session cookie it
// answers with all three vaults, without it with the one vault open to anyone.
// The login endpoint accepts or refuses according to loginOK.
func scopedVaultServer(t *testing.T, loginOK bool) (cleanup func()) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/auth/login"):
			if !loginOK {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "admin-session"})
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/vaults":
			names := []string{"default"}
			if c, err := r.Cookie("muninn_session"); err == nil && c.Value == "admin-session" {
				names = []string{"default", "pilot-station", "customs-ledger"}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(names)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
	vaultAdminBase, vaultUIBase, vaultCookie = ts.URL, ts.URL, ""
	return func() {
		ts.Close()
		vaultAdminBase, vaultUIBase, vaultCookie = oldAdmin, oldUI, oldCookie
	}
}

// TestVaultList_ListsEveryVaultWithTheAdminSession: `muninn vault list` signs
// in and sends the session, so it still lists every vault now that the server
// tells a caller with no session only about the vault open to it. Sent without
// the session, the command would list one vault and say nothing.
func TestVaultList_ListsEveryVaultWithTheAdminSession(t *testing.T) {
	defer scopedVaultServer(t, true)()

	out := captureStdout(func() { runVault([]string{"list"}) })
	for _, vault := range []string{"default", "pilot-station", "customs-ledger"} {
		if !strings.Contains(out, vault) {
			t.Errorf("vault %q is missing from the list:\n%s", vault, out)
		}
	}
	if strings.Contains(out, "Not signed in") {
		t.Errorf("signed in, yet the output says the list is the short one:\n%s", out)
	}
}

// TestVaultList_SaysSoWhenTheListIsTheShortOne: when it cannot sign in, the
// command still lists what the server will tell it — and says that is not
// every vault, rather than letting a one-line list pass for the whole server.
func TestVaultList_SaysSoWhenTheListIsTheShortOne(t *testing.T) {
	defer scopedVaultServer(t, false)()

	out := captureStdout(func() { runVault([]string{"list"}) })
	if !strings.Contains(out, "default") {
		t.Errorf("the vault open to anyone is missing:\n%s", out)
	}
	if strings.Contains(out, "pilot-station") {
		t.Errorf("a vault the server did not list appeared:\n%s", out)
	}
	if !strings.Contains(out, "Not signed in as admin") || !strings.Contains(out, "muninn vault list -u") {
		t.Errorf("the output does not say the list is short, or how to get the full one:\n%s", out)
	}
}

// TestListVaultNamesAdmin_SendsTheSession: "export every vault" lists the
// vaults first. Without the session that list would be one vault long and the
// export would silently cover one vault.
func TestListVaultNamesAdmin_SendsTheSession(t *testing.T) {
	defer scopedVaultServer(t, true)()
	vaultCookie = "admin-session" // its callers have already signed in

	names, err := listVaultNamesAdmin()
	if err != nil {
		t.Fatalf("listVaultNamesAdmin: %v", err)
	}
	if len(names) != 3 {
		t.Errorf("vaults = %v; want all three", names)
	}
}
