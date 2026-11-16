package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// source_type says where a memory came from. It was read from the LAST
// provenance entry, and the first access appends an update entry written as
// "inferred", so one read flipped the field: it reported who touched the
// memory last. This drives the real path: write, recall, read (which records
// an access), recall again.
func TestActivate_SourceTypeIsTheOriginNotTheLastToucher(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "source-type-origin"

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "ferry timetable for the north pier",
		Content: "The north pier ferry leaves on the hour until dusk in winter.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitFTS(t, eng)
	store.DrainProvenance()

	sourceOf := func(stage string) string {
		t.Helper()
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:       vault,
			Context:     []string{"north pier ferry timetable"},
			MaxResults:  10,
			Threshold:   0.01,
			DisableHops: true,
			ReadOnly:    true,
		})
		if err != nil {
			t.Fatalf("%s: Activate: %v", stage, err)
		}
		for _, it := range resp.Activations {
			if it.ID == w.ID {
				return it.SourceType
			}
		}
		t.Fatalf("%s: the memory was not returned (%d rows)", stage, len(resp.Activations))
		return ""
	}

	before := sourceOf("before any access")
	if before == "" {
		t.Fatal("no source_type before any access: the fixture is not exercising the provenance path")
	}

	// Record one access, the way a read does. Engine.Read makes this same
	// storage call from a goroutine after it returns; calling it here keeps
	// the test deterministic.
	id, err := storage.ParseULID(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAccess(ctx, store.ResolveVaultPrefix(vault), id); err != nil {
		t.Fatalf("TouchAccess: %v", err)
	}
	store.DrainProvenance()

	// Prove the precondition, or the test passes for the wrong reason: the log
	// must now END with an update whose source differs from the origin's.
	entries, err := eng.GetProvenance(ctx, vault, w.ID)
	if err != nil {
		t.Fatalf("GetProvenance: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("the read appended no provenance entry (%d entries): nothing to distinguish origin from last toucher", len(entries))
	}
	last := entries[len(entries)-1]
	if provenance.IsOriginOperation(last.Operation) || sourceTypeString(last.Source) == before {
		t.Fatalf("precondition not met: last entry is %q with source %q, origin source is %q", last.Operation, sourceTypeString(last.Source), before)
	}

	if after := sourceOf("after one read"); after != before {
		t.Errorf("source_type changed from %q to %q after one read: it is reporting the last entry (%q, %s), not the memory's origin",
			before, after, last.Operation, sourceTypeString(last.Source))
	}
}

// Every path that brings a memory into being must leave an origin the recall
// path can find. The list of creating operations is closed (an unknown
// operation is never taken for an origin), so a creating path that records a
// name the list does not know would report no source at all: this fails then.
func TestEveryCreatingWritePathLeavesAKnownOrigin(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "origin-of-every-write-path"
	ws := store.ResolveVaultPrefix(vault)

	made := map[string]string{}

	w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "harbour tide table", Content: "High water at the harbour mouth is two hours after the quay."})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	made["Write"] = w.ID

	resps, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{Vault: vault, Concept: "lighthouse keeper rota", Content: "The lighthouse rota changes on the first Monday of each month."},
		{Vault: vault, Concept: "slipway closure", Content: "The slipway is closed for repairs until the spring tides."},
	})
	for i, e := range errs {
		if e != nil {
			t.Fatalf("WriteBatch item %d: %v", i, e)
		}
	}
	made["WriteBatch"] = resps[1].ID

	evolved, err := eng.Evolve(ctx, vault, w.ID, "High water at the harbour mouth is ninety minutes after the quay.", "re-measured", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	made["Evolve (the new version)"] = evolved.String()

	tree, err := eng.RememberTree(ctx, &RememberTreeRequest{Vault: vault, Root: TreeNodeInput{
		Concept: "boatyard jobs", Content: "Jobs booked at the boatyard this season.",
		Children: []TreeNodeInput{{Concept: "repaint the tender", Content: "The tender needs two coats before launch."}},
	}})
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}
	made["RememberTree (root)"] = tree.RootID
	made["RememberTree (child)"] = tree.NodeMap["repaint the tender"]

	store.DrainProvenance()

	for path, idStr := range made {
		id, err := storage.ParseULID(idStr)
		if err != nil {
			t.Fatalf("%s: bad id %q: %v", path, idStr, err)
		}
		origin, ok, err := eng.prov.Origin(ctx, ws, [16]byte(id))
		if err != nil {
			t.Fatalf("%s: Origin: %v", path, err)
		}
		if !ok {
			entries, _ := eng.GetProvenance(ctx, vault, idStr)
			ops := make([]string, 0, len(entries))
			for _, e := range entries {
				ops = append(ops, e.Operation)
			}
			t.Errorf("%s: no origin found; its provenance operations are %q. A creating path must record an operation provenance.IsOriginOperation knows", path, ops)
			continue
		}
		if sourceTypeString(origin.Source) == "" {
			t.Errorf("%s: origin %+v has no reportable source", path, origin)
		}
	}
}
