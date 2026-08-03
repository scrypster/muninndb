package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWrite_UpsertMode_CreateThenEvolve verifies the Rev 2 upsert orchestrator
// end-to-end. The 0x2F forward index maps the upsert key to the CURRENT HEAD of
// the chain (never a fixed engram), so:
//   - first upsert (miss) creates a fresh engram, pins it → "upsert-created";
//   - second upsert with CHANGED content evolves the head (new ULID, predecessor
//     superseded) and atomically re-points 0x2F to the successor → "upsert-evolved".
//
// After the evolve, the 0x2F pointer targets the successor, recall sees only the
// successor (the predecessor is soft-deleted), and the supersedes association
// links successor → predecessor. The storage-layer primitives themselves
// (GetUpsertKey, PutUpsertKey, StoreBatch.RepointUpsertKey) are pinned in
// internal/storage/upsert_key_test.go.
func TestWrite_UpsertMode_CreateThenEvolve(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v1", Tags: []string{"red"},
		Confidence: 0.42, UpsertMode: true, IdempotentID: "doc-1",
	})
	if err != nil {
		t.Fatalf("first upsert Write: %v", err)
	}
	if resp1.Hint != "upsert-created" {
		t.Errorf("first Hint: got %q, want upsert-created", resp1.Hint)
	}
	if resp1.ID == "" {
		t.Fatal("first: empty ID")
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v2", Tags: []string{"green"},
		UpsertMode: true, IdempotentID: "doc-1",
	})
	if err != nil {
		t.Fatalf("second upsert Write: %v", err)
	}
	if resp2.Hint != "upsert-evolved" {
		t.Errorf("second Hint: got %q, want upsert-evolved", resp2.Hint)
	}
	if resp2.ID == resp1.ID {
		t.Errorf("evolve should mint a new ULID, got the same id %s", resp2.ID)
	}

	ws := store.ResolveVaultPrefix("")
	id1, _ := storage.ParseULID(resp1.ID)
	id2, _ := storage.ParseULID(resp2.ID)

	// The 0x2F pointer now targets the SUCCESSOR (the evolve re-pointed it in
	// the same atomic batch that wrote the successor).
	keyHash := sha256.Sum256([]byte("doc-1"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	if pinned != id2 {
		t.Errorf("upsert-key pin: got %x, want successor %x", pinned[:], id2[:])
	}

	// Successor is the current head — Active and carrying the new content.
	head, err := store.GetEngram(ctx, ws, id2)
	if err != nil {
		t.Fatalf("GetEngram successor: %v", err)
	}
	if head.Content != "v2" {
		t.Errorf("successor content: got %q, want v2", head.Content)
	}
	if head.State != storage.StateActive {
		t.Errorf("successor state: got %d, want StateActive", head.State)
	}

	// Predecessor is superseded — soft-deleted, hidden from the present.
	pred, err := store.GetEngram(ctx, ws, id1)
	if err != nil {
		t.Fatalf("GetEngram predecessor: %v", err)
	}
	if pred.State != storage.StateSoftDeleted {
		t.Errorf("predecessor state: got %d, want StateSoftDeleted (superseded)", pred.State)
	}
	if pred.Content != "v1" {
		t.Errorf("predecessor content mutated: got %q, want v1 (evolve creates a new engram, never mutates)", pred.Content)
	}

	// The supersedes association successor → predecessor is the chain link.
	assocs, err := store.GetAssociations(ctx, ws, []storage.ULID{id2}, 16)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	var sawSupersedes bool
	for _, a := range assocs[id2] {
		if a.RelType == storage.RelSupersedes && a.TargetID == id1 {
			sawSupersedes = true
		}
	}
	if !sawSupersedes {
		t.Errorf("supersedes association successor→predecessor missing; got %#v", assocs[id2])
	}
}

// TestWrite_UpsertMode_IdenticalContent_NoOp: a second upsert with byte-identical
// content is a no-op — returns the existing head id with Hint="upsert-identical",
// no new ULID minted, no evolve batch committed. Pinned by the orchestrator's
// content-hash fast path.
func TestWrite_UpsertMode_IdenticalContent_NoOp(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "same", UpsertMode: true, IdempotentID: "doc-same",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if resp1.Hint != "upsert-created" {
		t.Fatalf("first Hint: got %q, want upsert-created", resp1.Hint)
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "same", UpsertMode: true, IdempotentID: "doc-same",
	})
	if err != nil {
		t.Fatalf("identical re-write: %v", err)
	}
	if resp2.Hint != "upsert-identical" {
		t.Errorf("identical Hint: got %q, want upsert-identical", resp2.Hint)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("identical should return the same id: got %s, want %s", resp2.ID, resp1.ID)
	}

	// No successor was minted — the 0x2F pointer still targets the original.
	ws := store.ResolveVaultPrefix("")
	id1, _ := storage.ParseULID(resp1.ID)
	pinned, _ := store.GetUpsertKey(ctx, ws, sha256.Sum256([]byte("doc-same")))
	if pinned != id1 {
		t.Errorf("identical re-pointed the pointer: got %x, want %x", pinned[:], id1[:])
	}
}

