package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWrite_UpsertMode_DistinctKeysSameContentStayDistinct pins the rule that
// upsert identity is the KEY, never the content (#556).
//
// The create branch delegates to the default Write path, which runs content-hash
// dedup. Left ungated, two distinct upsert keys whose documents carry
// byte-identical text collapse onto ONE engram and both pins alias it. They then
// share fate: the first key whose document changes evolves the shared engram and
// soft-deletes it out from under the other key. The other key's content vanishes
// from default recall until its own next sync, then returns under a NEW ULID —
// destroying the stable identity the feature exists to provide — and the
// predecessor carries a FALSE RelSupersedes claim that document A was superseded
// by document B's revision.
//
// Duplicate text across chunks (boilerplate, licence blocks, repeated headings)
// is ordinary in re-ingest corpora, so this is routine, not a corner case.
//
// RED: drop the `!skipContentDedup(ctx)` guard on the dedup lookup in Write and
// this fails at the first assertion with the two keys collapsed onto one ULID.
func TestWrite_UpsertMode_DistinctKeysSameContentStayDistinct(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const shared = "a paragraph that appears verbatim in two source documents"

	a, err := eng.Write(ctx, &mbp.WriteRequest{Concept: "c", Content: shared, UpsertMode: true, IdempotentID: "docA"})
	if err != nil {
		t.Fatalf("upsert docA: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Concept: "c", Content: shared, UpsertMode: true, IdempotentID: "docB"})
	if err != nil {
		t.Fatalf("upsert docB: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("distinct upsert keys collapsed onto one engram %s — content-hash dedup must not "+
			"apply to an identity-addressed create", a.ID)
	}

	ws := store.ResolveVaultPrefix("")
	for key, want := range map[string]string{"docA": a.ID, "docB": b.ID} {
		h := sha256.Sum256([]byte(key))
		pinned, err := store.GetUpsertKey(ctx, ws, h)
		if err != nil {
			t.Fatalf("GetUpsertKey(%s): %v", key, err)
		}
		if pinned.String() != want {
			t.Errorf("pin[%s]: got %s, want %s", key, pinned, want)
		}
	}

	// docB changes. docA must be untouched — still Active, still its own ULID.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "docB has been revised", UpsertMode: true, IdempotentID: "docB",
	}); err != nil {
		t.Fatalf("upsert docB revision: %v", err)
	}

	idA, _ := storage.ParseULID(a.ID)
	engA, err := store.GetEngram(ctx, ws, idA)
	if err != nil {
		t.Fatalf("docA head unreadable after an unrelated key's revision: %v", err)
	}
	if engA.State != storage.StateActive {
		t.Errorf("docA head state = %v, want active — an unrelated key's revision must not "+
			"supersede it", engA.State)
	}
	if !engA.ValidUntil.IsZero() {
		t.Errorf("docA head has a CLOSED ValidUntil — it was falsely superseded by docB's revision")
	}
}

// TestWrite_UpsertMode_EvolveClosesValidUntil pins the #856 declared-supersession
// signature on the upsert content-change path: the predecessor must carry BOTH a
// RelSupersedes edge (pinned by TestWrite_UpsertMode_CreateThenEvolve) and a
// CLOSED ValidUntil. Soft-deleted with an open stamp is the signature COG-28
// reads as "trash, not history", which would put the old content beyond as_of
// and include_invalid.
func TestWrite_UpsertMode_EvolveClosesValidUntil(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	r1, err := eng.Write(ctx, &mbp.WriteRequest{Concept: "c", Content: "v1", UpsertMode: true, IdempotentID: "d1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Concept: "c", Content: "v2", UpsertMode: true, IdempotentID: "d1"}); err != nil {
		t.Fatalf("evolve: %v", err)
	}

	ws := store.ResolveVaultPrefix("")
	id1, _ := storage.ParseULID(r1.ID)
	pred, err := store.GetEngram(ctx, ws, id1)
	if err != nil {
		t.Fatalf("GetEngram predecessor: %v", err)
	}
	if pred.ValidUntil.IsZero() {
		t.Errorf("predecessor has an OPEN ValidUntil after an upsert-evolve — a content-replacing " +
			"operation must DECLARE supersession (#856): closed stamp + RelSupersedes edge")
	}
}

// TestWrite_UpsertMode_AppendCredential pins SEC-15 across the three upsert
// branches: an append-mode credential MAY create (upsert is the create path
// there) and MAY re-submit identical content (a strict no-op), but MUST be
// refused the content-change branch, which evolves an existing engram.
func TestWrite_UpsertMode_AppendCredential(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ap := withMode(auth.ModeAppend)

	if _, err := eng.Write(ap, &mbp.WriteRequest{Content: "v1", UpsertMode: true, IdempotentID: "d1"}); err != nil {
		t.Fatalf("append-mode upsert create must be allowed: %v", err)
	}
	if _, err := eng.Write(ap, &mbp.WriteRequest{Content: "v1", UpsertMode: true, IdempotentID: "d1"}); err != nil {
		t.Errorf("append-mode upsert no-op must be allowed: %v", err)
	}
	_, err := eng.Write(ap, &mbp.WriteRequest{Content: "v2-changed", UpsertMode: true, IdempotentID: "d1"})
	if !errors.Is(err, ErrAppendForbidden) {
		t.Errorf("append-mode upsert content-change: err = %v, want ErrAppendForbidden", err)
	}
}
