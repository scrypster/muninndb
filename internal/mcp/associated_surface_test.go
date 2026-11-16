package mcp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// associatedContractEngine answers as the real engine does: the COG-35
// associated block only for a request context marked
// mbp.WithAssociatedRendered (the engine records those ids as served).
type associatedContractEngine struct{ fakeEngine }

func (e *associatedContractEngine) Activate(ctx context.Context, _ *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	resp := &mbp.ActivateResponse{QueryID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2V", TotalFound: 1, Activations: []mbp.ActivationItem{
		{ID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2X", Concept: "culvert outlet", Content: "riprap at the outlet", Score: 0.5, RelevanceBand: "strong"}}}
	if mbp.AssociatedRendered(ctx) {
		resp.Associated = []mbp.AssociatedItem{{ID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2W", Concept: "saddle campsite",
			Why: "shares entity \"Kestrel Saddle\" with returned 01JD3K8Q0W5Z7Y2M4N6P8R0T2X", Strength: 0.2, Basis: "associated_not_matched", Truncated: true}}
		resp.AssociatedOmitted = 2
	}
	return resp, nil
}

// TestHandleRecall_AssociatedOmittedAndTruncatedSurvive — the MCP recall
// response map is hand-built: muninn_recall marks its request as rendering
// the block, and the block, an item's truncated flag and the omitted count
// all survive the map.
func TestHandleRecall_AssociatedOmittedAndTruncatedSurvive(t *testing.T) {
	srv := newTestServerWith(&associatedContractEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","query":"culvert outlet"}}}`
	resp := decodeResp(t, postRPC(t, srv, body).Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := extractInnerJSON(t, resp)
	assoc, _ := out["associated"].([]any)
	if len(assoc) != 1 || out["associated_omitted"] != float64(2) {
		t.Fatalf("muninn_recall lost the associated block or its omitted count: %v / %v", out["associated"], out["associated_omitted"])
	}
	if assoc[0].(map[string]any)["truncated"] != true {
		t.Errorf("the truncated flag did not survive: %v", assoc[0])
	}
}
