package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The agent's own path, end to end through MCP dispatch and a real engine:
// declaring a supersession inline on muninn_remember must do, for every
// credential mode, what declaring it with muninn_link does (COG-38).
//
// Fixtures are invented (a pottery studio).

const supersedesTestVault = "studio"

type supersedesMCPHarness struct {
	t   *testing.T
	eng *engine.Engine
	srv *MCPServer
	n   int
}

func newSupersedesMCPHarness(t *testing.T) (*supersedesMCPHarness, func()) {
	t.Helper()
	eng, adapter, cleanup := newRealEngineAdapter(t)
	keys := newMockKeyStore(
		auth.APIKey{ID: "full", Vault: supersedesTestVault, Mode: auth.ModeFull},
		auth.APIKey{ID: "write", Vault: supersedesTestVault, Mode: auth.ModeWrite},
		auth.APIKey{ID: "append", Vault: supersedesTestVault, Mode: auth.ModeAppend},
		auth.APIKey{ID: "observe", Vault: supersedesTestVault, Mode: auth.ModeObserve},
	)
	return &supersedesMCPHarness{t: t, eng: eng, srv: New(":0", adapter, "mdb_static", keys, nil, nil)}, cleanup
}

// seed writes a memory straight into the engine (no credential involved).
func (h *supersedesMCPHarness) seed(topic string) string {
	h.t.Helper()
	h.n++
	resp, err := h.eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: supersedesTestVault, Concept: topic,
		Content: fmt.Sprintf("%s — studio note %d, written before the change.", topic, h.n),
	})
	if err != nil {
		h.t.Fatalf("seed: %v", err)
	}
	return resp.ID
}

func (h *supersedesMCPHarness) call(token, tool string, args map[string]any) JSONRPCResponse {
	h.t.Helper()
	args["vault"] = supersedesTestVault
	w := doAuthenticatedPost(h.srv, token, mkToolCallBody(tool, args))
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		h.t.Fatalf("%s: decode: %v", tool, err)
	}
	return resp
}

// resultJSON unwraps a tool result's text content into a map.
func resultJSON(t *testing.T, resp JSONRPCResponse) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(resp.Result)
	var env struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Content) == 0 {
		t.Fatalf("tool result has no text content: %s", raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(env.Content[0].Text), &out); err != nil {
		t.Fatalf("tool result text is not JSON: %s", env.Content[0].Text)
	}
	return out
}

type mcpSupersedeOutcome struct {
	refused    bool
	retired    bool
	successors int
}

func (o mcpSupersedeOutcome) String() string {
	verdict := "accepted"
	if o.refused {
		verdict = "REFUSED"
	}
	return fmt.Sprintf("%s (retired=%v successors=%d)", verdict, o.retired, o.successors)
}

