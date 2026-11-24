package storage

import (
	"context"
	"testing"
	"time"
)

// TestVaultHasEngrams pins the emptiness probe the workflow-vault reclamation
// sweep trusts to decide whether a vault may be deleted (#973): a key-only
// scan of the vault's 0x01 engram range, limit 1. No counter, no timestamp
// bounds — the count cache can read stale and a time-bounded id range can
// miss a caller-backdated row, and either false-empty would delete a vault
// that holds data.
func TestVaultHasEngrams(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	wsEmpty := keys_VaultPrefixForTest(t, store, "wf-probe-empty")
	has, err := store.VaultHasEngrams(ctx, wsEmpty)
	if err != nil {
		t.Fatalf("probe never-written vault: %v", err)
	}
	if has {
		t.Error("never-written vault reports engrams present, want none")
	}

	// A backdated CreatedAt: the row sits in the 0x01 range regardless of any
	// time bound a probe might be tempted to use.
	wsBackdated := keys_VaultPrefixForTest(t, store, "wf-probe-backdated")
	if _, err := store.WriteEngram(ctx, wsBackdated, &Engram{
		Concept: "old event", Content: "backdated row",
		CreatedAt: time.Now().Add(-365 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("WriteEngram backdated: %v", err)
	}
	has, err = store.VaultHasEngrams(ctx, wsBackdated)
	if err != nil {
		t.Fatalf("probe backdated vault: %v", err)
	}
	if !has {
		t.Error("vault holding one backdated engram reports empty — a time-bounded probe would delete real data")
	}

	// Isolation: engrams in another vault must not satisfy this vault's probe.
	has, err = store.VaultHasEngrams(ctx, wsEmpty)
	if err != nil {
		t.Fatalf("probe empty vault again: %v", err)
	}
	if has {
		t.Error("empty vault reports engrams after an unrelated vault was written — probe is not vault-scoped")
	}

	// Delete the row and the vault reads empty again.
	wsPlain := keys_VaultPrefixForTest(t, store, "wf-probe-plain")
	id, err := store.WriteEngram(ctx, wsPlain, &Engram{Concept: "c", Content: "x"})
	if err != nil {
		t.Fatalf("WriteEngram plain: %v", err)
	}
	has, err = store.VaultHasEngrams(ctx, wsPlain)
	if err != nil {
		t.Fatalf("probe plain vault: %v", err)
	}
	if !has {
		t.Fatal("vault holding one engram reports empty")
	}
	if err := store.DeleteEngram(ctx, wsPlain, id); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}
	has, err = store.VaultHasEngrams(ctx, wsPlain)
	if err != nil {
		t.Fatalf("probe after delete: %v", err)
	}
	if has {
		t.Error("vault reports engrams after its only row was deleted")
	}

	// A workspace of all 0xFF bytes cannot have a finite upper bound (the
	// next prefix wraps). incrementWS refuses it — the same refusal the clone
	// path applies — and the error, not a wrapped bound, is what the caller
	// sees: the sweep treats a failed probe as "do not delete", so the vault
	// in an unprobeable workspace survives rather than reading fail-open.
	wsMax := [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := store.WriteEngram(ctx, wsMax, &Engram{Concept: "c", Content: "max workspace"}); err != nil {
		t.Fatalf("WriteEngram max workspace: %v", err)
	}
	has, err = store.VaultHasEngrams(ctx, wsMax)
	if err == nil {
		t.Fatalf("all-0xFF workspace: probe returned (%v, nil), want the incrementWS overflow error", has)
	}
}

// keys_VaultPrefixForTest registers the vault name and resolves its ws prefix.
func keys_VaultPrefixForTest(t *testing.T, store *PebbleStore, name string) [8]byte {
	t.Helper()
	ws := store.VaultPrefix(name)
	if err := store.WriteVaultName(ws, name); err != nil {
		t.Fatalf("WriteVaultName %s: %v", name, err)
	}
	return ws
}
