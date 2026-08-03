package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeTestEngram writes a minimal valid Engram and returns its assigned ULID.
func writeTestEngram(t *testing.T, store *PebbleStore, ws [8]byte, concept, content string) ULID {
	t.Helper()
	eng := &Engram{
		Concept: concept,
		Content: content,
	}
	id, err := store.WriteEngram(context.Background(), ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram(%q): %v", concept, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// GetEngrams
// ---------------------------------------------------------------------------

// TestGetEngrams_Roundtrip writes 3 engrams and verifies GetEngrams returns all 3.
func TestGetEngrams_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-roundtrip")

	id1 := writeTestEngram(t, store, ws, "concept-a", "content-a")
	id2 := writeTestEngram(t, store, ws, "concept-b", "content-b")
	id3 := writeTestEngram(t, store, ws, "concept-c", "content-c")

	results, err := store.GetEngrams(ctx, ws, []ULID{id1, id2, id3})
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Each slot must be non-nil and must match the expected concept.
	expected := []struct {
		id      ULID
		concept string
	}{
		{id1, "concept-a"},
		{id2, "concept-b"},
		{id3, "concept-c"},
	}
	for i, e := range expected {
		if results[i] == nil {
			t.Errorf("slot %d: expected non-nil engram for id %v", i, e.id)
			continue
		}
		if results[i].ID != e.id {
			t.Errorf("slot %d: ID mismatch: got %v, want %v", i, results[i].ID, e.id)
		}
		if results[i].Concept != e.concept {
			t.Errorf("slot %d: Concept mismatch: got %q, want %q", i, results[i].Concept, e.concept)
		}
	}
}

// TestGetEngrams_EmptyInput verifies that an empty ULID slice returns an empty
// result slice with no error.
func TestGetEngrams_EmptyInput(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-empty")

	results, err := store.GetEngrams(ctx, ws, []ULID{})
	if err != nil {
		t.Fatalf("GetEngrams(empty): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty result, got %d elements", len(results))
	}
}

// TestGetEngrams_DanglingID verifies that a non-existent ID causes that slot to
// be nil while the other slots are populated correctly.
func TestGetEngrams_DanglingID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-dangling")

	id1 := writeTestEngram(t, store, ws, "real-concept", "real-content")
	dangling := NewULID() // never written

	// Request real ID first, then the dangling one.
	results, err := store.GetEngrams(ctx, ws, []ULID{id1, dangling})
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 result slots, got %d", len(results))
	}
	if results[0] == nil {
		t.Error("slot 0 (real id): expected non-nil engram")
	}
	if results[1] != nil {
		t.Errorf("slot 1 (dangling id): expected nil, got %+v", results[1])
	}
}

// ---------------------------------------------------------------------------
// UpdateMetadata
// ---------------------------------------------------------------------------

// TestUpdateMetadata_ChangesConfidence writes an engram, updates its confidence
// via UpdateMetadata, and verifies the change is visible through GetEngram.
func TestUpdateMetadata_ChangesConfidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatemeta-confidence")

	id := writeTestEngram(t, store, ws, "meta-concept", "meta-content")

	// Read current metadata to build an updated copy.
	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(metas) == 0 || metas[0] == nil {
		t.Fatal("GetMetadata returned nothing for newly-written engram")
	}

	// Patch only confidence; copy all other fields.
	updated := *metas[0]
	updated.Confidence = 0.5

	if err := store.UpdateMetadata(ctx, ws, id, &updated); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	// Verify via GetEngram (bypasses cache because UpdateMetadata invalidates it).
	eng, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after UpdateMetadata: %v", err)
	}
	if eng.Confidence != 0.5 {
		t.Errorf("Confidence: got %v, want 0.5", eng.Confidence)
	}
}

// TestUpdateMetadata_NotFound verifies that UpdateMetadata returns an error for
// a non-existent engram ID.
func TestUpdateMetadata_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatemeta-notfound")

	ghost := NewULID()
	meta := &EngramMeta{
		ID:         ghost,
		Confidence: 0.9,
		State:      StateActive,
	}
	err := store.UpdateMetadata(ctx, ws, ghost, meta)
	if err == nil {
		t.Fatal("expected error when updating non-existent engram, got nil")
	}
}