// TestWrite_UpsertMode_StalePointerSoftDeleted_Recreates: when the 0x2F entry
// points at a non-Active (soft-deleted) head, the orchestrator must treat it as
// stale and create a fresh engram + re-pin the pointer — never evolve into a
// tombstone (RedTeam #556 Change-2: silent data loss otherwise).
func TestWrite_UpsertMode_StalePointerSoftDeleted_Recreates(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v1", UpsertMode: true, IdempotentID: "doc-stale",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id1, _ := storage.ParseULID(resp1.ID)
	ws := store.ResolveVaultPrefix("")

	// Soft-delete the head → the 0x2F entry now points at a tombstone.
	if err := store.SoftDelete(ctx, ws, id1); err != nil {
		t.Fatalf("soft-delete head: %v", err)
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v2", UpsertMode: true, IdempotentID: "doc-stale",
	})
	if err != nil {
		t.Fatalf("recreate after soft-delete: %v", err)
	}
	if resp2.Hint != "upsert-created" {
		t.Errorf("stale-pointer Hint: got %q, want upsert-created", resp2.Hint)
	}
	if resp2.ID == resp1.ID {
		t.Fatal("recreate should mint a new ULID, not reuse the tombstoned one")
	}
	id2, _ := storage.ParseULID(resp2.ID)

	// Pointer re-pinned to the fresh engram.
	pinned, _ := store.GetUpsertKey(ctx, ws, sha256.Sum256([]byte("doc-stale")))
	if pinned != id2 {
		t.Errorf("pointer not re-pinned: got %x, want %x", pinned[:], id2[:])
	}

	// The fresh engram is Active with the new content; the tombstone stays
	// soft-deleted (not mutated, not evolved into).
	got, _ := store.GetEngram(ctx, ws, id2)
	if got.State != storage.StateActive || got.Content != "v2" {
		t.Errorf("recreate head wrong: state=%d content=%q", got.State, got.Content)
	}
	tomb, _ := store.GetEngram(ctx, ws, id1)
	if tomb.State != storage.StateSoftDeleted || tomb.Content != "v1" {
		t.Errorf("tombstone mutated: state=%d content=%q", tomb.State, tomb.Content)
	}
}

