package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// associatedContractEngine answers as the real engine does: the COG-35
// associated block only for a request context marked
// mbp.WithAssociatedRendered (the engine records those ids as served).
type associatedContractEngine struct{ MockEngine }

func (e *associatedContractEngine) Activate(ctx context.Context, _ *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	resp := &mbp.ActivateResponse{QueryID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2V"}
	if mbp.AssociatedRendered(ctx) {
		resp.Associated = []mbp.AssociatedItem{{ID: "01JD3K8Q0W5Z7Y2M4N6P8R0T2W", Concept: "saddle campsite",
			Why: "shares entity \"Kestrel Saddle\" with returned 01JD3K8Q0W5Z7Y2M4N6P8R0T2X", Strength: 0.2, Basis: "associated_not_matched"}}
		resp.AssociatedOmitted = 2
	}
	return resp, nil
}

// TestActivate_CarriesTheAssociatedBlock — POST /api/activate returns the
// ActivateResponse verbatim, so the handler marks the request as rendering
// the block, and the block and its omitted count reach the JSON body.
func TestActivate_CarriesTheAssociatedBlock(t *testing.T) {
	server := NewServer("localhost:8080", &associatedContractEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	req := httptest.NewRequest("POST", "/api/activate", strings.NewReader(`{"context":["culvert outlet"],"vault":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	assoc, _ := got["associated"].([]any)
	if len(assoc) != 1 || got["associated_omitted"] != float64(2) {
		t.Errorf("REST /api/activate lost the associated block: associated=%v omitted=%v", got["associated"], got["associated_omitted"])
	}
}
