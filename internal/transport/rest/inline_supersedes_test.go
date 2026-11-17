package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
)

// A REST write that carries an inline supersedes it cannot honour is refused
// with a status the caller can act on and a message that survives the trip.
// REST's 5xx path replaces every message with "an internal error occurred", so
// an unclassified refusal would tell the caller nothing — not that the write
// was refused for its declaration, and not which memory is the current one.
//
// Fixtures are invented (a pottery studio).

func postEngram(t *testing.T, s *Server, mode string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/api/engrams", bytes.NewReader(raw))
	ctx := context.WithValue(r.Context(), auth.ContextMode, mode)
	ctx = context.WithValue(ctx, auth.ContextVault, "default")
	w := httptest.NewRecorder()
	s.handleCreateEngram(w, r.WithContext(ctx))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func restErrMessage(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// RED on develop: 201 for every case — the fork is written, the forgotten
// memory gains an edge, and the append credential's declaration is accepted.
func TestCreateEngram_InlineSupersedesRefusalsAreActionable(t *testing.T) {
	eng, cleanup := newRESTRetryEnrichEnv(t)
	defer cleanup()
	s := newTestServer(t, newTestAuthStore(t))
	s.engine = NewEngineWrapper(eng, nil)

	write := func(content string) string {
		t.Helper()
		w, out := postEngram(t, s, auth.ModeFull, map[string]any{"concept": "glaze mixing ratio", "content": content})
		if w.Code != http.StatusCreated {
			t.Fatalf("seed write: %d %s", w.Code, w.Body.String())
		}
		return out["id"].(string)
	}
	superseding := func(field, content, target string) map[string]any {
		body := map[string]any{"concept": "glaze mixing ratio", "content": content}
		if field == "relationships" {
			body["relationships"] = []map[string]any{{"target_id": target, "relation": "supersedes", "weight": 0.9}}
		} else {
			body["associations"] = []map[string]any{{"target_id": target, "rel_type": uint16(storage.RelSupersedes), "weight": 0.9, "confidence": 1.0}}
		}
		return body
	}

	for _, field := range []string{"relationships", "associations"} {
		t.Run(field, func(t *testing.T) {
			old := write("Celadon glaze is blended at three parts feldspar to one part whiting. " + field)

			// Accepted: 201, and the REST response still carries the id.
			w, out := postEngram(t, s, auth.ModeFull, superseding(field, "Shino slip recipes switched to nepheline syenite. "+field, old))
			if w.Code != http.StatusCreated {
				t.Fatalf("a legal inline supersedes: %d %s", w.Code, w.Body.String())
			}
			head := out["id"].(string)

			// Already superseded: 409, and the message names the current head.
			w, out = postEngram(t, s, auth.ModeFull, superseding(field, "Tenmoku batches are thinned with rainwater. "+field, old))
			if w.Code != http.StatusConflict {
				t.Fatalf("supersedes of an already-superseded memory: status %d, want 409 — body %s", w.Code, w.Body.String())
			}
			if msg := restErrMessage(out); !strings.Contains(msg, head) || !strings.Contains(msg, "nothing was stored") {
				t.Errorf("the 409 must say nothing was stored and name the current head %s; got %q", head, msg)
			}

			// Append credential: 403, said plainly.
			live := write("Ash glaze tests are logged on the kiln-room chalkboard. " + field)
			w, out = postEngram(t, s, auth.ModeAppend, superseding(field, "Ash glaze tests moved to the studio ledger. "+field, live))
			if w.Code != http.StatusForbidden {
				t.Fatalf("append-mode inline supersedes: status %d, want 403 — body %s", w.Code, w.Body.String())
			}
			if msg := restErrMessage(out); !strings.Contains(msg, "append-mode") || !strings.Contains(msg, "supersedes") {
				t.Errorf("the 403 must say why an append credential was refused; got %q", msg)
			}

			// Forgotten target: 409.
			if _, err := eng.Forget(context.Background(), &ForgetRequest{Vault: "default", ID: live}); err != nil {
				t.Fatal(err)
			}
			w, _ = postEngram(t, s, auth.ModeFull, superseding(field, "Ash glaze tests moved to the studio ledger. "+field, live))
			if w.Code != http.StatusConflict {
				t.Errorf("supersedes of a forgotten memory: status %d, want 409 — body %s", w.Code, w.Body.String())
			}

			// Missing target: still the 400 it already was.
			w, _ = postEngram(t, s, auth.ModeFull, superseding(field, "A note about nothing in particular. "+field, storage.NewULID().String()))
			if w.Code != http.StatusBadRequest {
				t.Errorf("supersedes of a memory that does not exist: status %d, want 400 — body %s", w.Code, w.Body.String())
			}
		})
	}
}

// In a REST batch the refusal is per item, in the body, with the same message.
// RED on develop: every item reports ok.
func TestBatchCreate_InlineSupersedesRefusalIsPerItem(t *testing.T) {
	eng, cleanup := newRESTRetryEnrichEnv(t)
	defer cleanup()
	s := newTestServer(t, newTestAuthStore(t))
	s.engine = NewEngineWrapper(eng, nil)

	w, out := postEngram(t, s, auth.ModeFull, map[string]any{"concept": "wheel maintenance", "content": "The kick wheel bearings are greased monthly."})
	if w.Code != http.StatusCreated {
		t.Fatalf("seed: %d", w.Code)
	}
	old := out["id"].(string)
	w, out = postEngram(t, s, auth.ModeFull, map[string]any{"concept": "wheel maintenance", "content": "Wheel head runout is checked every fortnight.",
		"relationships": []map[string]any{{"target_id": old, "relation": "supersedes", "weight": 0.9}}})
	if w.Code != http.StatusCreated {
		t.Fatalf("first supersedes: %d %s", w.Code, w.Body.String())
	}
	head := out["id"].(string)

	raw, _ := json.Marshal(map[string]any{"engrams": []map[string]any{
		{"concept": "drying shelf rotation", "content": "Greenware is rotated a quarter turn each morning."},
		{"concept": "wheel maintenance", "content": "The treadle linkage is oiled whenever it squeaks.",
			"relationships": []map[string]any{{"target_id": old, "relation": "supersedes", "weight": 0.9}}},
	}})
	r := httptest.NewRequest(http.MethodPost, "/api/engrams/batch", bytes.NewReader(raw))
	ctx := context.WithValue(r.Context(), auth.ContextMode, auth.ModeFull)
	ctx = context.WithValue(ctx, auth.ContextVault, "default")
	rec := httptest.NewRecorder()
	s.handleBatchCreate(rec, r.WithContext(ctx))

	var body struct {
		Results []struct {
			Index  int    `json:"index"`
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Results) != 2 {
		t.Fatalf("batch response: %v %s", err, rec.Body.String())
	}
	if body.Results[0].Status != "ok" || body.Results[0].ID == "" {
		t.Errorf("the ordinary item should be stored: %+v", body.Results[0])
	}
	if body.Results[1].Status != "error" || !strings.Contains(body.Results[1].Error, head) {
		t.Errorf("the superseding item should be refused naming %s: %+v", head, body.Results[1])
	}
}
