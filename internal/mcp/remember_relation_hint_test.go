package mcp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The inline `relationships` argument of muninn_remember and
// muninn_remember_batch goes through the same relation coercion as muninn_link:
// a relation outside the 16 is stored as relates_to (#927). #745 made
// muninn_link say so; the remember paths did not. Storage is unchanged here as
// well: the relation still reaches the engine exactly as written, the engine
// still resolves it to relates_to, and nothing is rejected. The caller is told.

// relCaptureEngine records the relationships each write request carried when
// it reached the engine, so a test can show they are passed through untouched.
type relCaptureEngine struct {
	fakeEngine
	single []mbp.InlineRelationship
	batch  [][]mbp.InlineRelationship
}

func (e *relCaptureEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.single = append([]mbp.InlineRelationship(nil), req.Relationships...)
	return e.fakeEngine.Write(ctx, req)
}

func (e *relCaptureEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	e.batch = nil
	for _, r := range reqs {
		e.batch = append(e.batch, append([]mbp.InlineRelationship(nil), r.Relationships...))
	}
	return e.fakeEngine.WriteBatch(ctx, reqs)
}

func callRememberTool(t *testing.T, srv *MCPServer, tool string, args map[string]any) map[string]any {
	t.Helper()
	w := postRPC(t, srv, string(mkToolCallBody(tool, args)))
	return extractInnerJSON(t, decodeResp(t, w.Body.String()))
}

func relationsOf(rels []mbp.InlineRelationship) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.Relation
	}
	return out
}

const (
	relTargetA = "01JZZZZZZZZZZZZZZZZZZZZZZA"
	relTargetB = "01JZZZZZZZZZZZZZZZZZZZZZZB"
	relTargetC = "01JZZZZZZZZZZZZZZZZZZZZZZC"
)

func TestRemember_UnknownRelationIsReported(t *testing.T) {
	eng := &relCaptureEngine{}
	srv := newTestServerWith(eng)

	out := callRememberTool(t, srv, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "the new timetable replaces the old one",
		"content": "the branch line now runs every thirty minutes",
		"relationships": []any{
			map[string]any{"target_id": relTargetA, "relation": "superseded_by"},
			map[string]any{"target_id": relTargetB, "relation": "supports"},
			map[string]any{"target_id": relTargetC, "relation": "contradicted_by"},
		},
	})
	hint, _ := out["hint"].(string)
	for _, want := range []string{`"superseded_by"`, `"contradicted_by"`, `stored as "relates_to"`} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint must contain %s; got %q", want, hint)
		}
	}
	if strings.Contains(hint, `"supports"`) {
		t.Errorf("a recognised relation must not be reported; got %q", hint)
	}
	// The caller can only correct the relation if the hint names the valid
	// ones, and every one of them.
	for _, rel := range relationNames() {
		if !strings.Contains(hint, rel) {
			t.Errorf("hint must list the valid relation %q; got %q", rel, hint)
		}
	}
	// Storage unchanged: nothing dropped or rewritten before the engine.
	want := []string{"superseded_by", "supports", "contradicted_by"}
	if got := relationsOf(eng.single); !reflect.DeepEqual(got, want) {
		t.Errorf("engine received relations %v, want %v unchanged", got, want)
	}

	// Recognised relations only: no relation sentence at all.
	out = callRememberTool(t, srv, "muninn_remember", map[string]any{
		"vault":   "default",
		"content": "the goods yard shunter is a small diesel",
		"relationships": []any{
			map[string]any{"target_id": relTargetA, "relation": "supersedes"},
		},
	})
	if hint, _ := out["hint"].(string); strings.Contains(hint, "not recognised") {
		t.Errorf("a recognised relation produced a notice: %q", hint)
	}
}

