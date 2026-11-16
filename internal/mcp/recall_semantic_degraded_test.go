package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The degraded flag and its reason survive the hand-built muninn_recall
// response map (a field added to mbp.ActivateResponse alone reaches REST and
// silently vanishes here). A keyword-only result must say it is one, and why.
func TestHandleRecall_SemanticDegradedCarriesReason(t *testing.T) {
	eng := &abstainedFakeEngine{resp: &mbp.ActivateResponse{
		TotalFound:             1,
		SemanticDegraded:       true,
		SemanticDegradedReason: "embed_timeout",
		Activations: []mbp.ActivationItem{
			{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: "c", Content: "x", Score: 0.9},
		},
	}}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["x"]}}}`
	resp := decodeResp(t, postRPC(t, srv, body).Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	out := extractInnerJSON(t, resp)
	if out["semantic_degraded"] != true {
		t.Fatalf("semantic_degraded = %v, want true", out["semantic_degraded"])
	}
	if out["semantic_degraded_reason"] != "embed_timeout" {
		t.Fatalf("semantic_degraded_reason = %v, want embed_timeout", out["semantic_degraded_reason"])
	}
}

// muninn_status names a busy embedder as such: the embedder block carries the
// reason and health_reasons says the backend is up but was busy, which an
// operator handles differently from an unreachable one.
func TestDeriveHealth_EmbedTimeoutReason(t *testing.T) {
	off := cognitive.EngineWorkerStats{Hebbian: cognitive.DisabledWorkerStats(), Contradict: cognitive.DisabledWorkerStats(), Confidence: cognitive.DisabledWorkerStats()}
	now := time.Unix(1_800_000_000, 0)
	h, reasons, emb, _ := deriveHealth(healthSnapshot{
		EmbedState: "degraded_to_keyword", EmbedLastDegraded: now, EmbedDegradedReason: "embed_timeout",
		Workers: off, Now: now,
	})
	if h != "degraded" || emb.State != "degraded_to_keyword" || emb.LastDegradedReason != "embed_timeout" {
		t.Fatalf("health=%q embedder=%+v, want degraded / degraded_to_keyword / embed_timeout", h, emb)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "busy") || !strings.Contains(reasons[0], "embed_timeout") {
		t.Fatalf("health_reasons = %v, want one entry naming the busy embedder and embed_timeout", reasons)
	}
}
