package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// fakeEngine implements EngineInterface for tests.
type fakeEngine struct{}

func (f *fakeEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	return &mbp.WriteResponse{ID: "fake-id"}, nil
}
func (f *fakeEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	responses := make([]*mbp.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i := range reqs {
		responses[i] = &mbp.WriteResponse{ID: fmt.Sprintf("batch-id-%d", i)}
	}
	return responses, errs
}
func (f *fakeEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return &mbp.ActivateResponse{}, nil
}
func (f *fakeEngine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return &mbp.ReadResponse{}, nil
}
func (f *fakeEngine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	return &mbp.ForgetResponse{}, nil
}
func (f *fakeEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	return &mbp.LinkResponse{}, nil
}
func (f *fakeEngine) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	return &mbp.StatResponse{}, nil
}
func (f *fakeEngine) GetContradictions(ctx context.Context, vault string) ([]ContradictionPair, error) {
	return nil, nil
}
func (f *fakeEngine) Evolve(ctx context.Context, vault, oldID, newContent, reason string) (*WriteResult, error) {
	return &WriteResult{ID: "new-id"}, nil
}
func (f *fakeEngine) Consolidate(ctx context.Context, vault string, ids []string, merged string) (*ConsolidateResult, error) {
	return &ConsolidateResult{ID: "merged-id"}, nil
}
func (f *fakeEngine) Session(ctx context.Context, vault string, since time.Time) (*SessionSummary, error) {
	return &SessionSummary{}, nil
}
func (f *fakeEngine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*WriteResult, error) {
	return &WriteResult{ID: "decision-id"}, nil
}

// Epic 18 fake implementations
func (f *fakeEngine) Restore(ctx context.Context, vault string, id string) (*RestoreResult, error) {
	return &RestoreResult{ID: id, Concept: "restored concept", State: "active"}, nil
}
func (f *fakeEngine) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResult, error) {
	return &TraverseResult{Nodes: []TraversalNode{}, Edges: []TraversalEdge{}}, nil
}
func (f *fakeEngine) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResult, error) {
	return &ExplainResult{EngramID: req.EngramID, WouldReturn: true, Threshold: 0.5}, nil
}
func (f *fakeEngine) UpdateState(ctx context.Context, vault string, id string, state string, reason string) error {
	return nil
}
func (f *fakeEngine) ListDeleted(ctx context.Context, vault string, limit int) ([]DeletedEngram, error) {
	return []DeletedEngram{}, nil
}
func (f *fakeEngine) RetryEnrich(ctx context.Context, vault string, id string) (*RetryEnrichResult, error) {
	return &RetryEnrichResult{EngramID: id, PluginsQueued: []string{}, AlreadyComplete: []string{}}, nil
}
func (f *fakeEngine) GetVaultPlasticity(_ context.Context, _ string) (*auth.ResolvedPlasticity, error) {
	r := auth.ResolvePlasticity(nil)
	return &r, nil
}
func (f *fakeEngine) RememberTree(_ context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	return &RememberTreeResult{RootID: "fake-root-id", NodeMap: map[string]string{}}, nil
}
func (f *fakeEngine) RecallTree(_ context.Context, vault, rootID string, maxDepth, limit int, includeCompleted bool) (*RecallTreeResult, error) {
	return &RecallTreeResult{Root: &TreeNode{ID: rootID, Concept: "fake", State: "active", Children: []TreeNode{}}}, nil
}
func (f *fakeEngine) AddChild(_ context.Context, vault, parentID string, child *AddChildRequest) (*AddChildResult, error) {
	return &AddChildResult{ChildID: "fake-child-id", Ordinal: 1}, nil
}
func (f *fakeEngine) CountChildren(_ context.Context, vault, engramID string) (int, error) {
	return 0, nil
}
func (f *fakeEngine) GetEnrichmentMode(_ context.Context) string {
	return "none"
}
func (f *fakeEngine) WhereLeftOff(_ context.Context, _ string, _ int) ([]WhereLeftOffEntry, error) {
	return []WhereLeftOffEntry{}, nil
}
func (f *fakeEngine) FindByEntity(_ context.Context, _, _ string, _ int) ([]*storage.Engram, error) {
	return nil, nil
}
func (f *fakeEngine) CheckIdempotency(_ context.Context, _ string) (*storage.IdempotencyReceipt, error) {
	return nil, nil
}
func (f *fakeEngine) WriteIdempotency(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeEngine) SetEntityState(_ context.Context, _, _, _, _ string) error {
	return nil
}
func (f *fakeEngine) SetEntityStateBatch(_ context.Context, ops []engine.EntityStateOp) []error {
	return make([]error, len(ops))
}
func (f *fakeEngine) GetEntityClusters(_ context.Context, _ string, _, _ int) ([]EntityClusterResult, error) {
	return []EntityClusterResult{}, nil
}
func (f *fakeEngine) ExportGraph(_ context.Context, _ string, _ bool) (*engine.ExportGraph, error) {
	return &engine.ExportGraph{Nodes: []engine.GraphNode{}, Edges: []engine.GraphEdge{}}, nil
}
func (f *fakeEngine) GetEntityTimeline(_ context.Context, _ string, _ string, _ int) (*engine.EntityTimeline, error) {
	return &engine.EntityTimeline{Entity: "test", FirstSeen: time.Now(), MentionCount: 0, Entries: []engine.TimelineEntry{}, Count: 0}, nil
}
func (f *fakeEngine) FindSimilarEntities(_ context.Context, _ string, _ float64, _ int) ([]engine.SimilarEntityPair, error) {
	return []engine.SimilarEntityPair{}, nil
}
func (f *fakeEngine) MergeEntity(_ context.Context, _, _, _ string, _ bool) (*engine.MergeEntityResult, error) {
	return &engine.MergeEntityResult{}, nil
}
func (f *fakeEngine) ReplayEnrichment(_ context.Context, _ string, _ []string, _ int, dryRun bool) (*engine.ReplayEnrichmentResult, error) {
	return &engine.ReplayEnrichmentResult{
		Processed: 3,
		Skipped:   1,
		Failed:    0,
		Remaining: 0,
		StagesRun: []string{"entities", "relationships", "classification", "summary"},
		DryRun:    dryRun,
	}, nil
}

