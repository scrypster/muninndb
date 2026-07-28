package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// validityCapturingEngine records the last requests seen on the valid-time surfaces.
type validityCapturingEngine struct {
	fakeEngine
	lastActivate   *mbp.ActivateRequest
	lastWrite      *mbp.WriteRequest
	lastWriteBatch []*mbp.WriteRequest
	lastForget     *mbp.ForgetRequest
	lastEvolveAt   time.Time
}

func (e *validityCapturingEngine) WriteBatch(_ context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	e.lastWriteBatch = reqs
	resps := make([]*mbp.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i := range reqs {
		resps[i] = &mbp.WriteResponse{ID: "01JVALIDTIME0000000000TEST"}
	}
	return resps, errs
}

func (e *validityCapturingEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.lastActivate = req
	return &mbp.ActivateResponse{}, nil
}

func (e *validityCapturingEngine) Write(_ context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.lastWrite = req
	return &mbp.WriteResponse{ID: "01JVALIDTIME0000000000TEST"}, nil
}

func (e *validityCapturingEngine) Forget(_ context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	e.lastForget = req
	return &mbp.ForgetResponse{OK: true}, nil
}

func (e *validityCapturingEngine) Evolve(_ context.Context, vault, oldID, newContent, reason string, embedding []float32, concept string, effectiveAt time.Time) (*WriteResult, error) {
	e.lastEvolveAt = effectiveAt
	return &WriteResult{ID: "01JVALIDTIME0000000000TEST"}, nil
}

// TestHandleRecall_ValidTimeArgs_Wired verifies as_of and include_invalid are
// parsed into the engine ActivateRequest, alongside (not replacing) since/before.
func TestHandleRecall_ValidTimeArgs_Wired(t *testing.T) {
	eng := &validityCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{
		"vault":"default","context":["runway"],
		"as_of":"2026-05-01T00:00:00Z",
		"include_invalid":true,
		"since":"2026-01-01T00:00:00Z"
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastActivate == nil {
		t.Fatal("Activate not called")
	}
	if eng.lastActivate.AsOf == nil {
		t.Fatal("as_of not plumbed into ActivateRequest")
	}
	want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if !eng.lastActivate.AsOf.Equal(want) {
		t.Errorf("AsOf = %v, want %v", eng.lastActivate.AsOf, want)
	}
	if !eng.lastActivate.IncludeInvalid {
		t.Error("include_invalid not plumbed into ActivateRequest")
	}
	// since/before stay = the transaction axis; they must coexist with as_of.
	if _, ok := findFilter(eng.lastActivate.Filters, "created_after"); !ok {
		t.Error("since (transaction axis) filter lost when as_of present")
	}
}

// TestHandleRecall_InvalidAsOf_Rejected verifies a malformed as_of fails loudly.
func TestHandleRecall_InvalidAsOf_Rejected(t *testing.T) {
	eng := &validityCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{
		"vault":"default","context":["x"],"as_of":"last tuesday"
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error == nil {
		t.Error("expected error for malformed as_of")
	}
}

// TestHandleRemember_ValidityArgs_Wired verifies valid_from/valid_until flow
// into the WriteRequest, on both muninn_remember and muninn_remember_batch.
func TestHandleRemember_ValidityArgs_Wired(t *testing.T) {
	eng := &validityCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{
		"vault":"default","content":"the office was in building 7",
		"valid_from":"2024-01-01T00:00:00Z","valid_until":"2025-06-30T00:00:00Z"
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastWrite == nil || eng.lastWrite.ValidFrom == nil || eng.lastWrite.ValidUntil == nil {
		t.Fatalf("valid_from/valid_until not plumbed: %+v", eng.lastWrite)
	}
	if !eng.lastWrite.ValidFrom.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ValidFrom = %v", eng.lastWrite.ValidFrom)
	}
	if !eng.lastWrite.ValidUntil.Equal(time.Date(2025, 6, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ValidUntil = %v", eng.lastWrite.ValidUntil)
	}

	// Batch path.
	eng.lastWrite = nil
	body = `{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"muninn_remember_batch","arguments":{
		"vault":"default","memories":[{"content":"batch validity fact","valid_from":"2024-02-01T00:00:00Z"}]
	}}}`
	w = postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("batch: unexpected error: %v", resp.Error)
	}
	if eng.lastWriteBatch == nil || len(eng.lastWriteBatch) != 1 || eng.lastWriteBatch[0].ValidFrom == nil {
		t.Fatalf("batch valid_from not plumbed")
	}
}

// TestHandleForget_NotTrueSince_Wired verifies not_true_since is parsed and
// forwarded, and malformed values are rejected.
func TestHandleForget_NotTrueSince_Wired(t *testing.T) {
	eng := &validityCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{
		"vault":"default","id":"01HZXF00000000000000000000","not_true_since":"2026-07-01T00:00:00Z"
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastForget == nil || eng.lastForget.NotTrueSince == nil {
		t.Fatal("not_true_since not plumbed into ForgetRequest")
	}
	if !eng.lastForget.NotTrueSince.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("NotTrueSince = %v", eng.lastForget.NotTrueSince)
	}

	body = `{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"muninn_forget","arguments":{
		"vault":"default","id":"01HZXF00000000000000000000","not_true_since":"yesterday"
	}}}`
	w = postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error == nil {
		t.Error("expected error for malformed not_true_since")
	}
}

// TestHandleEvolve_EffectiveAt_Wired verifies effective_at reaches the engine.
func TestHandleEvolve_EffectiveAt_Wired(t *testing.T) {
	eng := &validityCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{
		"vault":"default","id":"01HZXF00000000000000000000","new_content":"v2","reason":"update",
		"effective_at":"2026-06-15T12:00:00Z"
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if !eng.lastEvolveAt.Equal(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("effectiveAt = %v, want 2026-06-15T12:00:00Z", eng.lastEvolveAt)
	}
}

// readValidityEngine serves a fixed ReadResponse carrying validity fields.
type readValidityEngine struct {
	fakeEngine
	isCurrent  bool
	validUntil int64
}

func (e *readValidityEngine) Read(_ context.Context, _ *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return &mbp.ReadResponse{
		ID: "01HZXF00000000000000000000", Concept: "c", Content: "x",
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		ValidFrom:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		ValidUntil: e.validUntil,
		IsCurrent:  e.isCurrent,
	}, nil
}

// TestHandleRead_EchoesValidity verifies muninn_read surfaces
// valid_from / valid_until / is_current in the JSON payload.
func TestHandleRead_EchoesValidity(t *testing.T) {
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	eng := &readValidityEngine{isCurrent: false, validUntil: until.UnixNano()}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_read","arguments":{
		"vault":"default","id":"01HZXF00000000000000000000"
	}}}`
	w := postRPC(t, srv, body)
	out := w.Body.String()
	if resp := decodeResp(t, out); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	for _, want := range []string{`\"valid_from\":`, `\"valid_until\":`, `\"is_current\":false`} {
		if !strings.Contains(out, want) {
			t.Errorf("read response missing %s in %s", want, out)
		}
	}

	// Open window: is_current true, valid_until omitted.
	eng2 := &readValidityEngine{isCurrent: true, validUntil: 0}
	srv2 := newTestServerWith(eng2)
	w = postRPC(t, srv2, body)
	out = w.Body.String()
	if !strings.Contains(out, `\"is_current\":true`) {
		t.Errorf("read response missing is_current:true in %s", out)
	}
	if strings.Contains(out, `\"valid_until\":`) {
		t.Errorf("open window must omit valid_until, got %s", out)
	}
}
