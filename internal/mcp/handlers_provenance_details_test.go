package mcp

import (
	"context"
	"testing"
	"time"
)

type provenanceDetailsEngine struct{ fakeEngine }

func (e *provenanceDetailsEngine) GetProvenance(_ context.Context, _, _ string) ([]ProvenanceEntry, error) {
	return []ProvenanceEntry{
		// A legacy-shaped entry: nothing but the verb.
		{Timestamp: time.Unix(1700000000, 0).UTC().Format(time.RFC3339), Source: "human", Operation: "create"},
		// An evolve entry carrying what changed and why.
		{
			Timestamp:     time.Unix(1700000100, 0).UTC().Format(time.RFC3339),
			Source:        "human",
			Operation:     "evolve",
			PredecessorID: "01OLDULID",
			Reason:        "region migration completed",
			EffectiveAt:   time.Unix(1700000050, 0).UTC().Format(time.RFC3339),
		},
	}, nil
}

// TestHandleProvenance_EvolveDetailsSurfaced pins that muninn_provenance
// actually reports predecessor_id/reason/effective_at, and that an entry
// without them omits the keys entirely rather than emitting empty strings a
// caller could mistake for "no predecessor" / "no reason given".
func TestHandleProvenance_EvolveDetailsSurfaced(t *testing.T) {
	srv := New(":0", &provenanceDetailsEngine{}, "", nil, nil, nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_provenance","arguments":{"vault":"default","id":"01NEWULID"}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	entries, ok := content["entries"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("want 2 entries, got %T %v", content["entries"], content["entries"])
	}

	created := entries[0].(map[string]any)
	for _, k := range []string{"predecessor_id", "reason", "effective_at"} {
		if _, present := created[k]; present {
			t.Errorf("create entry emitted %q (%v); an unrecorded field must be omitted", k, created[k])
		}
	}

	evolved := entries[1].(map[string]any)
	if evolved["predecessor_id"] != "01OLDULID" {
		t.Errorf("predecessor_id = %v, want 01OLDULID", evolved["predecessor_id"])
	}
	if evolved["reason"] != "region migration completed" {
		t.Errorf("reason = %v", evolved["reason"])
	}
	if evolved["effective_at"] != time.Unix(1700000050, 0).UTC().Format(time.RFC3339) {
		t.Errorf("effective_at = %v", evolved["effective_at"])
	}
}