func (f *fakeEngine) GetProvenance(_ context.Context, _, _ string) ([]ProvenanceEntry, error) {
	return []ProvenanceEntry{}, nil
}

func (f *fakeEngine) RecordFeedback(_ context.Context, _, _ string, _ bool) error {
	return nil
}

func (f *fakeEngine) GetEntityAggregate(_ context.Context, _, _ string, _ int) (*EntityAggregate, error) {
	return &EntityAggregate{
		Name: "test", Type: "person", Confidence: 1.0, MentionCount: 1, State: "active",
		Engrams: []EntityEngramSummary{}, Relationships: []EntityRelSummary{}, CoOccurring: []EntityCoOccurrence{},
	}, nil
}

func (f *fakeEngine) ListEntities(_ context.Context, _ string, _ int, _ string) ([]EntitySummary, error) {
	return []EntitySummary{}, nil
}

func newTestServer() *MCPServer {
	return New(":0", &fakeEngine{}, "", nil)
}

func postRPC(t *testing.T, srv *MCPServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	return w
}

func TestMalformedJSONRPC(t *testing.T) {
	srv := newTestServer()

	cases := []struct {
		name        string
		body        string
		wantErrCode int
	}{
		{
			name:        "invalid JSON",
			body:        `{not json`,
			wantErrCode: -32700,
		},
		{
			name:        "jsonrpc not 2.0",
			body:        `{"jsonrpc":"1.0","method":"tools/call","id":1}`,
			wantErrCode: -32600,
		},
		{
			name:        "missing method",
			body:        `{"jsonrpc":"2.0","id":1}`,
			wantErrCode: -32601,
		},
		{
			name:        "unknown method",
			body:        `{"jsonrpc":"2.0","method":"unknown","id":1}`,
			wantErrCode: -32601,
		},
		{
			name:        "unknown tool name",
			body:        `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_nonexistent","arguments":{"vault":"default"}}}`,
			wantErrCode: -32602,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postRPC(t, srv, tc.body)
			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error == nil {
				t.Fatal("expected error in response, got nil")
			}
			if resp.Error.Code != tc.wantErrCode {
				t.Errorf("error code = %d, want %d", resp.Error.Code, tc.wantErrCode)
			}
		})
	}
}

func TestBodySizeLimit(t *testing.T) {
	srv := newTestServer()
	big := bytes.Repeat([]byte("x"), 2<<20)
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(big))
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Error("expected non-200 for oversized body")
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/mcp/health", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", w.Code)
	}
}

func TestListTools(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/mcp/tools", nil)
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("tools status = %d, want 200", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	tools, _ := result["tools"].([]any)
	if len(tools) != 36 {
		t.Errorf("expected 36 tools, got %d", len(tools))
	}
}

func TestHandleRememberHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test content"}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Error("expected non-nil result")
	}
}

func TestHandleRecallHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test query"]}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

func TestHandleRememberBatchHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"memory one","concept":"c1"},{"content":"memory two","tags":["tag1"]}]}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Error("expected non-nil result")
	}
}

