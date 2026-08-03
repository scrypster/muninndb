package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// An empty recall was indistinguishable from an un-run one at the MCP surface.
//
// The engine has known since the abstention fix whether it deliberately
// returned nothing (ActivateResult.Abstained / AbstainedReason), but that
// signal died at the mbp boundary: ActivateResponse did not carry it, so the
// MCP caller saw {"memories": null, "total": 0} plus a generic hint whether the
// pipeline abstained, found nothing, or filtered everything. Two evaluation
// rounds called this out — and called the hint itself "wrong advice", because
// it suggests mode='recent' while never mentioning the threshold, which is the
// lever that actually changes the outcome.
//
// This is the same silent-substitution class as #742/#743/#745/#746: an
// unknown state (why is this empty?) reported as a known one (nothing exists).
type abstainedFakeEngine struct {
	fakeEngine
	resp *mbp.ActivateResponse
}

func (e *abstainedFakeEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return e.resp, nil
}

func TestHandleRecall_AbstainedIsSelfDescribing(t *testing.T) {
	eng := &abstainedFakeEngine{resp: &mbp.ActivateResponse{
		TotalFound:      0,
		Abstained:       true,
		AbstainedReason: "below_threshold",
	}}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["anything"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := extractInnerJSON(t, resp)
	if out["abstained"] != true {
		t.Errorf("abstained recall must carry abstained:true — an empty result the caller cannot "+
			"interpret is indistinguishable from 'nothing exists'; got %v", out["abstained"])
	}
	if out["abstained_reason"] != "below_threshold" {
		t.Errorf("abstained_reason = %v, want %q", out["abstained_reason"], "below_threshold")
	}
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, "threshold") {
		t.Errorf("the empty-result hint must name the threshold lever — evaluators called the old "+
			"hint 'wrong advice' because it suggested mode='recent' while omitting the one knob that "+
			"changes the outcome; got %q", hint)
	}
}

// A recall that returns results must NOT carry the abstained fields — an
// annotation that appears on every response stops meaning anything.
func TestHandleRecall_NonEmptyCarriesNoAbstained(t *testing.T) {
	eng := &abstainedFakeEngine{resp: &mbp.ActivateResponse{
		TotalFound: 1,
		Activations: []mbp.ActivationItem{
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: "c", Content: "x", Score: 0.9},
		},
	}}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["x"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := extractInnerJSON(t, resp)
	if _, present := out["abstained"]; present {
		t.Errorf("non-empty recall must omit the abstained field, got %v", out["abstained"])
	}
}

// Declaring that a memory contradicts ITSELF was accepted with {"ok":true},
// creating an edge that annotates the memory as conflicting with itself. An
// evaluator did it live (call 15/49: "self-link contradicts accepted;
// conflicts_with pointed at itself"). A self-contradiction is not a
// declaration, it is a caller error, and accepting it poisons the one channel
// (declared edges) the system treats as ground truth.
func TestHandleLink_RejectsSelfLink(t *testing.T) {
	eng := &abstainedFakeEngine{resp: &mbp.ActivateResponse{}}
	srv := newTestServerWith(eng)
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"` + id + `","target_id":"` + id + `","relation":"contradicts"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatalf("linking a memory to itself must be rejected, got success: %s", w.Body.String())
	}
	if !strings.Contains(resp.Error.Message, "itself") {
		t.Errorf("the error should say the memory cannot be linked to itself, got %q", resp.Error.Message)
	}
}
