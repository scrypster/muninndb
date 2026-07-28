package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestAppendMode_RefusesEvolveAndForget proves the engine-layer backstop: an
// append-mode credential (auth.ModeAppend, threaded via ctx by every transport)
// cannot Evolve or Forget an existing memory, but can still Write (create) and
// the target memory survives the refused destructive calls. This holds
// regardless of transport, so a leaked/misconfigured append key cannot destroy.
func TestAppendMode_RefusesEvolveAndForget(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "append-vault"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "original memory to protect"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id := resp.ID

	// Evolve under append -> refused.
	if _, err := eng.Evolve(withMode(auth.ModeAppend), vault, id, "rewritten content", "reason", nil, ""); !errors.Is(err, ErrAppendForbidden) {
		t.Errorf("Evolve under append mode: err = %v, want ErrAppendForbidden", err)
	}
	// Forget under append -> refused.
	if _, err := eng.Forget(withMode(auth.ModeAppend), &mbp.ForgetRequest{Vault: vault, ID: id}); !errors.Is(err, ErrAppendForbidden) {
		t.Errorf("Forget under append mode: err = %v, want ErrAppendForbidden", err)
	}

	// The protected memory must be intact and unchanged.
	got, err := eng.Read(withMode(auth.ModeAppend), &mbp.ReadRequest{Vault: vault, ID: id, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read after refused destructive ops: %v", err)
	}
	if got.Content != "original memory to protect" {
		t.Errorf("content changed under append mode: %q", got.Content)
	}

	// Append CAN create a new memory (additive is allowed).
	if _, err := eng.Write(withMode(auth.ModeAppend), &mbp.WriteRequest{Vault: vault, Content: "a newly appended memory"}); err != nil {
		t.Errorf("append mode must allow Write (create): %v", err)
	}

	// Full mode is NOT append-forbidden (the gate is specific to append).
	if _, err := eng.Evolve(withMode(auth.ModeFull), vault, id, "full-mode rewrite", "reason", nil, ""); errors.Is(err, ErrAppendForbidden) {
		t.Errorf("full mode must not be append-forbidden")
	}
}
