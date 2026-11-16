package mbp

import (
	"context"
	"testing"
)

// associatedContractEngine answers as the real engine does: the COG-35
// associated block (and its omitted count) only for a request context marked
// WithAssociatedRendered — the engine records those ids as served, so only a
// surface that returns the block may receive it.
type associatedContractEngine struct{ stubEngine }

func (e *associatedContractEngine) Activate(ctx context.Context, _ *ActivateRequest) (*ActivateResponse, error) {
	resp := &ActivateResponse{QueryID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"}
	if AssociatedRendered(ctx) {
		resp.Associated = []AssociatedItem{{ID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2W", Concept: "saddle campsite",
			Why: "shares entity \"Kestrel Saddle\" with returned 01JD3K8Q0W5Z7Y2M4N6P8R0T2X", Strength: 0.2, Basis: "associated_not_matched"}}
		resp.AssociatedOmitted = 2
	}
	return resp, nil
}

// TestServer_ActivateCarriesTheAssociatedBlock — the MBP ACTIVATE response
// frame is the ActivateResponse verbatim, so the handler must mark the
// request as rendering the block, and the block and its omitted count reach
// the wire. Without the marker the engine builds no block (and records none).
func TestServer_ActivateCarriesTheAssociatedBlock(t *testing.T) {
	s := newTestServer(t, &associatedContractEngine{})
	c, wait := startTestConn(t, s)
	defer wait()
	doHandshake(t, c)
	resp := sendAndReceive(t, c, TypeActivate, 1, &ActivateRequest{Context: []string{"culvert outlet"}})
	var got ActivateResponse
	if err := DecodeMsgpack(resp.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Associated) != 1 || got.Associated[0].Basis != "associated_not_matched" || got.AssociatedOmitted != 2 {
		t.Errorf("MBP ACTIVATE response lost the associated block: %+v (omitted %d)", got.Associated, got.AssociatedOmitted)
	}
}