// TestWrite_UpsertMode_StalePointerHardDeleted_Recreates: when the 0x2F entry
// points at a hard-deleted/absent engram (Forget removed 0x01, or ClearVault
// left 0x2F dangling), GetEngram returns (nil, ErrNotFound); the orchestrator
// must treat it as stale and recreate rather than error forever.
func TestWrite_UpsertMode_StalePointerHardDeleted_Recreates(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v1", UpsertMode: true, IdempotentID: "doc-hard",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id1, _ := storage.ParseULID(resp1.ID)
	ws := store.ResolveVaultPrefix("")

	// Hard-delete the pinned engram. DeleteEngram does NOT sweep 0x2F, so the
	// forward-index entry → id1 now dangles.
	if err := store.DeleteEngram(ctx, ws, id1); err != nil {
		t.Fatalf("DeleteEngram head: %v", err)
	}
	if _, gErr := store.GetEngram(ctx, ws, id1); !errors.Is(gErr, storage.ErrNotFound) {
		t.Fatalf("precondition: GetEngram after delete should be ErrNotFound, got %v", gErr)
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Concept: "c", Content: "v2", UpsertMode: true, IdempotentID: "doc-hard",
	})
	if err != nil {
		t.Fatalf("recreate after hard-delete: %v", err)
	}
	if resp2.Hint != "upsert-created" {
		t.Errorf("stale-pointer Hint: got %q, want upsert-created", resp2.Hint)
	}
	id2, _ := storage.ParseULID(resp2.ID)

	pinned, _ := store.GetUpsertKey(ctx, ws, sha256.Sum256([]byte("doc-hard")))
	if pinned != id2 {
		t.Errorf("pointer not re-pinned: got %x, want %x", pinned[:], id2[:])
	}
}

// TestWrite_UpsertMode_RequiresIdempotentID: upsert_mode without an
// idempotent_id is rejected — a bare upsert is a caller bug, fail loud.
func TestWrite_UpsertMode_RequiresIdempotentID(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()

	_, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Content: "x", UpsertMode: true, // no IdempotentID
	})
	if err == nil {
		t.Fatal("expected error when upsert_mode is set without idempotent_id")
	}
}

// TestWrite_UpsertMode_DefaultUnchanged: with upsert_mode=false (the default),
// two writes with the same idempotent_id take the legacy idempotent-receipt +
// content-hash dedup path, NOT the upsert branch (regression guard for the
// dispatch). The upsert Hints must never surface in default mode.
func TestWrite_UpsertMode_DefaultUnchanged(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Content: "a", IdempotentID: "op-1",
	})
	if err != nil {
		t.Fatalf("first default Write: %v", err)
	}
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Content: "b", IdempotentID: "op-1",
	})
	if err != nil {
		t.Fatalf("second default Write: %v", err)
	}
	// First call: no receipt yet → normal write (empty Hint). Second call: the
	// legacy receipt returns the same id with "idempotent". Neither must surface
	// an upsert hint (the branch stayed dormant for upsert_mode=false).
	if resp2.ID != resp1.ID {
		t.Errorf("legacy idempotency broke: second id %s != first %s", resp2.ID, resp1.ID)
	}
	if resp2.Hint != "idempotent" {
		t.Errorf("second Hint: got %q, want idempotent (legacy receipt)", resp2.Hint)
	}
	if resp1.Hint == "upsert-created" || resp1.Hint == "upsert-evolved" ||
		resp2.Hint == "upsert-created" || resp2.Hint == "upsert-evolved" {
		t.Errorf("upsert branch hijacked default mode: resp1=%q resp2=%q", resp1.Hint, resp2.Hint)
	}
}