// ---------------------------------------------------------------------------
// UpdateTags
// ---------------------------------------------------------------------------

// TestUpdateTags_ReplacesTags writes an engram with tags ["a","b"], then calls
// UpdateTags with ["c","d"], and verifies the new tags via GetEngram.
func TestUpdateTags_ReplacesTags(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-replace")

	eng := &Engram{
		Concept: "tag-concept",
		Content: "tag-content",
		Tags:    []string{"a", "b"},
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if err := store.UpdateTags(ctx, ws, id, []string{"c", "d"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after UpdateTags: %v", err)
	}

	wantTags := map[string]bool{"c": true, "d": true}
	if len(got.Tags) != 2 {
		t.Errorf("expected 2 tags after update, got %d: %v", len(got.Tags), got.Tags)
	}
	for _, tag := range got.Tags {
		if !wantTags[tag] {
			t.Errorf("unexpected tag %q in updated engram", tag)
		}
	}
}

// TestUpdateTags_NotFound verifies that UpdateTags returns an error for a
// non-existent engram ID.
func TestUpdateTags_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-notfound")

	ghost := NewULID()
	err := store.UpdateTags(ctx, ws, ghost, []string{"tag1"})
	if err == nil {
		t.Fatal("expected error when updating tags of non-existent engram, got nil")
	}
}

// TestUpdateTags_RejectedTagDoesNotPoisonCache pins the error path: a call that
// FAILS validation must leave every reader seeing the original tags.
//
// The defect this closes: GetEngram hands back the L1 cache's live entry
// (DomainCache.Get does not clone), and UpdateTags mutated eng.Tags on that very
// object before validating anything. The WriteRawTagIndexEntry error branch then
// returned ahead of the cache invalidation, so the rejected tags stayed cached.
//
// It is reachable straight from MCP: normalizeTags checks only type, emptiness,
// and byte length, and ValidateRawTagValue rejects a NUL byte inside a
// `key:value` tag's VALUE — which a JSON `"<NUL>"` delivers intact.
//
// RED before the fix (measured):
//
//	baseline tags = [env:prod]
//	UpdateTags with NUL tag returned err = raw tag index: tag value contains a
//	  NUL byte, rejected
//	AFTER FAILED CALL, GetEngram (L1 cache) tags = [env:sta ging]
//	AFTER FAILED CALL, Pebble record tags      = [env:prod]
//
// Why that is the worst failure class in the project and not a cosmetic nit: the
// call returns an error, so the caller believes nothing changed — yet until
// eviction muninn_read echoes the rejected tags AND
// activation.PassesMetaFilter, which re-checks the poisoned eng.Tags, filters
// the engram OUT of a `tags_all: ["env:prod"]` recall. An error-returning call
// producing a silent false negative is strictly worse than a loud failure.
func TestUpdateTags_RejectedTagDoesNotPoisonCache(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-cache-poison")

	eng := &Engram{
		Concept: "release checklist",
		Content: "the checklist covers the staged rollout",
		Tags:    []string{"env:prod"},
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Warm the L1 cache, exactly as a recall would.
	if _, err := store.GetEngram(ctx, ws, id); err != nil {
		t.Fatalf("GetEngram (warm cache): %v", err)
	}

	rejected := []string{"env:sta\x00ging"}
	if err := store.UpdateTags(ctx, ws, id, rejected); err == nil {
		t.Fatal("UpdateTags with a NUL byte in a key:value tag's value returned nil, want rejection")
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after the failed UpdateTags: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "env:prod" {
		t.Errorf("after a FAILED UpdateTags, GetEngram tags = %q, want [env:prod] — the cache is serving tags that were rejected and never committed", got.Tags)
	}
}

// TestUpdateTags_RecachesCommittedRecord pins the OTHER half of the same cache
// handling: after a SUCCESSFUL retag the post-commit ps.cache.Set must leave the
// committed record in the L1 cache, carrying the new tags.
//
// Why this has to be an in-package test reading ps.cache directly rather than a
// GetEngram assertion: with the post-commit Set removed the entry is merely
// ABSENT — the pre-commit Delete stood, nothing put anything back, and
// `committed = true` still runs so the deferred invalidation is a no-op — so the
// next GetEngram misses the cache, reads Pebble, and returns the correct NEW
// tags. A GetEngram-based assertion passes in both states and proves nothing.
// The distinction that matters is present-and-fresh vs absent, and only the cache
// accessor can see it.
//
// What the absence costs in production, and why "the next read repopulates it
// correctly" is not a defence: the invalidate sits BEFORE the commit and readers
// do not take casLocks (recall calls GetEngram/GetEngrams constantly). A reader
// landing in that window misses, reads the PRE-commit Pebble value, and re-caches
// the OLD tags — which then stay cached against a committed new tag set until
// something else evicts them. The post-commit Set closes the window by
// overwriting whatever such a racing reader left behind, under the same stripe
// lock (inside it, per UpdateConfidence's reasoning: outside, a racing
// DeleteEngram's post-commit Delete could land first and this Set would re-cache
// an engram Pebble has already deleted).
//
// RED with the ps.cache.Set line removed (measured):
//
//	L1 cache has NO entry after a successful UpdateTags — the post-commit
//	  re-cache is missing
//
// GREEN: the L1 entry is present and carries ["env:staging"].
func TestUpdateTags_RecachesCommittedRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-recache")

	eng := &Engram{
		Concept: "release checklist",
		Content: "the checklist covers the staged rollout",
		Tags:    []string{"env:prod"},
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Warm the L1 cache, exactly as a recall would.
	if _, err := store.GetEngram(ctx, ws, id); err != nil {
		t.Fatalf("GetEngram (warm cache): %v", err)
	}

	if err := store.UpdateTags(ctx, ws, id, []string{"env:staging"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	cached, ok := store.cache.Get(ws, id)
	if !ok {
		t.Fatal("L1 cache has NO entry after a successful UpdateTags — the post-commit re-cache is missing, so the pre-commit invalidate window stays open for an unlocked reader to repopulate with the PRE-commit tags")
	}
	if len(cached.Tags) != 1 || cached.Tags[0] != "env:staging" {
		t.Errorf("L1 cache entry tags = %q after a successful UpdateTags, want [env:staging]", cached.Tags)
	}
}

// TestUpdateTags_ConcurrentSoftDeleteDoesNotResurrect pins [STO-2]/[STO-3] for
// UpdateTags: it must hold casLocks.For(id) across read→commit, as SoftDelete,
// UpdateConfidence, TouchAccess, AdjustConfidence, CompareAndSet and
// DeleteEngram all do.
//
// UpdateTags re-encodes the FULL record (toERFEngram → erf.Encode), so it writes
// back every field from its snapshot — State included. Unlocked, that snapshot
// can predate a committed SoftDelete, and the write-back resurrects the record
// while the 0x0B state index still says soft_deleted: the #594 resurrection race
// reopened by a new unlocked state-mutating path. The user-visible symptom is
// muninn_forget reporting success, muninn_list_deleted showing the memory as
// deleted, and recall continuing to return it.
//
// The invariant's text says "state or lease" and tags are neither, but the live
// code writes State unconditionally, so the invariant's intent applies — the
// code is what governs.
//
// RED without the lock (measured, plain `go test`, no -race needed — it fails
// deterministically enough that -race is not worth the CI minutes; two samples
// were 35/200 and 36/200):
//
//	LOST UPDATE / RESURRECTION: 35/200 engrams ended State != soft_deleted after
//	a committed SoftDelete
//	RECORD/INDEX DIVERGENCE: 35/200 soft_deleted in the 0x0B index but not in
//	the 0x02 record
//	(and 44/200 lost the tag update entirely)
func TestUpdateTags_ConcurrentSoftDeleteDoesNotResurrect(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-resurrection")

	const n = 200
	ids := make([]ULID, n)
	for i := range ids {
		ids[i] = writeTestEngram(t, store, ws, fmt.Sprintf("resurrect-%d", i), "body")
	}

	// Release both writers at once per engram so the read-modify-write windows
	// actually overlap.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func(id ULID) {
			defer wg.Done()
			<-start
			_ = store.UpdateTags(ctx, ws, id, []string{"retagged"})
		}(id)
		go func(id ULID) {
			defer wg.Done()
			<-start
			_ = store.SoftDelete(ctx, ws, id)
		}(id)
	}
	close(start)
	wg.Wait()

	// Whichever order the two calls serialize in, the committed outcome is the
	// same: SoftDelete last writes soft_deleted; UpdateTags last re-reads
	// soft_deleted under the lock and passes it through. The tag write survives
	// either way too, so the lock does not cost the update it was protecting.
	soft, err := indexedStateSet(ctx, store, ws, StateSoftDeleted)
	if err != nil {
		t.Fatalf("ScanEngramsByState: %v", err)
	}

	resurrected, diverged, lostTags := 0, 0, 0
	for _, id := range ids {
		// Read the authoritative record, not the L1 cache: SoftDelete
		// repopulates the cache post-commit, and the divergence under test is
		// between the 0x02 record and the 0x0B index.
		store.cache.Delete(ws, id)
		store.metaCache.Remove([16]byte(id))
		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEngram(%v): %v", id, err)
		}
		if eng.State != StateSoftDeleted {
			resurrected++
			if soft[id] {
				diverged++
			}
		}
		if len(eng.Tags) != 1 || eng.Tags[0] != "retagged" {
			lostTags++
		}
	}
	if resurrected != 0 {
		t.Errorf("LOST UPDATE / RESURRECTION: %d/%d engrams ended State != soft_deleted after a committed SoftDelete — UpdateTags wrote back a stale unlocked snapshot [STO-2]", resurrected, n)
	}
	if diverged != 0 {
		t.Errorf("RECORD/INDEX DIVERGENCE: %d/%d engrams are soft_deleted in the 0x0B state index but not in the 0x02 record [STO-3]", diverged, n)
	}
	if lostTags != 0 {
		t.Errorf("%d/%d engrams lost the tag update entirely", lostTags, n)
	}
}

// TestUpdateTags_ConcurrentTouchAccessPreservesAccessCount pins the other half
// of the same full-record write-back: UpdateTags snapshots AccessCount and
// LastAccess too, so unlocked it reverts a concurrent TouchAccess reinforcement.
// This is the assertion behind the changelog's claim that a retag preserves the
// access history — single-threaded that is trivially true; under concurrency it
// only holds because of the stripe lock.
//
// RED without the lock: 95/200 engrams had AccessCount reverted to 0.
func TestUpdateTags_ConcurrentTouchAccessPreservesAccessCount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-accesscount")

	const n = 200
	ids := make([]ULID, n)
	for i := range ids {
		ids[i] = writeTestEngram(t, store, ws, fmt.Sprintf("touch-%d", i), "body")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(2)
		go func(id ULID) {
			defer wg.Done()
			<-start
			_ = store.UpdateTags(ctx, ws, id, []string{"retagged"})
		}(id)
		go func(id ULID) {
			defer wg.Done()
			<-start
			_ = store.TouchAccess(ctx, ws, id)
		}(id)
	}
	close(start)
	wg.Wait()

	reverted := 0
	for _, id := range ids {
		store.cache.Delete(ws, id)
		store.metaCache.Remove([16]byte(id))
		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEngram(%v): %v", id, err)
		}
		// Serialized either way the count is exactly 1: TouchAccess first then
		// UpdateTags passes it through; UpdateTags first then TouchAccess
		// increments 0 → 1.
		if eng.AccessCount != 1 {
			reverted++
		}
	}
	if reverted != 0 {
		t.Errorf("ACCESS HISTORY REVERTED: %d/%d engrams have AccessCount != 1 after a concurrent TouchAccess — UpdateTags wrote back a stale unlocked snapshot", reverted, n)
	}
}

