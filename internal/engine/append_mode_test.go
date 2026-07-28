package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestAppendMode_RefusesEveryDestructiveOp is the completeness pin (both #687
// reviews): the append guarantee must hold at the ENGINE layer for every method
// that modifies or deletes an existing engram/entity/lease/enrichment, so it is
// transport-agnostic (the MCP dispatch gate alone left REST/gRPC/MBP open — the
// confirmed break). Each closure calls one such method under an append-mode ctx
// and must get ErrAppendForbidden. A newly-added destructive method that forgets
// the refuseAppend guard is caught by adding it here — and if it is destructive
// and NOT added here, that is the omission this test exists to make visible.
func TestAppendMode_RefusesEveryDestructiveOp(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ap := withMode(auth.ModeAppend)
	var z storage.ULID
	const v, id = "append-cov-vault", "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	ops := map[string]func() error{
		"UpdateTags":           func() error { return eng.UpdateTags(ap, v, z, []string{"t"}) },
		"AdjustConfidence":     func() error { _, e := eng.AdjustConfidence(ap, v, z, 0.1, z, false, "r", "c"); return e },
		"UpdateLifecycleState": func() error { return eng.UpdateLifecycleState(ap, v, id, "archived") },
		"SetTrust":             func() error { return eng.SetTrust(ap, v, id, "verified") },
		"Consolidate":          func() error { _, e := eng.Consolidate(ap, v, []string{id}, "m"); return e },
		"Decide":               func() error { _, e := eng.Decide(ap, v, "d", "r", nil, nil); return e },
		"RecordFeedback":       func() error { return eng.RecordFeedback(ap, v, id, true) },
		"SetEntityState":       func() error { return eng.SetEntityState(ap, "E", "merged", "", "") },
		"SetEntityStateBatch":  func() error { errs := eng.SetEntityStateBatch(ap, []EntityStateOp{{}}); return errs[0] },
		"CompareAndSet":        func() error { _, e := eng.CompareAndSet(ap, v, id, nil, nil); return e },
		"Claim":                func() error { _, e := eng.Claim(ap, v, id, "o", 60); return e },
		"Release":              func() error { _, e := eng.Release(ap, v, id, "o"); return e },
		"MergeEntity":          func() error { _, e := eng.MergeEntity(ap, v, "a", "b", false); return e },
		"ReplayEnrichment":     func() error { _, e := eng.ReplayEnrichment(ap, v, nil, 10, false); return e },
		"ApplyEnrichment":      func() error { _, e := eng.ApplyEnrichment(ap, v, &EnrichmentApplyRequest{}); return e },
		"Evolve":               func() error { _, e := eng.Evolve(ap, v, id, "new", "r", nil, ""); return e },
		"Forget":               func() error { _, e := eng.Forget(ap, &mbp.ForgetRequest{Vault: v, ID: id}); return e },
	}
	for name, fn := range ops {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrAppendForbidden) {
				t.Errorf("%s under append: err = %v, want ErrAppendForbidden", name, err)
			}
		})
	}
}

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