func TestHandleRememberBatchEmptyMemories(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[]}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty memories, got %v", resp.Error)
	}
}

func TestHandleRememberBatchExceedsLimit(t *testing.T) {
	srv := newTestServer()
	memories := make([]map[string]any, 51)
	for i := range memories {
		memories[i] = map[string]any{"content": fmt.Sprintf("item %d", i)}
	}
	args, _ := json.Marshal(map[string]any{"vault": "default", "memories": memories})
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":%s}}`, string(args))
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for >50 memories, got %v", resp.Error)
	}
}

func TestHandleRememberBatchMissingContent(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"concept":"no content"}]}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing content, got %v", resp.Error)
	}
}

func TestHandleRememberMissingContent(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleRemember_RejectsWhitespaceContent(t *testing.T) {
	srv := newTestServer()
	// Attempt to remember with whitespace-only content (spaces, tabs, newlines).
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"   "}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200 (JSON-RPC errors return 200)", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for whitespace-only content, got nil")
	}
	// Expect a parameter validation error code (-32602).
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", resp.Error.Code)
	}
}

func TestHandleConsolidateExceedsLimit(t *testing.T) {
	srv := newTestServer()
	ids := make([]string, 51)
	for i := range ids {
		ids[i] = "id"
	}
	b, _ := json.Marshal(map[string]any{
		"vault": "default", "ids": ids, "merged_content": "merged",
	})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":` + string(b) + `}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for >50 ids, got %v", resp.Error)
	}
}

func TestHandleEvolveRequiredParams(t *testing.T) {
	srv := newTestServer()
	// Missing new_content
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"abc","reason":"why"}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing new_content, got %v", resp.Error)
	}
}

func TestHandleSessionInvalidSince(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_session","arguments":{"vault":"default","since":"not-a-timestamp"}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid since, got %v", resp.Error)
	}
}

func TestApplyTypeArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		wantType   uint8
		wantLabel  string
	}{
		{
			name:      "enum name sets both type and label",
			args:      map[string]any{"type": "decision"},
			wantType:  1, // TypeDecision
			wantLabel: "decision",
		},
		{
			name:      "issue alias for bugfix",
			args:      map[string]any{"type": "bugfix"},
			wantType:  4, // TypeIssue
			wantLabel: "bugfix",
		},
		{
			name:      "new enum type procedure",
			args:      map[string]any{"type": "procedure"},
			wantType:  6, // TypeProcedure
			wantLabel: "procedure",
		},
		{
			name:      "free-form label defaults to TypeFact",
			args:      map[string]any{"type": "architectural_decision"},
			wantType:  0, // TypeFact (default)
			wantLabel: "architectural_decision",
		},
		{
			name:      "explicit type_label overrides inferred",
			args:      map[string]any{"type": "decision", "type_label": "architectural_decision"},
			wantType:  1, // TypeDecision
			wantLabel: "architectural_decision",
		},
		{
			name:      "type_label alone",
			args:      map[string]any{"type_label": "custom_label"},
			wantType:  0, // default
			wantLabel: "custom_label",
		},
		{
			name:      "no type params",
			args:      map[string]any{},
			wantType:  0,
			wantLabel: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &mbp.WriteRequest{}
			applyTypeArgs(tc.args, req)
			if req.MemoryType != tc.wantType {
				t.Errorf("MemoryType = %d, want %d", req.MemoryType, tc.wantType)
			}
			if req.TypeLabel != tc.wantLabel {
				t.Errorf("TypeLabel = %q, want %q", req.TypeLabel, tc.wantLabel)
			}
		})
	}
}