// indexedStateSet collects the ids the 0x0B state secondary index lists in the
// given lifecycle state.
func indexedStateSet(ctx context.Context, store *PebbleStore, ws [8]byte, state LifecycleState) (map[ULID]bool, error) {
	out := make(map[ULID]bool)
	err := store.ScanEngramsByState(ctx, ws, state, func(id ULID) error {
		out[id] = true
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// GetEmbedding
// ---------------------------------------------------------------------------

// TestGetEmbedding_NoEmbedding verifies that GetEmbedding returns nil (not an
// error) when no embedding was stored for an engram.
func TestGetEmbedding_NoEmbedding(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembedding-none")

	// Write an engram without an embedding.
	id := writeTestEngram(t, store, ws, "no-embed-concept", "no-embed-content")

	embedding, err := store.GetEmbedding(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if embedding != nil {
		t.Errorf("expected nil embedding, got slice of length %d", len(embedding))
	}
}

// TestGetEmbedding_Roundtrip writes an engram with an embedding and verifies
// that GetEmbedding returns values that are close to the originals.
// The storage layer uses quantized int8; values are approximate after round-trip.
func TestGetEmbedding_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembedding-roundtrip")

	want := []float32{0.1, 0.5, -0.3, 0.9, -0.7}
	eng := &Engram{
		Concept:   "embed-concept",
		Content:   "embed-content",
		Embedding: want,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram with embedding: %v", err)
	}

	got, err := store.GetEmbedding(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil embedding, got nil")
	}
	if len(got) != len(want) {
		t.Fatalf("embedding length mismatch: got %d, want %d", len(got), len(want))
	}
	// int8 quantization introduces ~0.01 error; allow 0.02 tolerance.
	const tolerance = 0.02
	for i := range want {
		diff := got[i] - want[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			t.Errorf("embedding[%d]: got %v, want %v (diff %v > tolerance %v)", i, got[i], want[i], diff, tolerance)
		}
	}
}

// TestGetEmbeddings_MatchesIndividualReads writes several engrams (some with
// embeddings, some without, via ERF v2), then confirms GetEmbeddings returns
// vectors positionally aligned with the requested ids and equal to what N
// individual GetEmbedding calls would return -- including a nil/empty slot for
// an id with no stored embedding and an unknown id mixed into the batch.
func TestGetEmbeddings_MatchesIndividualReads(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembeddings-batch")

	vecA := []float32{0.1, 0.5, -0.3, 0.9, -0.7}
	vecB := []float32{-0.2, 0.4, 0.6, -0.8, 0.1}

	idA, err := store.WriteEngram(ctx, ws, &Engram{Concept: "a", Content: "a-content", Embedding: vecA})
	if err != nil {
		t.Fatalf("WriteEngram A: %v", err)
	}
	idB, err := store.WriteEngram(ctx, ws, &Engram{Concept: "b", Content: "b-content", Embedding: vecB})
	if err != nil {
		t.Fatalf("WriteEngram B: %v", err)
	}
	// No embedding at all.
	idNoEmbed := writeTestEngram(t, store, ws, "no-embed", "no-embed-content")
	// Never written -- unknown id.
	idUnknown := NewULID()

	ids := []ULID{idA, idNoEmbed, idB, idUnknown}
	batch, err := store.GetEmbeddings(ctx, ws, ids)
	if err != nil {
		t.Fatalf("GetEmbeddings: %v", err)
	}
	if len(batch) != len(ids) {
		t.Fatalf("GetEmbeddings returned %d entries, want %d", len(batch), len(ids))
	}

	// Positional checks against individual GetEmbedding calls.
	for i, id := range ids {
		individual, err := store.GetEmbedding(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEmbedding(%d): %v", i, err)
		}
		if len(batch[i]) != len(individual) {
			t.Fatalf("id %d: batch len %d != individual len %d", i, len(batch[i]), len(individual))
		}
		for j := range individual {
			if batch[i][j] != individual[j] {
				t.Errorf("id %d component %d: batch %v != individual %v", i, j, batch[i][j], individual[j])
			}
		}
	}

	// idNoEmbed and idUnknown must both be nil/empty, never an error.
	if len(batch[1]) != 0 {
		t.Errorf("expected empty embedding for engram with no embedding, got len %d", len(batch[1]))
	}
	if len(batch[3]) != 0 {
		t.Errorf("expected empty embedding for unknown id, got len %d", len(batch[3]))
	}

	// idA and idB must round-trip within quantization tolerance.
	const tolerance = 0.02
	for _, pair := range []struct {
		got, want []float32
	}{
		{batch[0], vecA},
		{batch[2], vecB},
	} {
		if len(pair.got) != len(pair.want) {
			t.Fatalf("length mismatch: got %d, want %d", len(pair.got), len(pair.want))
		}
		for i := range pair.want {
			diff := pair.got[i] - pair.want[i]
			if diff < 0 {
				diff = -diff
			}
			if diff > tolerance {
				t.Errorf("component %d: got %v, want %v (diff > tolerance)", i, pair.got[i], pair.want[i])
			}
		}
	}
}

// TestGetEmbeddings_Empty verifies GetEmbeddings handles an empty id slice
// without error.
func TestGetEmbeddings_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembeddings-empty")

	got, err := store.GetEmbeddings(ctx, ws, nil)
	if err != nil {
		t.Fatalf("GetEmbeddings(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result for empty id slice, got len %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// DeleteEngram
// ---------------------------------------------------------------------------

// TestDeleteEngram_RemovesRecord writes then hard-deletes an engram and verifies
// that a subsequent GetEngram returns an error.
func TestDeleteEngram_RemovesRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("deleteengram-removes")

	id := writeTestEngram(t, store, ws, "to-delete", "will be gone")

	// Sanity check: the engram exists before deletion.
	if _, err := store.GetEngram(ctx, ws, id); err != nil {
		t.Fatalf("GetEngram before delete: %v", err)
	}

	if err := store.DeleteEngram(ctx, ws, id); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// After deletion, GetEngram must return an error.
	_, err := store.GetEngram(ctx, ws, id)
	if err == nil {
		t.Fatal("expected error from GetEngram after delete, got nil")
	}
}

// TestDeleteEngram_RemovesLeaseSidecar verifies that hard-deleting a leased
// engram removes its ownership-lease sidecar too, so a stale-owner claim
// cannot orphan a lease key.
func TestDeleteEngram_RemovesLeaseSidecar(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("deleteengram-lease")

	id := writeTestEngram(t, store, ws, "leased", "will be deleted while leased")

	lease := Lease{Owner: "hostA:sess1", Heartbeat: 1000, TTLSeconds: 60}
	if _, err := store.CompareAndSet(ctx, ws, id, CASCondition{}, CASMutation{Lease: &lease}); err != nil {
		t.Fatalf("CompareAndSet put lease: %v", err)
	}

	if err := store.DeleteEngram(ctx, ws, id); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	got, err := store.GetLease(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetLease after delete: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected lease sidecar to be gone after DeleteEngram, got %+v", got)
	}
}

// TestDeleteEngram_AutoCleansOrdinalKey verifies that DeleteEngram atomically removes
// any ordinal keys where the deleted engram is the child.
func TestDeleteEngram_AutoCleansOrdinalKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("auto-ordinal-clean")

	// Write parent and child engrams.
	parentEng := &Engram{Concept: "parent", Content: "root"}
	parentID, err := store.WriteEngram(ctx, ws, parentEng)
	if err != nil {
		t.Fatal(err)
	}
	childEng := &Engram{Concept: "child", Content: "leaf"}
	childID, err := store.WriteEngram(ctx, ws, childEng)
	if err != nil {
		t.Fatal(err)
	}

	// Write ordinal key: parent → child at ordinal 1.
	if err := store.WriteOrdinal(ctx, ws, parentID, childID, 1); err != nil {
		t.Fatal(err)
	}

	// Verify ordinal exists.
	_, found, err := store.ReadOrdinal(ctx, ws, parentID, childID)
	if err != nil || !found {
		t.Fatalf("ordinal should exist after write: err=%v found=%v", err, found)
	}

	// Hard-delete the child engram.
	if err := store.DeleteEngram(ctx, ws, childID); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// Ordinal key must be gone automatically.
	_, found, err = store.ReadOrdinal(ctx, ws, parentID, childID)
	if err != nil {
		t.Fatalf("ReadOrdinal after delete: %v", err)
	}
	if found {
		t.Error("ordinal key should be automatically removed when child engram is deleted")
	}
}

// TestDeleteEngram_AutoCleansParentOrdinalKeys verifies that DeleteEngram atomically
// removes all ordinal keys where the deleted engram was the parent
// (i.e. keys of the form 0x1E|ws|deletedID|childID).
func TestDeleteEngram_AutoCleansParentOrdinalKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("auto-parent-ordinal-clean")

	// Write parent P and children C1, C2.
	parentID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "parent", Content: "root"})
	if err != nil {
		t.Fatal(err)
	}
	c1ID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "child1", Content: "leaf1"})
	if err != nil {
		t.Fatal(err)
	}
	c2ID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "child2", Content: "leaf2"})
	if err != nil {
		t.Fatal(err)
	}

	// Register both children under P with ordinals.
	if err := store.WriteOrdinal(ctx, ws, parentID, c1ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteOrdinal(ctx, ws, parentID, c2ID, 2); err != nil {
		t.Fatal(err)
	}

	// Sanity check: both ordinal keys exist before deleting the parent.
	entries, err := store.ListChildOrdinals(ctx, ws, parentID)
	if err != nil {
		t.Fatalf("ListChildOrdinals before delete: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 child ordinals before delete, got %d", len(entries))
	}

	// Hard-delete the parent engram P.
	if err := store.DeleteEngram(ctx, ws, parentID); err != nil {
		t.Fatalf("DeleteEngram(parent): %v", err)
	}

	// All parent-role ordinal keys must be gone automatically.
	entries, err = store.ListChildOrdinals(ctx, ws, parentID)
	if err != nil {
		t.Fatalf("ListChildOrdinals after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 child ordinals after parent deleted, got %d", len(entries))
	}
}

