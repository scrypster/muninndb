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

func asStrings(v interface{}) []string {
	if s, ok := v.([]string); ok {
		return s
	}
	return nil
}
