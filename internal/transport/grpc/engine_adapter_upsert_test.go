package grpc

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// TestAdapter_Write_UpsertMode_ThreadsToEngine verifies the gRPC adapter copies
// pb.WriteRequest.UpsertMode (+ IdempotentID) into the mbp.WriteRequest the
// engine sees — the gRPC half of the #556 Inc 3 cross-surface parity check.
//
// It drives a real (minimal) engine through the adapter: two writes with the
// same idempotent_id + upsert_mode must thread into the upsert orchestrator.
// Rev 2 semantics: the first creates (Hint="upsert-created"); the second with
// CHANGED content evolves the head — successor gets a new ULID, predecessor is
// superseded (soft-deleted), and the durable 0x2F forward index is re-pointed
// to the successor atomically. The upsert path is nil-safe for the engine's
// ancillary fields (hnsw/fts/triggers/coherence are nil-checked), so a
// Store-only EngineConfig suffices.
func TestAdapter_Write_UpsertMode_ThreadsToEngine(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	// store.Close drains PebbleStore background workers AND closes the db —
	// mirroring the engine test harness (a separate db.Close() double-closes).
	defer store.Close()

	eng := engine.NewEngine(engine.EngineConfig{Store: store})
	defer eng.Stop()

	adapter := NewEngineAdapter(eng)
	ctx := context.Background()

	// First upsert — create.
	resp1, err := adapter.Write(ctx, &pb.WriteRequest{
		Content: "v1", UpsertMode: true, IdempotentID: "grpc-doc",
	})
	if err != nil {
		t.Fatalf("first adapter.Write: %v", err)
	}
	if resp1.ID == "" {
		t.Fatal("first: empty ID")
	}

	// Second upsert — same key, changed content → evolve (new successor ULID).
	resp2, err := adapter.Write(ctx, &pb.WriteRequest{
		Content: "v2", UpsertMode: true, IdempotentID: "grpc-doc",
	})
	if err != nil {
		t.Fatalf("second adapter.Write: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Errorf("upsert evolve should mint a new id through gRPC: both %s", resp1.ID)
	}

	ws := store.ResolveVaultPrefix("")
	keyHash := sha256.Sum256([]byte("grpc-doc"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	id1, _ := storage.ParseULID(resp1.ID)
	id2, _ := storage.ParseULID(resp2.ID)

	// The durable forward index pins grpc-doc → the SUCCESSOR (re-pointed in
	// the evolve's atomic batch).
	if pinned != id2 {
		t.Errorf("upsert-key pin: got %x, want successor %x", pinned[:], id2[:])
	}

	// Successor carries the new content and is the active head.
	gotHead, err := store.GetEngram(ctx, ws, id2)
	if err != nil {
		t.Fatalf("GetEngram successor: %v", err)
	}
	if gotHead.Content != "v2" {
		t.Errorf("successor content: got %q, want %q", gotHead.Content, "v2")
	}

	// Predecessor is soft-deleted with the original content (evolve creates a
	// new engram; it never mutates the predecessor in place).
	gotPred, err := store.GetEngram(ctx, ws, id1)
	if err != nil {
		t.Fatalf("GetEngram predecessor: %v", err)
	}
	if gotPred.State != storage.StateSoftDeleted {
		t.Errorf("predecessor state: got %d, want StateSoftDeleted", gotPred.State)
	}
	if gotPred.Content != "v1" {
		t.Errorf("predecessor content: got %q, want %q (evolve must not mutate)", gotPred.Content, "v1")
	}
}
