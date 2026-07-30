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
// same idempotent_id + upsert_mode must land on the same engram (merge), and
// the durable 0x2E forward index must pin it. The upsert path is nil-safe for
// the engine's ancillary fields (hnsw/fts/triggers/coherence are nil-checked),
// so a Store-only EngineConfig suffices.
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

	// Second upsert — same key, changed content → merge in place (same id).
	resp2, err := adapter.Write(ctx, &pb.WriteRequest{
		Content: "v2", UpsertMode: true, IdempotentID: "grpc-doc",
	})
	if err != nil {
		t.Fatalf("second adapter.Write: %v", err)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("upsert changed the id through gRPC: %s vs %s", resp1.ID, resp2.ID)
	}

	// The durable forward index pins grpc-doc → the merged id.
	ws := store.ResolveVaultPrefix("")
	keyHash := sha256.Sum256([]byte("grpc-doc"))
	pinned, err := store.GetUpsertKey(ctx, ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey: %v", err)
	}
	id, err := storage.ParseULID(resp1.ID)
	if err != nil {
		t.Fatalf("parse ULID: %v", err)
	}
	if pinned != id {
		t.Errorf("upsert-key pin: got %x, want %x", pinned[:], id[:])
	}

	// Content merged to v2.
	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Content != "v2" {
		t.Errorf("merged content: got %q, want %q", got.Content, "v2")
	}
}
