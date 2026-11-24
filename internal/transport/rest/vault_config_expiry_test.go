package rest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestHandleSetVaultConfig_CannotMintOrClearExpiry pins the single-owner rule
// for VaultConfig.ExpiresAt (#973). Only muninn_create_workflow_vault stamps
// it, because a stamped expiry on a wf-* name is exactly what puts a vault
// into the reclamation sweep's path, and the sweep deletes the vault's name,
// its config and every capability — live ones included.
//
// Before this guard, PUT /api/admin/vaults/config decoded the full struct from
// the request body and stored it verbatim, so a hand-written body could mint
// an expiry on any vault, and a rename into the wf- namespace completed the
// path into the sweep.
func TestHandleSetVaultConfig_CannotMintOrClearExpiry(t *testing.T) {
	srv, store := newCollisionTestServer(t, nil)

	put := func(t *testing.T, body string) {
		t.Helper()
		req := httptest.NewRequest("PUT", "/api/admin/vaults/config?force=true", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT vault config: status %d: %s", w.Code, w.Body.String())
		}
	}

	// A body that tries to mint an expiry must not produce one.
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	put(t, fmt.Sprintf(`{"name":"wf-handwritten","expires_at":%q}`, past))

	got, err := store.GetVaultConfig("wf-handwritten")
	if err != nil {
		t.Fatalf("read back wf-handwritten: %v", err)
	}
	if got.ExpiresAt != nil {
		t.Fatalf("admin PUT minted an expiry (%v): a hand-written body can put a vault into the reclamation sweep", *got.ExpiresAt)
	}

	// An expiry stamped by the create path must survive an ordinary edit.
	created := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "wf-created", ExpiresAt: &created}); err != nil {
		t.Fatalf("seed wf-created: %v", err)
	}
	put(t, `{"name":"wf-created"}`)

	got, err = store.GetVaultConfig("wf-created")
	if err != nil {
		t.Fatalf("read back wf-created: %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("admin PUT cleared the create-stamped expiry")
	}
	if !got.ExpiresAt.Equal(created) {
		t.Fatalf("admin PUT changed the create-stamped expiry: got %v, want %v", *got.ExpiresAt, created)
	}
}
