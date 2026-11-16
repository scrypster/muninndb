package grpc_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// TestGRPCActivate_RecordsNoUndeliveredAssociatedIDs — the gRPC response
// has no `associated` field, so a gRPC recall must not record the COG-35
// associated items as served: muninn_feedback on its query id must reject a
// label for an association-only memory the caller never received. RED before:
// the adapter never opted out, up to three ids were recorded as served, and
// the label was accepted. Fixtures are invented (trail maintenance).
func TestGRPCActivate_RecordsNoUndeliveredAssociatedIDs(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-grpc-associated-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	defer store.Close()
	ftsIdx := fts.New(db)
	emb := activation.NewNoopEmbedder()
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx,
		ActivationEngine: activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, emb),
		TriggerSystem:    trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, emb), Embedder: emb})
	defer eng.Stop()
	ctx := context.Background()
	ents := []mbp.InlineEntity{{Name: "Kestrel Saddle", Type: "place"}, {Name: "Harrowmoor Spur", Type: "place"}}
	write := func(concept, content string, e []mbp.InlineEntity) string {
		w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: concept, Content: content, Entities: e})
		if err != nil {
			t.Fatal(err)
		}
		return w.ID
	}
	write("culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two", ents)
	write("culvert outlet riprap", "the culvert outlet riprap was reset", nil)
	write("outlet riprap drainage", "riprap drainage at the outlet upstream", nil)
	write("culvert drainage riprap", "culvert drainage riprap after storms", nil)
	injectee := write("saddle campsite", "campsite permits for the saddle area", ents)
	for i := 0; i < 10; i++ {
		write(fmt.Sprintf("brushing log %d", i), fmt.Sprintf("crew brushed the corridor on day %d", i), nil)
	}
	eng.WaitWriteTimeIdle()

	req := &mbp.ActivateRequest{Vault: "default", Context: []string{"culvert outlet riprap drainage"}, MaxResults: 3, Threshold: 0.001}
	// Premise: a surface that renders the block would carry the injectee.
	rendered, err := eng.Activate(mbp.WithAssociatedRendered(ctx), &mbp.ActivateRequest{Vault: req.Vault, Context: req.Context, MaxResults: 3, Threshold: 0.001, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	premise := false
	for _, a := range rendered.Associated {
		premise = premise || a.ID == injectee
	}
	if !premise {
		t.Fatalf("premise: the injectee is association-only for this query: %+v", rendered.Associated)
	}

	resp, err := transportgrpc.TestableNewEngineAdapter(eng).Activate(ctx, &pb.ActivateRequest{Vault: "default",
		Context: req.Context, MaxResults: 3, Threshold: 0.001})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Activations {
		if a.Id == injectee {
			t.Fatalf("premise: the injectee must not be a matched row")
		}
	}
	out, err := eng.RecordFeedbackLabelsDetailed(ctx, "default",
		[]engine.FeedbackGroup{{QueryID: resp.QueryId, IDs: []string{injectee}, Useful: true}}, engine.FeedbackChannelDirect, engine.FeedbackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Outcome != engine.FeedbackOutcomeRejected || out[0].Reason != engine.FeedbackReasonNotInRecall {
		t.Errorf("a label for an id the gRPC caller never received was %+v; want rejected not_in_recall", out)
	}
}
