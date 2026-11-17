package rest

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestVaultListScopeMatrix pins what GET /api/vaults tells each kind of
// caller. A caller entitled to one vault — a key pinned to it, or no
// credential on a public vault — is told about that vault and no other. Only
// the admin session, which is what the console's vault picker and
// `muninn vault list` use, gets every vault on the server.
//
// Before this, every one of those callers got every vault's name. With the
// default vault public out of the box, that was every vault's name to anyone
// who could reach the REST port.
func TestVaultListScopeMatrix(t *testing.T) {
	env := newScopeEnv(t)
	all := []string{env.other, env.public, env.pinned}
	sort.Strings(all)

	list := func(t *testing.T, path, bearer string, admin bool) (int, []string, string) {
		t.Helper()
		code, body := env.get(t, path, bearer, admin)
		var names []string
		if code == http.StatusOK {
			if err := json.Unmarshal(body, &names); err != nil {
				t.Fatalf("decode %s: %v", body, err)
			}
			sort.Strings(names)
		}
		return code, names, string(body)
	}

	scoped := []struct {
		name, path, bearer, vault string
	}{
		{"no key, public vault", "/api/vaults?vault=" + env.public, "", env.public},
		{"observe key", "/api/vaults", env.key(t, auth.ModeObserve), env.pinned},
		{"full key", "/api/vaults", env.key(t, auth.ModeFull), env.pinned},
		{"append key", "/api/vaults", env.key(t, auth.ModeAppend), env.pinned},
	}
	for _, p := range scoped {
		t.Run(p.name, func(t *testing.T) {
			code, names, body := list(t, p.path, p.bearer, false)
			t.Logf("answer: %d %s", code, body)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			for _, vault := range all {
				if vault != p.vault && strings.Contains(body, vault) {
					t.Errorf("the answer names vault %q, which this caller is not entitled to", vault)
				}
			}
			if len(names) != 1 || names[0] != p.vault {
				t.Errorf("vaults = %v; want only the caller's own, %q", names, p.vault)
			}
		})
	}

	t.Run("write key", func(t *testing.T) {
		if code, _, body := list(t, "/api/vaults", env.key(t, auth.ModeWrite), false); code != http.StatusForbidden {
			t.Fatalf("status %d: %s; want 403, an ingest-only key reads nothing", code, body)
		}
	})
	t.Run("no key, locked vault", func(t *testing.T) {
		if code, _, body := list(t, "/api/vaults?vault="+env.pinned, "", false); code != http.StatusUnauthorized {
			t.Fatalf("status %d: %s; want 401", code, body)
		}
	})

	t.Run("admin session", func(t *testing.T) {
		code, names, body := list(t, "/api/vaults", "", true)
		t.Logf("answer: %d %s", code, body)
		if code != http.StatusOK {
			t.Fatalf("status %d: %s", code, body)
		}
		for _, vault := range all {
			found := false
			for _, n := range names {
				found = found || n == vault
			}
			if !found {
				t.Errorf("the admin session's list %v is missing vault %q", names, vault)
			}
		}
	})

	// A cookie that does not verify is not an admin session.
	t.Run("forged session cookie, public vault", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, env.url+"/api/vaults?vault="+env.public, nil)
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: "not-a-session"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var names []string
		if err := json.NewDecoder(resp.Body).Decode(&names); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(names) != 1 || names[0] != env.public {
			t.Errorf("vaults = %v; want only %q", names, env.public)
		}
	})
}