// TestDeleteEngram_WithAssociations writes two engrams, links A->B, deletes A,
// and verifies that the association forward and reverse keys are removed from
// Pebble so GetAssociations (on a fresh, cache-cold store) returns no edges from A.
func TestDeleteEngram_WithAssociations(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("deleteengram-assoc")

	idA := writeTestEngram(t, store, ws, "engram-A", "content-A")
	idB := writeTestEngram(t, store, ws, "engram-B", "content-B")

	// Create a directed association A->B.
	assoc := &Association{
		TargetID: idB,
		Weight:   0.7,
	}
	if err := store.WriteAssociation(ctx, ws, idA, idB, assoc); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Confirm the association exists before deleting A.
	pre, err := store.GetAssociations(ctx, ws, []ULID{idA}, 10)
	if err != nil {
		t.Fatalf("GetAssociations before delete: %v", err)
	}
	if len(pre[idA]) == 0 {
		t.Fatal("expected A->B association before delete, found none")
	}

	// Hard-delete A. This must cascade and remove association keys.
	if err := store.DeleteEngram(ctx, ws, idA); err != nil {
		t.Fatalf("DeleteEngram(A): %v", err)
	}

	// The engram A must be gone.
	if _, err := store.GetEngram(ctx, ws, idA); err == nil {
		t.Error("expected error from GetEngram(A) after delete, got nil")
	}

	// Open a fresh store instance sharing the same underlying DB so the
	// assocCache (TTL=2s) starts cold and reads straight from Pebble.
	// This confirms the physical association keys were actually removed.
	freshStore := newFreshStore(t, store.db)
	post, err := freshStore.GetAssociations(ctx, ws, []ULID{idA}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after delete (fresh store): %v", err)
	}
	if len(post[idA]) != 0 {
		t.Errorf("expected 0 associations from A after delete, got %d", len(post[idA]))
	}
}

