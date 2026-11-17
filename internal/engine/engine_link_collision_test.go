package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestLink_ACollisionNamesBothRelations: a link that would replace an edge of
// another relation from the same memory to the same memory at the same weight
// is refused (STO-15). The refusal is what MCP and MBP show the caller, so it
// names both relations by the names the caller writes, says nothing was
// written, and says how to get the link in: at another weight. It used to
// print the relations as numbers. The refusal is per direction: the same link
// from the other memory is another key and is written. Fixtures are invented
// (a pottery studio).
func TestLink_ACollisionNamesBothRelations(t *testing.T) {
	eng, _, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()
	write := func(content string) string {
		r, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: "kiln shelves", Content: content})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	target := write("Kiln shelves are washed monthly.")
	succ := write("Kiln shelves are washed every two weeks.")
	link := func(src, dst string, rel storage.RelType) error {
		_, err := eng.Link(ctx, &mbp.LinkRequest{Vault: "default", SourceID: src, TargetID: dst, RelType: uint16(rel), Weight: 0.8})
		return err
	}
	if err := link(succ, target, storage.RelSupersedes); err != nil {
		t.Fatalf("supersedes link: %v", err)
	}

	err := link(succ, target, storage.RelRelatesTo)
	if !errors.Is(err, storage.ErrAssocRelTypeCollision) {
		t.Fatalf("relates_to at the same weight in the same direction: err = %v, want ErrAssocRelTypeCollision", err)
	}
	msg := err.Error()
	for _, want := range []string{"nothing was written", "a supersedes link to " + target, "a relates_to link at that weight", "Write the relates_to link at a different weight"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "rel_type 4") || strings.Contains(msg, "rel_type 5") {
		t.Errorf("refusal %q still names a relation by its number", msg)
	}

	if err := link(target, succ, storage.RelRelatesTo); err != nil {
		t.Errorf("relates_to in the other direction at the same weight: %v, want it written (another key)", err)
	}
}
