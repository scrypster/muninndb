package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestObserveCredential_ReadToolsDoNotStampCacheRecency: an observe-mode
// credential is always effectively read-only, so NO tool it calls may stamp
// the L1 cache access time that later recalls score as recency. Exercised
// through muninn_traverse, a read tool with no read_only flag of its own.
func TestObserveCredential_ReadToolsDoNotStampCacheRecency(t *testing.T) {
	eng, _, store, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "walled"
	ws := store.ResolveVaultPrefix(vault)
	old := time.Now().Add(-300 * 24 * time.Hour)
	id, err := store.WriteEngram(context.Background(), ws, &storage.Engram{
		Concept: "footbridge decking", Content: "the footbridge decking was replaced with larch boards",
		Confidence: 1, CreatedAt: old, UpdatedAt: old, LastAccess: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.EngramLastAccessNs(ws, id) != 0 {
		t.Fatal("fixture: engram already in the L1 cache")
	}

	keys := newMockKeyStore(auth.APIKey{ID: "obsns", Vault: vault, Mode: auth.ModeObserve})
	srv := New(":0", NewEngineAdapter(eng, nil, nil), "mdb_static", keys, nil, nil)
	w := doAuthenticatedPost(srv, "mk_obsns", mkToolCallBody("muninn_traverse", map[string]any{"vault": vault, "start_id": id.String()}))
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("traverse under observe failed: %s", resp.Error.Message)
	}
	eng.WaitWriteTimeIdle()
	if got := store.EngramLastAccessNs(ws, id); got != 0 {
		t.Fatalf("an observe-credential traverse stamped the cache access time (%d)", got)
	}
}
