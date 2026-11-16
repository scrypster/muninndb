package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// gRPC carries inline edges in `associations` (rel_type is numeric; the proto
// has no `relationships`). A supersedes among them must reach the engine as
// the declaration it is (COG-38): the target is retired, and a declaration
// that cannot be honoured comes back as the RPC's error text — gRPC has no
// hint field on a write to say "stored, but not as you asked".
//
// RED on develop: the second write succeeds (a fork) and the first leaves its
// target live.
//
// Fixtures are invented (a pottery studio).
func TestAdapter_Write_InlineSupersedesIsHonouredOrRefused(t *testing.T) {
	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()
	eng := engine.NewEngine(engine.EngineConfig{Store: store})
	defer eng.Stop()
	adapter := NewEngineAdapter(eng)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("")

	superseding := func(content, target string) *pb.WriteRequest {
		return &pb.WriteRequest{Concept: "glaze mixing ratio", Content: content, Associations: []*pb.Association{{
			TargetId: target, RelType: uint32(storage.RelSupersedes), Weight: 0.9, Confidence: 1,
		}}}
	}
	retired := func(id string) bool {
		t.Helper()
		u, _ := storage.ParseULID(id)
		e, err := store.GetEngram(ctx, ws, u)
		if err != nil {
			t.Fatal(err)
		}
		return !e.ValidUntil.IsZero() && e.State == storage.StateActive
	}

	old, err := adapter.Write(ctx, &pb.WriteRequest{Concept: "glaze mixing ratio", Content: "Celadon glaze is blended at three parts feldspar to one part whiting."})
	if err != nil {
		t.Fatal(err)
	}
	head, err := adapter.Write(ctx, superseding("Shino slip recipes switched to nepheline syenite.", old.Id))
	if err != nil {
		t.Fatalf("a legal inline supersedes over gRPC: %v", err)
	}
	if !retired(old.Id) {
		t.Errorf("the superseded memory is still live after a gRPC write that supersedes it")
	}

	_, err = adapter.Write(ctx, superseding("Tenmoku batches are thinned with rainwater.", old.Id))
	if err == nil {
		t.Fatal("a second supersedes of the same memory was accepted over gRPC — a fork")
	}
	if !strings.Contains(err.Error(), head.Id) || !strings.Contains(err.Error(), "nothing was stored") {
		t.Errorf("the RPC error must say nothing was stored and name the current head %s; got %v", head.Id, err)
	}

	batch, err := adapter.BatchWrite(ctx, &pb.BatchWriteRequest{Requests: []*pb.WriteRequest{
		{Concept: "drying shelf rotation", Content: "Greenware is rotated a quarter turn each morning."},
		superseding("Celadon is retired from the production glaze list.", old.Id),
	}})
	if err != nil || len(batch.Results) != 2 {
		t.Fatalf("batch: %v %+v", err, batch)
	}
	if batch.Results[0].Error != "" || batch.Results[0].Id == "" {
		t.Errorf("the ordinary batch item should be stored: %+v", batch.Results[0])
	}
	if !strings.Contains(batch.Results[1].Error, head.Id) || batch.Results[1].Id != "" {
		t.Errorf("the superseding batch item should be refused naming %s: %+v", head.Id, batch.Results[1])
	}
	succ, err := store.SupersedersOf(ctx, ws, mustULID(t, old.Id))
	if err != nil || len(succ) != 1 {
		t.Errorf("successors of the superseded memory = %v (err %v), want exactly one", succ, err)
	}
}

func mustULID(t *testing.T, s string) storage.ULID {
	t.Helper()
	u, err := storage.ParseULID(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