// TestGetMetadata_ReturnNilForMissing verifies that GetMetadata returns nil
// entries for non-existent engrams without error, allowing callers to
// distinguish missing from present engrams in a batch call.
func TestGetMetadata_ReturnNilForMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getmetadata-missing")

	// Write two engrams.
	id1 := writeTestEngram(t, store, ws, "exists-1", "content-1")
	id2 := writeTestEngram(t, store, ws, "exists-2", "content-2")

	// Create a non-existent ID (never written).
	missingID := NewULID()

	// Call GetMetadata with all three IDs (real, real, missing).
	metas, err := store.GetMetadata(ctx, ws, []ULID{id1, id2, missingID})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	// Verify we get exactly 3 result slots.
	if len(metas) != 3 {
		t.Fatalf("expected 3 metadata results, got %d", len(metas))
	}

	// Slot 0 (id1): should be non-nil.
	if metas[0] == nil {
		t.Error("slot 0 (id1): expected non-nil metadata")
	} else if metas[0].ID != id1 {
		t.Errorf("slot 0: ID mismatch: got %v, want %v", metas[0].ID, id1)
	}

	// Slot 1 (id2): should be non-nil.
	if metas[1] == nil {
		t.Error("slot 1 (id2): expected non-nil metadata")
	} else if metas[1].ID != id2 {
		t.Errorf("slot 1: ID mismatch: got %v, want %v", metas[1].ID, id2)
	}

	// Slot 2 (missingID): should be nil (no error).
	if metas[2] != nil {
		t.Errorf("slot 2 (missingID): expected nil, got %+v", metas[2])
	}
}