// TestWriteBatch_Upsert_RoutesPerItem: a mixed batch — two upsert items sharing
// a key (intra-batch evolve) plus a default-mode item — routes correctly: upsert
// items go through writeUpsert (per-item, Phase 2 batch commit skips them), the
// default item takes the legacy path, and intra-batch same-key chaining works
// (item 1 sees item 0's just-committed forward-index entry and evolves it).
func TestWriteBatch_Upsert_RoutesPerItem(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.WriteRequest{
		{Concept: "c", Content: "bv1", UpsertMode: true, IdempotentID: "b-doc"},
		{Concept: "c", Content: "bv2", UpsertMode: true, IdempotentID: "b-doc"},
		{Concept: "d", Content: "plain"},
	}
	resps, errs := eng.WriteBatch(ctx, reqs)
	for i, e := range errs {
		if e != nil {
			t.Fatalf("item %d error: %v", i, e)
		}
	}
	if resps[0].Hint != "upsert-created" {
		t.Errorf("item0 Hint: got %q, want upsert-created", resps[0].Hint)
	}
	if resps[1].Hint != "upsert-evolved" {
		t.Errorf("item1 Hint: got %q, want upsert-evolved (intra-batch evolve)", resps[1].Hint)
	}
	if resps[1].ID == resps[0].ID {
		t.Errorf("intra-batch evolve should mint a new id: item0=%s item1=%s", resps[0].ID, resps[1].ID)
	}
	if resps[2].ID == "" || resps[2].ID == resps[0].ID {
		t.Errorf("default-mode item should get its own id: got %q", resps[2].ID)
	}

	// The pointer pins the SUCCESSOR (last write wins) and recall sees bv2.
	ws := store.ResolveVaultPrefix("")
	pinned, _ := store.GetUpsertKey(ctx, ws, sha256.Sum256([]byte("b-doc")))
	head, _ := store.GetEngram(ctx, ws, pinned)
	if head.Content != "bv2" {
		t.Errorf("upsert head content: got %q, want bv2", head.Content)
	}
	if head.ID.String() != resps[1].ID {
		t.Errorf("pinned id: got %s, want successor %s", head.ID.String(), resps[1].ID)
	}
}