// One item's unknown relation must be reported on that item only: an item
// with valid relations gets no relation notice, and an item with its own
// unknown relation is not told about another item's.
func TestRememberBatch_UnknownRelationIsReportedPerItem(t *testing.T) {
	eng := &relCaptureEngine{}
	srv := newTestServerWith(eng)

	out := callRememberTool(t, srv, "muninn_remember_batch", map[string]any{
		"vault": "default",
		"memories": []any{
			map[string]any{
				"content":       "item A: the up starter is a semaphore",
				"relationships": []any{map[string]any{"target_id": relTargetA, "relation": "superseded_by"}},
			},
			map[string]any{
				"content":       "item B: the distant signal is fixed at caution",
				"relationships": []any{map[string]any{"target_id": relTargetB, "relation": "supports"}},
			},
			map[string]any{
				"content":       "item C: the down home signal was renewed",
				"relationships": []any{map[string]any{"target_id": relTargetC, "relation": "contradicted_by"}},
			},
		},
	})
	results, _ := out["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %v", out)
	}
	hintOf := func(i int) string {
		item, _ := results[i].(map[string]any)
		if item["status"] != "ok" {
			t.Errorf("results[%d].status = %v, want ok (nothing is rejected)", i, item["status"])
		}
		h, _ := item["hint"].(string)
		return h
	}

	hintA := hintOf(0)
	if !strings.Contains(hintA, `"superseded_by"`) || !strings.Contains(hintA, `stored as "relates_to"`) {
		t.Errorf("item A must be told its relation was stored as relates_to; got %q", hintA)
	}
	if strings.Contains(hintA, "contradicted_by") {
		t.Errorf("item A's hint carries item C's relation: %q", hintA)
	}

	hintB := hintOf(1)
	if strings.Contains(hintB, "superseded_by") || strings.Contains(hintB, "contradicted_by") || strings.Contains(hintB, "not recognised") {
		t.Errorf("item B has only valid relations and must get no relation notice; got %q", hintB)
	}

	hintC := hintOf(2)
	if !strings.Contains(hintC, `"contradicted_by"`) {
		t.Errorf("item C must be told about its own relation; got %q", hintC)
	}
	if strings.Contains(hintC, "superseded_by") {
		t.Errorf("item C's hint carries item A's relation: %q", hintC)
	}

	want := [][]string{{"superseded_by"}, {"supports"}, {"contradicted_by"}}
	if len(eng.batch) != len(want) {
		t.Fatalf("engine received %d batch items, want %d", len(eng.batch), len(want))
	}
	for i, rels := range eng.batch {
		if got := relationsOf(rels); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("engine received item %d relations %v, want %v unchanged", i, got, want[i])
		}
	}
}

// The same loop dropped two more kinds of relationship without a word: an
// item that is not an object or has no target_id or relation, and every item
// past the 30th. Both are still dropped; the hint now counts them.
func TestRemember_SkippedAndOverCapRelationshipsAreReported(t *testing.T) {
	eng := &relCaptureEngine{}
	srv := newTestServerWith(eng)

	rels := []any{
		"not an object",
		map[string]any{"target_id": "", "relation": "supports"},
		map[string]any{"target_id": relTargetA},
	}
	// 35 items against the cap of 30. Literal numbers, not
	// maxInlineRelationships, so reverting handlers.go fails this test
	// rather than the build.
	for len(rels) < 35 {
		rels = append(rels, map[string]any{"target_id": relTargetB, "relation": "supports"})
	}
	out := callRememberTool(t, srv, "muninn_remember", map[string]any{
		"vault":         "default",
		"content":       "the signal box closed when the line was resignalled",
		"relationships": rels,
	})
	hint, _ := out["hint"].(string)
	if !strings.Contains(hint, "3 relationship item(s) were skipped") {
		t.Errorf("hint must count the 3 unusable items; got %q", hint)
	}
	if !strings.Contains(hint, "Only the first 30 relationships are read; 5 more were ignored") {
		t.Errorf("hint must count the 5 items past the cap; got %q", hint)
	}
	// Still dropped, exactly as before: 30 read, 3 of them unusable.
	if got := len(eng.single); got != 27 {
		t.Errorf("engine received %d relationships, want 27", got)
	}

	// Nothing dropped: no count sentence.
	out = callRememberTool(t, srv, "muninn_remember", map[string]any{
		"vault":         "default",
		"content":       "the goods loop holds forty wagons",
		"relationships": []any{map[string]any{"target_id": relTargetA, "relation": "supports"}},
	})
	if hint, _ := out["hint"].(string); strings.Contains(hint, "skipped") || strings.Contains(hint, "ignored") {
		t.Errorf("a clean relationships list produced a count notice: %q", hint)
	}
}
