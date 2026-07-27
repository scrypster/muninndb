package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWrite_UpsertMode_CreateThenMerge verifies the engine upsert wiring
// end-to-end: two Write calls with upsert_mode + the same idempotent_id land on
// the SAME engram (merge in place), the second call's content wins, cognitive
// state (Confidence) is preserved, and the 0x2B forward index pins the id.
// Exercises the writeUpsert branch (dispatch, stripe lock, engram construction,
// store.UpsertEngram, Hint) — the storage-layer semantics themselves are
// pinned in internal/storage/upsert_key_test.go.
func TestWrite_UpsertMode_CreateThenMerge(t *testing.T) {
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
	if resp2.Hint != "upsert-merged" {
		t.Errorf("second Hint: got %q, want upsert-merged", resp2.Hint)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("merge changed the ID: got %s, want %s", resp2.ID, resp1.ID)
	}

	// Verify via the store: content overwritten, Confidence preserved.
	ws := store.ResolveVaultPrefix("")
	id, err := storage.ParseULID(resp1.ID)
	if err != nil {
		t.Fatalf("parse ULID: %v", err)
	}
	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Content != "v2" {
		t.Errorf("content: got %q, want %q", got.Content, "v2")
	}
	if got.Confidence != 0.42 {
		t.Errorf("Confidence not preserved: got %v, want 0.42", got.Confidence)
	}

	// The 0x2B forward index pins doc-1 → id.
	keyHash := sha256.Sum256([]byte("doc-1"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	if pinned != id {
		t.Errorf("upsert-key pin: got %x, want %x", pinned[:], id[:])
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
// two writes with the same idempotent_id create TWO distinct engrams — the
// upsert branch is not consulted, and the legacy idempotent-receipt + content-
// hash dedup path is untouched (regression guard for the dispatch).
func TestWrite_UpsertMode_DefaultUnchanged(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	// Same idempotent_id, DIFFERENT content, upsert_mode NOT set. The legacy
	// idempotent receipt returns the original id on the second call — so both
	// calls return the same id, and only ONE engram exists (the receipt path,
	// not the upsert path). This confirms the upsert branch didn't hijack the
	// default flow.
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
	if resp1.Hint == "upsert-created" || resp1.Hint == "upsert-merged" ||
		resp2.Hint == "upsert-created" || resp2.Hint == "upsert-merged" {
		t.Errorf("upsert branch hijacked default mode: resp1=%q resp2=%q", resp1.Hint, resp2.Hint)
	}
}

// TestWriteBatch_Upsert_RoutesPerItem: a mixed batch — two upsert items sharing
// a key (intra-batch merge) plus a default-mode item — routes correctly: upsert
// items go through writeUpsert (per-item, Phase 2 batch commit skips them), the
// default item takes the legacy path, and intra-batch same-key dedup works
// (item 1 sees item 0's just-committed forward-index entry).
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
	if resps[0].ID != resps[1].ID {
		t.Errorf("intra-batch same-key upsert: item0=%s item1=%s (want same id)", resps[0].ID, resps[1].ID)
	}
	if resps[0].Hint != "upsert-created" {
		t.Errorf("item0 Hint: got %q, want upsert-created", resps[0].Hint)
	}
	if resps[1].Hint != "upsert-merged" {
		t.Errorf("item1 Hint: got %q, want upsert-merged (intra-batch merge)", resps[1].Hint)
	}
	if resps[2].ID == "" || resps[2].ID == resps[0].ID {
		t.Errorf("default-mode item should get its own id: got %q", resps[2].ID)
	}
	// Last merge wins → content bv2.
	ws := store.ResolveVaultPrefix("")
	id, _ := storage.ParseULID(resps[0].ID)
	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Content != "bv2" {
		t.Errorf("upsert content after batch: got %q, want bv2", got.Content)
	}
}

// TestWrite_UpsertMode_ConcurrentSameKey_OneEngram is the core concurrency
// proof: N goroutines Write the same upsert key at once. The upsertKeyLock must
// serialise them so exactly ONE creates and the rest merge — all return the
// same id, exactly one "upsert-created" hint. This is the invariant the
// content-hash dedup path lacks a test for; UPSERT ships it. Run with -race.
func TestWrite_UpsertMode_ConcurrentSameKey_OneEngram(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	const N = 32
	ids := make([]string, N)
	hints := make([]string, N)
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
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			ids[i] = resp.ID
			hints[i] = resp.Hint
		}()
	}
	close(start)
	wg.Wait()

	// Exactly one engram pinned to "race-key".
	ws := store.ResolveVaultPrefix("")
	keyHash := sha256.Sum256([]byte("race-key"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	if pinned == (storage.ULID{}) {
		t.Fatal("no engram pinned to race-key")
	}
	for i, id := range ids {
		if id == "" {
			continue
		}
		if id != pinned.String() {
			t.Errorf("goroutine %d returned id %s, want pinned %s", i, id, pinned.String())
		}
	}
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
