package rest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestInvalidVaultNameIsRefusedWithoutBeingRepeated pins that a request with no
// key naming something no vault can be called is refused as that, before the
// name is looked up, and that the refusal does not carry the name back
// (SEC-27). The caller chose the name and has shown no credential.
func TestInvalidVaultNameIsRefusedWithoutBeingRepeated(t *testing.T) {
	env := newScopeEnv(t)

	for _, name := range []string{strings.Repeat("v", 65), strings.Repeat("v", 8000), "Harbour-Notices", "harbour.notices"} {
		status, body := env.get(t, "/api/stats?vault="+name, "", false)
		// 401 VAULT_LOCKED is what such a request was always answered with.
		if status != http.StatusUnauthorized || string(body) != `{"code":"VAULT_LOCKED","error":"invalid vault name"}` {
			t.Errorf("%d-byte name %.20q: HTTP %d %.120s; want 401 VAULT_LOCKED \"invalid vault name\"", len(name), name, status, body)
		}
		if len(body) > 256 {
			t.Errorf("%d-byte name: the refusal was %d bytes; it must not carry the name back", len(name), len(body))
		}
	}

	t.Run("well-formed names are answered as before", func(t *testing.T) {
		if status, body := env.get(t, "/api/stats?vault="+env.public, "", false); status != http.StatusOK {
			t.Errorf("no key, public vault: HTTP %d %s", status, body)
		}
		if status, body := env.get(t, "/api/stats?vault=tug-roster", "", false); status != http.StatusUnauthorized || !strings.Contains(string(body), "VAULT_LOCKED") {
			t.Errorf("no key, unconfigured vault: HTTP %d %s", status, body)
		}
		if status, body := env.get(t, "/api/stats?vault="+env.public, env.key(t, auth.ModeFull), false); status != http.StatusUnauthorized || !strings.Contains(string(body), "VAULT_KEY_MISMATCH") {
			t.Errorf("a key naming another vault: HTTP %d %s", status, body)
		}
	})
}