func (h *supersedesMCPHarness) outcome(target string, resp JSONRPCResponse) mcpSupersedeOutcome {
	h.t.Helper()
	ctx := context.Background()
	id, err := storage.ParseULID(target)
	if err != nil {
		h.t.Fatal(err)
	}
	ws := h.eng.Store().ResolveVaultPrefix(supersedesTestVault)
	eng, err := h.eng.Store().GetEngram(ctx, ws, id)
	if err != nil {
		h.t.Fatal(err)
	}
	succ, err := h.eng.Store().SupersedersOf(ctx, ws, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return mcpSupersedeOutcome{
		refused:    resp.Error != nil,
		retired:    !eng.ValidUntil.IsZero() && eng.State == storage.StateActive,
		successors: len(succ),
	}
}

// RED on develop: for full/write/static the inline form leaves the target
// live, and for an APPEND key muninn_remember writes the supersedes edge that
// muninn_link is forbidden to write.
func TestInlineSupersedes_MCPModeTableAgreesWithLink(t *testing.T) {
	h, cleanup := newSupersedesMCPHarness(t)
	defer cleanup()

	for _, m := range []struct{ name, token string }{
		{"full", "mk_full"},
		{"write", "mk_write"},
		{"append", "mk_append"},
		{"observe", "mk_observe"},
		{"none (static token)", "mdb_static"},
	} {
		t.Run(m.name, func(t *testing.T) {
			// A live target.
			lOld, lNew := h.seed("kiln firing schedule"), h.seed("kiln firing schedule")
			viaLink := h.outcome(lOld, h.call(m.token, "muninn_link", map[string]any{
				"source_id": lNew, "target_id": lOld, "relation": "supersedes"}))
			iOld := h.seed("glaze mixing ratio")
			viaInline := h.outcome(iOld, h.call(m.token, "muninn_remember", map[string]any{
				"concept":       "glaze mixing ratio",
				"content":       "The glaze ratio changed under " + m.name + " credentials.",
				"relationships": []any{map[string]any{"target_id": iOld, "relation": "supersedes"}}}))
			t.Logf("mode=%-20s live target      : muninn_link %v | muninn_remember %v", m.name, viaLink, viaInline)
			if viaLink != viaInline {
				t.Errorf("live target: muninn_link and an inline supersedes disagree.\n   muninn_link:     %v\n   muninn_remember: %v", viaLink, viaInline)
			}

			// A target that already has a successor.
			fl, fi := h.seed("wheel maintenance"), h.seed("drying shelf rotation")
			for _, old := range []string{fl, fi} {
				if _, err := h.eng.Link(context.Background(), &mbp.LinkRequest{Vault: supersedesTestVault,
					SourceID: h.seed("replacement"), TargetID: old, RelType: uint16(storage.RelSupersedes), Weight: 1}); err != nil {
					t.Fatalf("fixture link: %v", err)
				}
			}
			forkLink := h.outcome(fl, h.call(m.token, "muninn_link", map[string]any{
				"source_id": h.seed("rival"), "target_id": fl, "relation": "supersedes"}))
			forkInline := h.outcome(fi, h.call(m.token, "muninn_remember", map[string]any{
				"concept":       "drying shelf rotation",
				"content":       "A rival shelf note written under " + m.name + " credentials.",
				"relationships": []any{map[string]any{"target_id": fi, "relation": "supersedes"}}}))
			t.Logf("mode=%-20s superseded target: muninn_link %v | muninn_remember %v", m.name, forkLink, forkInline)
			if forkLink != forkInline {
				t.Errorf("already-superseded target: muninn_link and an inline supersedes disagree.\n   muninn_link:     %v\n   muninn_remember: %v", forkLink, forkInline)
			}
			if forkInline.successors != 1 {
				t.Errorf("an inline supersedes forked a memory: %d successors", forkInline.successors)
			}
		})
	}
}

// The refusal an agent sees names the reason and the current head, on the
// single write and per item in a batch — and each caller's weight is stored as
// supplied (the two tools' defaults differ on purpose; neither is rewritten).
//
// RED on develop: both writes succeed and fork the memory.
func TestInlineSupersedes_MCPRefusalNamesTheHead(t *testing.T) {
	h, cleanup := newSupersedesMCPHarness(t)
	defer cleanup()
	old := h.seed("glaze mixing ratio")

	first := h.call("mk_full", "muninn_remember", map[string]any{
		"concept": "glaze mixing ratio", "content": "Celadon is now blended four parts feldspar to one part whiting.",
		"relationships": []any{map[string]any{"target_id": old, "relation": "supersedes"}}})
	if first.Error != nil {
		t.Fatalf("first inline supersedes: %s", first.Error.Message)
	}
	head, _ := resultJSON(t, first)["id"].(string)

	// Each tool's own default weight reaches storage unchanged.
	linked := h.seed("wheel maintenance")
	linkSrc := h.seed("wheel maintenance")
	if r := h.call("mk_full", "muninn_link", map[string]any{"source_id": linkSrc, "target_id": linked, "relation": "supersedes"}); r.Error != nil {
		t.Fatalf("link: %s", r.Error.Message)
	}
	ws := h.eng.Store().ResolveVaultPrefix(supersedesTestVault)
	// The supersedes edge specifically: the novelty worker may add a `refines`
	// edge between two similar seeds, and that one is not under test.
	weightOf := func(src string) float32 {
		id, _ := storage.ParseULID(src)
		fwd, err := h.eng.Store().GetAssociations(context.Background(), ws, []storage.ULID{id}, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range fwd[id] {
			if e.RelType == storage.RelSupersedes {
				return e.Weight
			}
		}
		t.Fatalf("%s has no supersedes edge: %v", src, fwd[id])
		return 0
	}
	if got := weightOf(head); got != 0.9 {
		t.Errorf("inline supersedes edge weight = %v, want the relationships default 0.9", got)
	}
	if got := weightOf(linkSrc); got != 0.8 {
		t.Errorf("muninn_link supersedes edge weight = %v, want the link default 0.8", got)
	}

	second := h.call("mk_full", "muninn_remember", map[string]any{
		"concept": "glaze mixing ratio", "content": "Celadon is now blended five parts feldspar to one part whiting.",
		"relationships": []any{map[string]any{"target_id": old, "relation": "supersedes"}}})
	if second.Error == nil {
		t.Fatalf("a second inline supersedes of the same memory was accepted — a fork")
	}
	for _, want := range []string{head, "nothing was stored", "already has a successor"} {
		if !strings.Contains(second.Error.Message, want) {
			t.Errorf("the refusal should contain %q; got: %s", want, second.Error.Message)
		}
	}

	batch := h.call("mk_full", "muninn_remember_batch", map[string]any{"memories": []any{
		map[string]any{"concept": "drying shelf rotation", "content": "Greenware is rotated a quarter turn each morning."},
		map[string]any{"concept": "glaze mixing ratio", "content": "Celadon is now blended six parts feldspar to one part whiting.",
			"relationships": []any{map[string]any{"target_id": old, "relation": "supersedes"}}},
	}})
	if batch.Error != nil {
		t.Fatalf("batch: %s", batch.Error.Message)
	}
	results, _ := resultJSON(t, batch)["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("batch results: %v", results)
	}
	okItem, _ := results[0].(map[string]any)
	bad, _ := results[1].(map[string]any)
	if okItem["status"] != "ok" {
		t.Errorf("the ordinary batch item should be stored: %v", okItem)
	}
	if msg, _ := bad["error"].(string); bad["status"] != "error" || !strings.Contains(msg, head) {
		t.Errorf("the superseding batch item should be refused naming %s: %v", head, bad)
	}
}
