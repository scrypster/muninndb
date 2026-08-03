package mcp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// upsertCaptureEngine records the UpsertMode + IdempotentID the handler passes
// to Write, so the cross-surface parity tests can assert muninn_remember
// threads upsert_mode through to the engine (mirrors the idempotentEngine /
// limitTrackingEngine fake pattern).
type upsertCaptureEngine struct {
	fakeEngine
	lastUpsertMode   bool
	lastIdempotentID string
	writeCalls       int
}

func (e *upsertCaptureEngine) Write(_ context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.writeCalls++
	e.lastUpsertMode = req.UpsertMode
	e.lastIdempotentID = req.IdempotentID
	return &mbp.WriteResponse{ID: "upsert-id"}, nil
}

// TestHandleRemember_UpsertMode_ThreadsToEngine: muninn_remember with
// upsert_mode + op_id must set WriteRequest.UpsertMode=true and
// IdempotentID=op_id, and reach Write — NOT short-circuit on a receipt (upsert
// uses the durable 0x2F forward index, not the receipt path). MCP-side half of
// the #556 Inc 3 cross-surface parity check.
func TestHandleRemember_UpsertMode_ThreadsToEngine(t *testing.T) {
	eng := &upsertCaptureEngine{}
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"v1","op_id":"doc-1","upsert_mode":true}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	if eng.writeCalls != 1 {
		t.Fatalf("expected exactly 1 Write call (upsert must not short-circuit on a receipt), got %d", eng.writeCalls)
	}
	if !eng.lastUpsertMode {
		t.Error("WriteRequest.UpsertMode not threaded (want true)")
	}
	if eng.lastIdempotentID != "doc-1" {
		t.Errorf("WriteRequest.IdempotentID: got %q, want %q (op_id is the upsert key)", eng.lastIdempotentID, "doc-1")
	}
}

// TestHandleRemember_UpsertMode_RequiresOpID: upsert_mode without op_id is
// rejected — the durable forward index is keyed by op_id, so a bare upsert is a
// caller bug. Fail loud, matching the engine + CLI surfaces.
func TestHandleRemember_UpsertMode_RequiresOpID(t *testing.T) {
	srv := newTestServerWith(&upsertCaptureEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"v1","upsert_mode":true}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error when upsert_mode is set without op_id")
	}
}

// TestHandleRemember_UpsertMode_SkipsReceiptWrite: a successful upsert must NOT
// write an idempotency receipt — the durable forward index tracks the pin, and a
// receipt would make a later non-upsert retry return stale. Verified via a fake
// whose WriteIdempotency fails the test if called.
type receiptTrackingEngine struct {
	upsertCaptureEngine
	receiptWrites int
}

func (e *receiptTrackingEngine) WriteIdempotency(_ context.Context, _, _ string) error {
	e.receiptWrites++
	return nil
}

func TestHandleRemember_UpsertMode_SkipsReceiptWrite(t *testing.T) {
	eng := &receiptTrackingEngine{}
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"v1","op_id":"doc-1","upsert_mode":true}}}`
	w := postRPC(t, srv, body)
	if decodeResp(t, w.Body.String()).Error != nil {
		t.Fatalf("unexpected error")
	}
	if eng.receiptWrites != 0 {
		t.Errorf("upsert must not write an idempotency receipt (the durable index tracks it); got %d receipt writes", eng.receiptWrites)
	}
}
