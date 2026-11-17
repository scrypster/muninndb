package rest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// post sends one POST with a JSON body. bearer and admin are optional
// credentials.
func (e *scopeEnv) post(t *testing.T, path, body, bearer string, admin bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if admin {
		req.AddCookie(&http.Cookie{Name: "muninn_session", Value: e.session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, out
}

// vaults is every vault the server holds, as an admin session is told.
func (e *scopeEnv) vaults(t *testing.T) []string {
	t.Helper()
	status, body := e.get(t, "/api/vaults", "", true)
	if status != http.StatusOK {
		t.Fatalf("admin GET /api/vaults: HTTP %d %s", status, body)
	}
	var names []string
	if err := json.Unmarshal(body, &names); err != nil {
		t.Fatalf("decode vault list %s: %v", body, err)
	}
	sort.Strings(names)
	return names
}

func hello(vault string) string {
	b, _ := json.Marshal(map[string]string{"version": "1", "vault": vault})
	return string(b)
}

// TestHelloRegistersNothingForACallerWhoMayNotUseTheVault pins that
// POST /api/hello is not a way to write to the server without a credential
// (SEC-29). Before this the route had no authentication at all and registered
// whatever vault name its body carried: two keys written to storage for every
// distinct name, which then showed up in the admin vault list.
func TestHelloRegistersNothingForACallerWhoMayNotUseTheVault(t *testing.T) {
	env := newScopeEnv(t)
	before := env.vaults(t)

	t.Run("no credential, a vault nobody made", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello("invented-by-a-stranger"), "", false)
		if status != http.StatusUnauthorized {
			t.Errorf("HTTP %d %s; want 401", status, body)
		}
	})
	t.Run("no credential, a locked vault", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello(env.pinned), "", false)
		if status != http.StatusUnauthorized {
			t.Errorf("HTTP %d %s; want 401", status, body)
		}
	})
	t.Run("no credential, no vault named, and default is not public", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", `{"version":"1"}`, "", false)
		if status != http.StatusUnauthorized {
			t.Errorf("HTTP %d %s; want 401", status, body)
		}
	})
	// Sixty, to stay inside the per-address rate limit (100 a second), which
	// was the only thing that slowed this down.
	t.Run("no credential, sixty different names", func(t *testing.T) {
		for i := 0; i < 60; i++ {
			if status, body := env.post(t, "/api/hello", hello(fmt.Sprintf("squatted-%03d", i)), "", false); status != http.StatusUnauthorized {
				t.Fatalf("name %d: HTTP %d %s; want 401", i, status, body)
			}
		}
	})
	t.Run("a key naming a vault that is not its own", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello("invented-by-a-key-holder"), env.key(t, auth.ModeFull), false)
		if status == http.StatusOK {
			t.Errorf("HTTP 200 %s; want a refusal", body)
		}
	})
	t.Run("an admin session naming something that is not a vault name", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello("Not A Vault Name"), "", true)
		if status != http.StatusBadRequest {
			t.Errorf("HTTP %d %s; want 400", status, body)
		}
	})

	if after := env.vaults(t); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("the server's vaults changed.\nbefore: %v\nafter:  %v", before, after)
	}

	// The callers hello is for are answered as before.
	t.Run("no credential, a public vault", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello(env.public), "", false)
		if status != http.StatusOK || !strings.Contains(string(body), `"vault_id":"`+env.public+`"`) {
			t.Errorf("HTTP %d %s; want 200 for the public vault", status, body)
		}
	})
	t.Run("a key, its own vault named or not", func(t *testing.T) {
		token := env.key(t, auth.ModeFull)
		for _, body := range []string{hello(env.pinned), `{"version":"1"}`} {
			status, out := env.post(t, "/api/hello", body, token, false)
			if status != http.StatusOK || !strings.Contains(string(out), `"vault_id":"`+env.pinned+`"`) {
				t.Errorf("%s: HTTP %d %s; want 200 for the key's vault", body, status, out)
			}
		}
	})
	t.Run("an admin session registers a new vault, as the console does after creating one", func(t *testing.T) {
		status, body := env.post(t, "/api/hello", hello("tug-roster"), "", true)
		if status != http.StatusOK {
			t.Fatalf("HTTP %d %s; want 200", status, body)
		}
		after := env.vaults(t)
		if len(after) != len(before)+1 || !strings.Contains(strings.Join(after, ","), "tug-roster") {
			t.Errorf("vaults after an admin hello for tug-roster: %v; want %v plus tug-roster", after, before)
		}
	})
}
