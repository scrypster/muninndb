package mcp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// filterCapturingEngine records the Filters from the last ActivateRequest.
type filterCapturingEngine struct {
	fakeEngine
	lastFilters []mbp.Filter
}

func (e *filterCapturingEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.lastFilters = req.Filters
	return &mbp.ActivateResponse{}, nil
}

func findFilter(filters []mbp.Filter, field string) (mbp.Filter, bool) {
	for _, f := range filters {
		if f.Field == field {
			return f, true
		}
	}
	return mbp.Filter{}, false
}

// TestHandleRecall_TagFilters_Wired verifies #479: tags_all / tags_any /
// tag_filter recall args are parsed into the matching engine Filters.
func TestHandleRecall_TagFilters_Wired(t *testing.T) {
	eng := &filterCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{
		"vault":"default","context":["open loops"],
		"tags_all":["truth:current"],
		"tags_any":["status:open","status:waiting"],
		"tag_filter":{"prefix":"due:","lte":"2026-06-17"}
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	all, ok := findFilter(eng.lastFilters, "tags_all")
	if !ok || asStrings(all.Value)[0] != "truth:current" {
		t.Errorf("tags_all filter missing/wrong: %+v", all)
	}
	any, ok := findFilter(eng.lastFilters, "tags_any")
	if !ok || len(asStrings(any.Value)) != 2 {
		t.Errorf("tags_any filter missing/wrong: %+v", any)
	}
	tp, ok := findFilter(eng.lastFilters, "tag_prefix")
	if !ok || tp.Op != "lte" {
		t.Fatalf("tag_prefix filter missing/wrong: %+v", tp)
	}
	if pair, ok := tp.Value.([2]string); !ok || pair[0] != "due:" || pair[1] != "2026-06-17" {
		t.Errorf("tag_prefix value = %+v, want [due: 2026-06-17]", tp.Value)
	}
}

// TestHandleRecall_TagFilter_RequiresPrefix verifies a tag_filter without a
// prefix is rejected.
func TestHandleRecall_TagFilter_RequiresPrefix(t *testing.T) {
	eng := &filterCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{
		"vault":"default","context":["x"],"tag_filter":{"lte":"2026-06-17"}
	}}}`
	w := postRPC(t, srv, body)
	if resp := decodeResp(t, w.Body.String()); resp.Error == nil {
		t.Error("expected error for tag_filter without prefix")
	}
}

// TestHandleRecall_TagFilter_RejectsNonObject verifies that a tag_filter passed
// as a non-object (e.g. the string "due:2026-06-17", a natural mistake given the
// tags_all/tags_any args ARE strings) is REJECTED, not silently ignored. Before
// the fix the type assertion args["tag_filter"].(map[string]any) failed, the whole
// block was skipped, and recall ran UNFILTERED — returning plausible-but-wrong
// results with no error. That is a silent fail-open: a filter the caller believes
// is active is a no-op (principle #1 — explicit config is never silently dropped).
func TestHandleRecall_TagFilter_RejectsNonObject(t *testing.T) {
	eng := &filterCapturingEngine{}
	srv := newTestServerWith(eng)
	for _, malformed := range []string{
		`"skill:email"`,      // string (the reported mistake)
		`["due:","2026-06"]`, // array
		`42`,                 // number
	} {
		body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{
			"vault":"default","context":["x"],"tag_filter":` + malformed + `
		}}}`
		w := postRPC(t, srv, body)
		resp := decodeResp(t, w.Body.String())
		if resp.Error == nil {
			t.Errorf("tag_filter=%s: expected error, got none (filter silently dropped)", malformed)
		}
		if len(eng.lastFilters) != 0 {
			t.Errorf("tag_filter=%s: engine was called with filters %+v; a rejected recall must not reach the engine", malformed, eng.lastFilters)
		}
		eng.lastFilters = nil
	}
}

func asStrings(v interface{}) []string {
	if s, ok := v.([]string); ok {
		return s
	}
	return nil
}
