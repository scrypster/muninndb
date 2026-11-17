package engine

import (
	"context"
	"strconv"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// seedStatVaults writes memories into three vaults of a fictional harbour pilot
// station and returns their names.
func seedStatVaults(t *testing.T, eng *Engine) (own, second, third string) {
	t.Helper()
	own, second, third = "pilot-station", "harbour-notices", "customs-ledger"
	for vault, n := range map[string]int{own: 2, second: 3, third: 5} {
		for i := 0; i < n; i++ {
			_, err := eng.Write(context.Background(), &mbp.WriteRequest{
				Vault: vault, Concept: "Log entry " + strconv.Itoa(i),
				Content: "Entry " + strconv.Itoa(i) + " in the " + vault + " log.",
			})
			if err != nil {
				t.Fatalf("seed %s: %v", vault, err)
			}
		}
	}
	return own, second, third
}

// TestStat_AnswersForOneVaultUnlessServerWide is the engine half of SEC-24:
// Stat is about the vault the request names and nothing else, on every
// transport, because the scoping is here and not in each handler. The
// server-wide answer has to be asked for by name.
func TestStat_AnswersForOneVaultUnlessServerWide(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	own, second, third := seedStatVaults(t, eng)
	ctx := context.Background()

	scoped, err := eng.Stat(ctx, &mbp.StatRequest{Vault: own})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if scoped.Scope != mbp.StatScopeVault {
		t.Errorf("scope = %q; want %q", scoped.Scope, mbp.StatScopeVault)
	}
	if scoped.VaultCount != 1 {
		t.Errorf("vault_count = %d; want 1", scoped.VaultCount)
	}
	if scoped.StorageBytes != 0 {
		t.Errorf("storage_bytes = %d; want 0 at vault scope", scoped.StorageBytes)
	}
	if len(scoped.CoherenceScores) != 1 {
		t.Errorf("coherence has %d entries; want only %q", len(scoped.CoherenceScores), own)
	}
	if got := scoped.CoherenceScores[own].TotalEngrams; got != 2 {
		t.Errorf("coherence[%q].total_engrams = %d; want 2", own, got)
	}
	for _, vault := range []string{second, third} {
		if _, ok := scoped.CoherenceScores[vault]; ok {
			t.Errorf("a request for %q was told about vault %q", own, vault)
		}
	}

	server, err := eng.Stat(ctx, &mbp.StatRequest{Vault: own, ServerWide: true})
	if err != nil {
		t.Fatalf("Stat(server-wide): %v", err)
	}
	if server.Scope != mbp.StatScopeServer {
		t.Errorf("scope = %q; want %q", server.Scope, mbp.StatScopeServer)
	}
	if server.VaultCount != 3 {
		t.Errorf("server-wide vault_count = %d; want 3", server.VaultCount)
	}
	for _, vault := range []string{own, second, third} {
		if _, ok := server.CoherenceScores[vault]; !ok {
			t.Errorf("server-wide coherence has no entry for %q", vault)
		}
	}
	// A named vault still gets its own engram count at server scope: the
	// console shows the selected vault's count beside the server's totals.
	if server.EngramCount != scoped.EngramCount {
		t.Errorf("server-wide engram_count for %q = %d; want the vault's own %d", own, server.EngramCount, scoped.EngramCount)
	}
}

// TestStat_NoVaultNamedIsTheDefaultVaultNotTheServer: leaving the vault out is
// not a way to ask for everything. A caller that forgets to name a vault gets
// the default vault's figures, as it does on every other operation.
func TestStat_NoVaultNamedIsTheDefaultVaultNotTheServer(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	own, second, third := seedStatVaults(t, eng)

	resp, err := eng.Stat(context.Background(), &mbp.StatRequest{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if resp.Scope != mbp.StatScopeVault || resp.VaultCount != 1 || resp.StorageBytes != 0 {
		t.Errorf("scope=%q vault_count=%d storage_bytes=%d; want the vault-scoped answer", resp.Scope, resp.VaultCount, resp.StorageBytes)
	}
	if resp.EngramCount != 0 {
		t.Errorf("engram_count = %d; want 0, the empty default vault's (the server holds 10)", resp.EngramCount)
	}
	for _, vault := range []string{own, second, third} {
		if _, ok := resp.CoherenceScores[vault]; ok {
			t.Errorf("an unnamed-vault request was told about vault %q", vault)
		}
	}
}
