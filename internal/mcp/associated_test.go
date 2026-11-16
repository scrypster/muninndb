package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRecallOverMCP_AssociatedBlock — the COG-35 associated block survives
// the hand-built recall response map; include_associated:false removes it;
// a recall with nothing association-only carries no `associated` key.
// Fixtures are invented (trail maintenance).
func TestRecallOverMCP_AssociatedBlock(t *testing.T) {
	eng, srv, cleanup := newContradictionServer(t, time.Hour)
	defer cleanup()
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

	call := func(args string) map[string]any {
		rpc := `{"jsonrpc":"2.0","method":"tools/call","id":7,"params":{"name":"muninn_recall","arguments":` + args + `}}`
		return extractInnerJSON(t, decodeResp(t, postRPC(t, srv, rpc).Body.String()))
	}
	got := call(`{"vault":"default","context":"culvert outlet riprap drainage","limit":3,"threshold":0.001}`)
	for _, m := range got["memories"].([]any) {
		if m.(map[string]any)["id"] == injectee {
			t.Fatalf("premise: the association-only memory must not make the limit=3 cut")
		}
	}
	assoc, ok := got["associated"].([]any)
	if !ok || len(assoc) == 0 {
		t.Fatalf("muninn_recall carries no associated block: %v", got)
	}
	first := assoc[0].(map[string]any)
	if first["id"] != injectee || !strings.Contains(fmt.Sprint(first["why"]), "shares entity") || first["basis"] != "associated_not_matched" {
		t.Errorf("associated[0] = %v", first)
	}
	if _, present := first["content"]; present {
		t.Errorf("associated items are compact: no content field, got %v", first)
	}
	off := call(`{"vault":"default","context":"culvert outlet riprap drainage","limit":3,"threshold":0.001,"include_associated":false}`)
	if _, present := off["associated"]; present {
		t.Errorf("include_associated:false must omit the block: %v", off["associated"])
	}
	none := call(`{"vault":"default","context":"corridor brushed","limit":3,"threshold":0.001}`)
	if _, present := none["associated"]; present {
		t.Errorf("no association-only rows: the key must be absent, got %v", none["associated"])
	}
}