func TestApplyEnrichmentArgs(t *testing.T) {
	tests := []struct {
		name         string
		args         map[string]any
		wantSummary  string
		wantEntities int
		wantRels     int
	}{
		{
			name:    "no enrichment fields",
			args:    map[string]any{},
		},
		{
			name:        "summary only",
			args:        map[string]any{"summary": "A quick summary"},
			wantSummary: "A quick summary",
		},
		{
			name: "entities only",
			args: map[string]any{
				"entities": []any{
					map[string]any{"name": "PostgreSQL", "type": "database"},
					map[string]any{"name": "Auth Service", "type": "service"},
				},
			},
			wantEntities: 2,
		},
		{
			name: "relationships only",
			args: map[string]any{
				"relationships": []any{
					map[string]any{"target_id": "01ABC", "relation": "depends_on", "weight": 0.9},
					map[string]any{"target_id": "01DEF", "relation": "supports"},
				},
			},
			wantRels: 2,
		},
		{
			name: "all fields together",
			args: map[string]any{
				"summary": "Full enrichment",
				"entities": []any{
					map[string]any{"name": "Redis", "type": "cache"},
				},
				"relationships": []any{
					map[string]any{"target_id": "01XYZ", "relation": "relates_to"},
				},
			},
			wantSummary:  "Full enrichment",
			wantEntities: 1,
			wantRels:     1,
		},
		{
			name: "invalid entity skipped",
			args: map[string]any{
				"entities": []any{
					map[string]any{"name": "Valid", "type": "tool"},
					map[string]any{"name": "", "type": "tool"},       // empty name
					map[string]any{"name": "NoType"},                  // missing type
					"not an object",                                   // wrong type
				},
			},
			wantEntities: 1,
		},
		{
			name: "invalid relationship skipped",
			args: map[string]any{
				"relationships": []any{
					map[string]any{"target_id": "01ABC", "relation": "supports"},
					map[string]any{"target_id": "", "relation": "supports"},        // empty target
					map[string]any{"target_id": "01ABC", "relation": ""},           // empty relation
				},
			},
			wantRels: 1,
		},
		{
			name: "relationship default weight",
			args: map[string]any{
				"relationships": []any{
					map[string]any{"target_id": "01ABC", "relation": "supports"},
				},
			},
			wantRels: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := &mbp.WriteRequest{}
			applyEnrichmentArgs(tc.args, req)
			if req.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", req.Summary, tc.wantSummary)
			}
			if len(req.Entities) != tc.wantEntities {
				t.Errorf("Entities count = %d, want %d", len(req.Entities), tc.wantEntities)
			}
			if len(req.Relationships) != tc.wantRels {
				t.Errorf("Relationships count = %d, want %d", len(req.Relationships), tc.wantRels)
			}
		})
	}
}

func TestApplyEnrichmentArgs_RelationshipWeight(t *testing.T) {
	args := map[string]any{
		"relationships": []any{
			map[string]any{"target_id": "01ABC", "relation": "supports"},
			map[string]any{"target_id": "01DEF", "relation": "depends_on", "weight": 0.5},
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Relationships) != 2 {
		t.Fatalf("expected 2 relationships, got %d", len(req.Relationships))
	}
	if req.Relationships[0].Weight != 0.9 {
		t.Errorf("default weight = %f, want 0.9", req.Relationships[0].Weight)
	}
	if req.Relationships[1].Weight != 0.5 {
		t.Errorf("explicit weight = %f, want 0.5", req.Relationships[1].Weight)
	}
}

func TestHandleRememberWithEnrichment(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{
		"vault":"default",
		"content":"PostgreSQL chosen for persistence layer",
		"summary":"Chose PostgreSQL for persistence",
		"entities":[{"name":"PostgreSQL","type":"database"}],
		"relationships":[{"target_id":"01ABC","relation":"depends_on","weight":0.9}]
	}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

func TestHandleRememberBatchWithEnrichment(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{
		"vault":"default",
		"memories":[
			{"content":"memory one","summary":"Summary for memory one","entities":[{"name":"Redis","type":"cache"}]},
			{"content":"memory two","type":"decision"}
		]
	}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
}

func TestToolsCallResponseFormat(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":42,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"MCP envelope test"}}}`
	w := postRPC(t, srv, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if raw["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want \"2.0\"", raw["jsonrpc"])
	}
	if raw["id"] == nil {
		t.Error("missing \"id\" in response")
	}

	result, ok := raw["result"].(map[string]any)
	if !ok {
		t.Fatalf("result should be an object (map), got %T", raw["result"])
	}

	contentRaw, exists := result["content"]
	if !exists {
		t.Fatal("result missing \"content\" key — bare array is no longer MCP-compliant")
	}
	content, ok := contentRaw.([]any)
	if !ok {
		t.Fatalf("result.content should be []any, got %T", contentRaw)
	}
	if len(content) == 0 {
		t.Fatal("result.content is empty")
	}

	elem, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("result.content[0] should be an object, got %T", content[0])
	}
	if elem["type"] != "text" {
		t.Errorf("content[0].type = %v, want \"text\"", elem["type"])
	}

	text, ok := elem["text"].(string)
	if !ok || text == "" {
		t.Fatalf("content[0].text should be a non-empty string, got %T", elem["text"])
	}

	var inner map[string]any
	if err := json.Unmarshal([]byte(text), &inner); err != nil {
		t.Fatalf("content[0].text is not valid JSON: %v", err)
	}
	if _, hasID := inner["id"]; !hasID {
		t.Error("inner JSON payload missing \"id\" field")
	}
}