// TestWrite_UpsertMode_ConcurrentSameKey_OneHead is the core concurrency proof:
// N goroutines Write the same upsert key at once. The upsertKeyLock must
// serialise them so the final 0x2F pointer targets exactly one Active head
// along a single chain — each write either creates (the first), evolves (the
// rest with changed content), or no-ops (the rest with identical content). No
// goroutine errors, no orphans pinned. Run with -race.
func TestWrite_UpsertMode_ConcurrentSameKey_OneHead(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	const N = 32
	ids := make([]string, N)
	hints := make([]string, N)
	errs := make([]error, N)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			resp, err := eng.Write(ctx, &mbp.WriteRequest{
				Content: fmt.Sprintf("v-%d", i), UpsertMode: true, IdempotentID: "race-key",
			})
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = resp.ID
			hints[i] = resp.Hint
		}()
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d errored: %v", i, e)
		}
	}

	// Exactly one engram is pinned as the head by the end.
	ws := store.ResolveVaultPrefix("")
	keyHash := sha256.Sum256([]byte("race-key"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	if pinned == (storage.ULID{}) {
		t.Fatal("no engram pinned to race-key after concurrent writes")
	}

	// Every non-empty response id must be either the current head or a known
	// predecessor along the chain (the evolve path superseded it). Walk the
	// supersedes chain back from the head to collect every legal id, then
	// assert each response id is in that set.
	legalIDs := map[storage.ULID]bool{pinned: true}
	cur := pinned
	for i := 0; i < N+1; i++ { // bound the walk well above any possible chain length
		assocs, aErr := store.GetAssociations(ctx, ws, []storage.ULID{cur}, 16)
		if aErr != nil {
			t.Fatalf("walking chain at %s: %v", cur.String(), aErr)
		}
		next := storage.ULID{}
		for _, a := range assocs[cur] {
			if a.RelType == storage.RelSupersedes {
				next = a.TargetID
				break
			}
		}
		if next == (storage.ULID{}) {
			break // end of chain
		}
		legalIDs[next] = true
		cur = next
	}

	for i, id := range ids {
		if id == "" {
			continue
		}
		u, _ := storage.ParseULID(id)
		if !legalIDs[u] {
			t.Errorf("goroutine %d returned id %s not on the pinned chain (head=%s)", i, id, pinned.String())
		}
	}

	// Exactly one create (the first writer to acquire the lock on an empty key).
	created := 0
	for _, h := range hints {
		if h == "upsert-created" {
			created++
		}
	}
	if created != 1 {
		t.Errorf("expected exactly 1 upsert-created (lock must serialise creates), got %d", created)
	}
}

// TestWriteUpsert_ExpiredHeadTakesCreateBranch pins the fix for a valid-time
// gap in writeUpsert's dispatch: it decided "is the head still usable" on
// head.State != StateActive ALONE. COG-19 not_true_since invalidation
// (muninn_forget's documented way to retire a fact without deleting it)
// deliberately leaves State=Active and only stamps ValidUntil. An invalidated
// head is therefore Active-but-invisible-to-default-recall, and the OLD
// dispatch took the identical-content no-op branch on it, stranding the
// upsert key: re-syncing the unchanged document returned "upsert-identical"
// forever, and the fact could only return to default recall if the CALLER
// EDITED THE TEXT — recovery conditional on a content change, not a policy
// choice.
//
// The fix mirrors the sibling predicate the default (non-upsert) content-hash
// dedup path already uses (this file's production code, engine.go ~line
// 1401): a head must be both non-soft-deleted AND not IsExpired(now) to be a
// live dedup target. An expired head takes the CREATE branch (not evolve —
// see the writeUpsert doc comment for why: evolving would manufacture a
// supersession edge from a fact already declared not-currently-true).
//
// "active_head_dedups" is the in-test control proving the ordinary
// content-hash no-op path is untouched by this fix.
func TestWriteUpsert_ExpiredHeadTakesCreateBranch(t *testing.T) {
	tests := []struct {
		name       string
		invalidate bool
		wantHint   string
		wantNewID  bool
	}{
		{name: "active_head_dedups", invalidate: false, wantHint: "upsert-identical", wantNewID: false},
		{name: "expired_head_recreates", invalidate: true, wantHint: "upsert-created", wantNewID: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, store, cleanup := testEnvWithStore(t)
			defer cleanup()
			ctx := context.Background()
			const key = "doc-valid-time"

			createdAt := time.Now().Add(-2 * time.Hour).UTC()
			resp1, err := eng.Write(ctx, &mbp.WriteRequest{
				Concept: "c", Content: "unchanged text", UpsertMode: true,
				IdempotentID: key, CreatedAt: &createdAt,
			})
			if err != nil {
				t.Fatalf("initial upsert: %v", err)
			}
			if resp1.Hint != "upsert-created" {
				t.Fatalf("initial Hint: got %q, want upsert-created", resp1.Hint)
			}

			if tc.invalidate {
				notTrueSince := time.Now().Add(-time.Hour).UTC()
				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
					ID: resp1.ID, NotTrueSince: &notTrueSince,
				}); err != nil {
					t.Fatalf("Forget(not_true_since): %v", err)
				}
			}

			// Re-sync the IDENTICAL, unchanged content — the ordinary re-ingest
			// case (the sync tool has no reason to know the fact was invalidated).
			resp2, err := eng.Write(ctx, &mbp.WriteRequest{
				Concept: "c", Content: "unchanged text", UpsertMode: true, IdempotentID: key,
			})
			if err != nil {
				t.Fatalf("re-sync upsert: %v", err)
			}
			if resp2.Hint != tc.wantHint {
				t.Errorf("re-sync Hint: got %q, want %q", resp2.Hint, tc.wantHint)
			}
			if gotNew := resp2.ID != resp1.ID; gotNew != tc.wantNewID {
				t.Errorf("re-sync minted new id = %v (id1=%s id2=%s), want %v", gotNew, resp1.ID, resp2.ID, tc.wantNewID)
			}

			ws := store.ResolveVaultPrefix("")
			id2, _ := storage.ParseULID(resp2.ID)
			pinned, err := store.GetUpsertKey(ctx, ws, sha256.Sum256([]byte(key)))
			if err != nil {
				t.Fatalf("GetUpsertKey: %v", err)
			}
			if pinned != id2 {
				t.Errorf("upsert key not pinned to the re-sync result: got %x, want %x", pinned[:], id2[:])
			}

			head, err := store.GetEngram(ctx, ws, id2)
			if err != nil {
				t.Fatalf("GetEngram head: %v", err)
			}
			if head.IsExpired(time.Now()) {
				t.Errorf("re-sync head is still expired — default recall remains permanently blind to this key")
			}
		})
	}
}
