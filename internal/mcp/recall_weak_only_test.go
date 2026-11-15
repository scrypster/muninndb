package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// weakOnlyFakeEngine records the request so the include_weak plumbing can be
// asserted, and answers with a weak_only abstention.
type weakOnlyFakeEngine struct {
	fakeEngine
	got *mbp.ActivateRequest
}

func (e *weakOnlyFakeEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.got = req
	if req.IncludeWeak {
		return &mbp.ActivateResponse{TotalFound: 1, Activations: []mbp.ActivationItem{
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: "c", Content: "x", Score: 0.3, RelevanceBand: "weak"}}}, nil
	}
	return &mbp.ActivateResponse{TotalFound: 3, Abstained: true, AbstainedReason: "weak_only", WeakAvailable: 3}, nil
}

// TestHandleRecall_WeakOnlyIsLoudAndRecoverable: the MCP response map is
// hand-built, so a new engine field reaches REST by alias and silently
// vanishes here unless it is mapped. weak_only must arrive with its count and
// a hint naming include_weak; include_weak and an explicit threshold must
// reach the engine as IncludeWeak.
func TestHandleRecall_WeakOnlyIsLoudAndRecoverable(t *testing.T) {
	eng := &weakOnlyFakeEngine{}
	srv := newTestServerWith(eng)
	call := func(args string) map[string]any {
		t.Helper()
		body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","query":"zz qx",` + args + `}}}`
		resp := decodeResp(t, postRPC(t, srv, body).Body.String())
		if resp.Error != nil {
			t.Fatalf("unexpected error: %v", resp.Error)
		}
		return extractInnerJSON(t, resp)
	}
	out := call(`"limit":10`)
	if eng.got.IncludeWeak {
		t.Fatalf("a plain recall must not opt into weak rows")
	}
	if out["abstained"] != true || out["abstained_reason"] != "weak_only" || out["weak_available"] != float64(3) {
		t.Fatalf("weak_only abstention not surfaced: %v", out)
	}
	if hint, _ := out["hint"].(string); !strings.Contains(hint, "include_weak: true") || strings.Contains(hint, "lower 'threshold'") {
		t.Errorf("hint must name include_weak (not the threshold lever, which is not the cause): %q", hint)
	}
	if out := call(`"include_weak":true`); !eng.got.IncludeWeak || out["abstained"] == true {
		t.Errorf("include_weak must reach the engine and return rows: req=%v out=%v", eng.got.IncludeWeak, out)
	}
	call(`"threshold":0.1`)
	if !eng.got.IncludeWeak {
		t.Errorf("an explicit threshold must opt out of weak_only abstention")
	}
}
