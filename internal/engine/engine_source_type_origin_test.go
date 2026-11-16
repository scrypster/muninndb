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
	if !provenance.IsUpdateOperation(last.Operation) || sourceTypeString(last.Source) == before {
		t.Fatalf("precondition not met: last entry is %q with source %q, origin source is %q", last.Operation, sourceTypeString(last.Source), before)
	}

	if after := sourceOf("after one read"); after != before {
		t.Errorf("source_type changed from %q to %q after one read: it is reporting the last entry (%q, %s), not the memory's origin",
			before, after, last.Operation, sourceTypeString(last.Source))
	}
}
