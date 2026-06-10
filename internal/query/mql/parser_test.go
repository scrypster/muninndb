package mql

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestParse_BasicActivate tests a basic ACTIVATE query with CONTEXT.
func TestParse_BasicActivate(t *testing.T) {
	input := `ACTIVATE FROM myvault CONTEXT ["memory", "sleep"]`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.Vault != "myvault" {
		t.Errorf("expected vault 'myvault', got %q", activateQuery.Vault)
	}
	if len(activateQuery.Context) != 2 {
		t.Errorf("expected 2 context terms, got %d", len(activateQuery.Context))
	}
	if activateQuery.Context[0] != "memory" {
		t.Errorf("expected first term 'memory', got %q", activateQuery.Context[0])
	}
	if activateQuery.Context[1] != "sleep" {
		t.Errorf("expected second term 'sleep', got %q", activateQuery.Context[1])
	}
	if activateQuery.Where != nil {
		t.Errorf("expected no WHERE clause, got %v", activateQuery.Where)
	}
}

// TestParse_WithWhere tests ACTIVATE with WHERE clause.
func TestParse_WithWhere(t *testing.T) {
	input := `ACTIVATE FROM vault1 CONTEXT ["x"] WHERE state = active`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.Where == nil {
		t.Fatal("expected WHERE clause, got nil")
	}
	pred, ok := activateQuery.Where.(*StatePredicate)
	if !ok {
		t.Fatalf("expected StatePredicate, got %T", activateQuery.Where)
	}
	if pred.State != "active" {
		t.Errorf("expected state 'active', got %q", pred.State)
	}
}

// TestParse_WithAndOr tests compound predicates with AND/OR.
func TestParse_WithAndOr(t *testing.T) {
	input := `ACTIVATE FROM v1 CONTEXT ["x"] WHERE state = active AND relevance > 0.5`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	andPred, ok := activateQuery.Where.(*AndPredicate)
	if !ok {
		t.Fatalf("expected AndPredicate, got %T", activateQuery.Where)
	}
	if _, ok := andPred.Left.(*StatePredicate); !ok {
		t.Fatalf("expected left to be StatePredicate, got %T", andPred.Left)
	}
	if _, ok := andPred.Right.(*ScorePredicate); !ok {
		t.Fatalf("expected right to be ScorePredicate, got %T", andPred.Right)
	}
}

// TestParse_WithAllClauses tests all optional clauses.
func TestParse_WithAllClauses(t *testing.T) {
	input := `ACTIVATE FROM vault2 CONTEXT ["a", "b"] WHERE state = completed MAX_RESULTS 50 HOPS 3 MIN_RELEVANCE 0.7`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.MaxResults != 50 {
		t.Errorf("expected MAX_RESULTS 50, got %d", activateQuery.MaxResults)
	}
	if activateQuery.Hops != 3 {
		t.Errorf("expected HOPS 3, got %d", activateQuery.Hops)
	}
	if activateQuery.MinRelevance != 0.7 {
		t.Errorf("expected MIN_RELEVANCE 0.7, got %f", activateQuery.MinRelevance)
	}
}

// TestParse_InvalidSyntax tests error handling for malformed queries.
func TestParse_InvalidSyntax(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "missing ACTIVATE",
			input: `FROM v CONTEXT ["x"]`,
		},
		{
			name:  "missing FROM",
			input: `ACTIVATE vault CONTEXT ["x"]`,
		},
		{
			name:  "missing CONTEXT",
			input: `ACTIVATE FROM v ["x"]`,
		},
		{
			name:  "empty context",
			input: `ACTIVATE FROM v CONTEXT []`,
		},
		{
			name:  "missing closing bracket",
			input: `ACTIVATE FROM v CONTEXT ["x"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.input)
			if err == nil {
				t.Errorf("expected error for input: %s", tc.input)
			}
		})
	}
}

// MockEngine is a mock engine for testing executor.
type MockEngine struct {
	lastRequest *mbp.ActivateRequest
	response    *mbp.ActivateResponse
}

func (m *MockEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	m.lastRequest = req
	if m.response == nil {
		m.response = &mbp.ActivateResponse{}
	}
	return m.response, nil
}

// TestExecute_BuildsRequest tests that Execute properly builds an ActivateRequest.
func TestExecute_BuildsRequest(t *testing.T) {
	query := &ActivateQuery{
		Vault:        "test_vault",
		Context:      []string{"memory", "learning"},
		MaxResults:   25,
		Hops:         3,
		MinRelevance: 0.6,
		Where:        &StatePredicate{State: "active"},
	}

	engine := &MockEngine{
		response: &mbp.ActivateResponse{
			Activations: []mbp.ActivationItem{},
		},
	}

	resp, err := Execute(context.Background(), engine, query)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if engine.lastRequest.Vault != "test_vault" {
		t.Errorf("expected vault 'test_vault', got %q", engine.lastRequest.Vault)
	}
	if len(engine.lastRequest.Context) != 2 {
		t.Errorf("expected 2 context terms, got %d", len(engine.lastRequest.Context))
	}
	if engine.lastRequest.MaxResults != 25 {
		t.Errorf("expected MaxResults 25, got %d", engine.lastRequest.MaxResults)
	}
	if engine.lastRequest.MaxHops != 3 {
		t.Errorf("expected MaxHops 3, got %d", engine.lastRequest.MaxHops)
	}

	if resp == nil {
		t.Error("expected non-nil response")
	}
}

// TestParse_TagPredicate tests tag = "string" predicate.
func TestParse_TagPredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE tag = "important"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*TagPredicate)
	if !ok {
		t.Fatalf("expected TagPredicate, got %T", activateQuery.Where)
	}
	if pred.Tag != "important" {
		t.Errorf("expected tag 'important', got %q", pred.Tag)
	}
}

// TestParse_CreatorPredicate tests creator = "string" predicate.
func TestParse_CreatorPredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE creator = "alice"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*CreatorPredicate)
	if !ok {
		t.Fatalf("expected CreatorPredicate, got %T", activateQuery.Where)
	}
	if pred.Creator != "alice" {
		t.Errorf("expected creator 'alice', got %q", pred.Creator)
	}
}

// TestParse_ScorePredicate tests relevance and confidence predicates.
func TestParse_ScorePredicate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		field string
		op    string
		value float32
	}{
		{
			name:  "relevance >",
			input: `ACTIVATE FROM v CONTEXT ["x"] WHERE relevance > 0.5`,
			field: "relevance",
			op:    ">",
			value: 0.5,
		},
		{
			name:  "confidence >=",
			input: `ACTIVATE FROM v CONTEXT ["x"] WHERE confidence >= 0.8`,
			field: "confidence",
			op:    ">=",
			value: 0.8,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query, err := Parse(tc.input)
			if err != nil {
				t.Fatalf("Parse failed: %v", err)
			}

			activateQuery, ok := query.(*ActivateQuery)
			if !ok {
				t.Fatalf("expected ActivateQuery, got %T", query)
			}

			pred, ok := activateQuery.Where.(*ScorePredicate)
			if !ok {
				t.Fatalf("expected ScorePredicate, got %T", activateQuery.Where)
			}
			if pred.Field != tc.field {
				t.Errorf("expected field %q, got %q", tc.field, pred.Field)
			}
			if pred.Op != tc.op {
				t.Errorf("expected op %q, got %q", tc.op, pred.Op)
			}
			if pred.Value != tc.value {
				t.Errorf("expected value %f, got %f", tc.value, pred.Value)
			}
		})
	}
}

// TestParse_CreatedAfterPredicate tests created_after predicate.
func TestParse_CreatedAfterPredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE created_after "2024-01-01T00:00:00Z"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*CreatedAfterPredicate)
	if !ok {
		t.Fatalf("expected CreatedAfterPredicate, got %T", activateQuery.Where)
	}
	expected, _ := time.Parse(time.RFC3339, "2024-01-01T00:00:00Z")
	if !pred.After.Equal(expected) {
		t.Errorf("expected After %v, got %v", expected, pred.After)
	}
}

// TestParse_CaseInsensitive tests that keywords are case-insensitive.
func TestParse_CaseInsensitive(t *testing.T) {
	input := `activate FROM myvault context ["x"]`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.Vault != "myvault" {
		t.Errorf("expected vault 'myvault', got %q", activateQuery.Vault)
	}
}

// TestParse_ParenthesizedPredicate tests parenthesized predicates.
func TestParse_ParenthesizedPredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE (state = active)`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*StatePredicate)
	if !ok {
		t.Fatalf("expected StatePredicate, got %T", activateQuery.Where)
	}
	if pred.State != "active" {
		t.Errorf("expected state 'active', got %q", pred.State)
	}
}

// TestTokenize tests the lexer.
func TestTokenize(t *testing.T) {
	input := `ACTIVATE FROM myvault CONTEXT ["term1", "term2"]`
	tokens := Tokenize(input)

	expectedTypes := []TokenType{
		TokenActivate,
		TokenFrom,
		TokenIdent,
		TokenContext,
		TokenLBracket,
		TokenString,
		TokenComma,
		TokenString,
		TokenRBracket,
		TokenEOF,
	}

	if len(tokens) != len(expectedTypes) {
		t.Errorf("expected %d tokens, got %d", len(expectedTypes), len(tokens))
	}

	for i, expected := range expectedTypes {
		if i >= len(tokens) {
			break
		}
		if tokens[i].Type != expected {
			t.Errorf("token %d: expected %s, got %s", i, expected, tokens[i].Type)
		}
	}
}

// TestParse_RecallEpisode tests RECALL EPISODE query parsing.
func TestParse_RecallEpisode(t *testing.T) {
	input := `RECALL EPISODE "01ARZ3NDEKTSV4RRFFQ69G5FAV" FRAMES 10`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	recallQuery, ok := query.(*RecallEpisodeQuery)
	if !ok {
		t.Fatalf("expected RecallEpisodeQuery, got %T", query)
	}

	if recallQuery.EpisodeID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("expected episode ID '01ARZ3NDEKTSV4RRFFQ69G5FAV', got %q", recallQuery.EpisodeID)
	}

	if recallQuery.Frames != 10 {
		t.Errorf("expected frames 10, got %d", recallQuery.Frames)
	}
}

// TestParse_RecallEpisode_NoFrames tests RECALL EPISODE without FRAMES clause.
func TestParse_RecallEpisode_NoFrames(t *testing.T) {
	input := `RECALL EPISODE "01ARZ3NDEKTSV4RRFFQ69G5FAV"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	recallQuery, ok := query.(*RecallEpisodeQuery)
	if !ok {
		t.Fatalf("expected RecallEpisodeQuery, got %T", query)
	}

	if recallQuery.Frames != 0 {
		t.Errorf("expected frames 0 (all), got %d", recallQuery.Frames)
	}
}

// TestParse_Traverse tests TRAVERSE query parsing.
func TestParse_Traverse(t *testing.T) {
	input := `TRAVERSE FROM "01ARZ3NDEKTSV4RRFFQ69G5FAV" HOPS 3 MIN_WEIGHT 0.5`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	traverseQuery, ok := query.(*TraverseQuery)
	if !ok {
		t.Fatalf("expected TraverseQuery, got %T", query)
	}

	if traverseQuery.StartID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("expected start ID '01ARZ3NDEKTSV4RRFFQ69G5FAV', got %q", traverseQuery.StartID)
	}

	if traverseQuery.Hops != 3 {
		t.Errorf("expected hops 3, got %d", traverseQuery.Hops)
	}

	if traverseQuery.MinWeight != 0.5 {
		t.Errorf("expected min_weight 0.5, got %f", traverseQuery.MinWeight)
	}
}

// TestParse_Traverse_NoMinWeight tests TRAVERSE without MIN_WEIGHT clause.
func TestParse_Traverse_NoMinWeight(t *testing.T) {
	input := `TRAVERSE FROM "engram123" HOPS 2`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	traverseQuery, ok := query.(*TraverseQuery)
	if !ok {
		t.Fatalf("expected TraverseQuery, got %T", query)
	}

	if traverseQuery.MinWeight != 0.0 {
		t.Errorf("expected min_weight 0.0 (default), got %f", traverseQuery.MinWeight)
	}
}

// TestParse_Consolidate tests CONSOLIDATE VAULT query parsing.
func TestParse_Consolidate(t *testing.T) {
	input := `CONSOLIDATE VAULT myvault DRY_RUN`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	consolidateQuery, ok := query.(*ConsolidateQuery)
	if !ok {
		t.Fatalf("expected ConsolidateQuery, got %T", query)
	}

	if consolidateQuery.Vault != "myvault" {
		t.Errorf("expected vault 'myvault', got %q", consolidateQuery.Vault)
	}

	if !consolidateQuery.DryRun {
		t.Errorf("expected DryRun true, got false")
	}
}

// TestParse_Consolidate_NoDryRun tests CONSOLIDATE without DRY_RUN flag.
func TestParse_Consolidate_NoDryRun(t *testing.T) {
	input := `CONSOLIDATE VAULT "prod_vault"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	consolidateQuery, ok := query.(*ConsolidateQuery)
	if !ok {
		t.Fatalf("expected ConsolidateQuery, got %T", query)
	}

	if consolidateQuery.DryRun {
		t.Errorf("expected DryRun false, got true")
	}
}

// TestParse_WorkingMemory tests WORKING_MEMORY SESSION query parsing.
func TestParse_WorkingMemory(t *testing.T) {
	input := `WORKING_MEMORY SESSION "sess-12345"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	wmQuery, ok := query.(*WorkingMemoryQuery)
	if !ok {
		t.Fatalf("expected WorkingMemoryQuery, got %T", query)
	}

	if wmQuery.SessionID != "sess-12345" {
		t.Errorf("expected session ID 'sess-12345', got %q", wmQuery.SessionID)
	}
}

// TestParse_ProvenanceSourcePredicate tests provenance.source = <name> in WHERE clause.
func TestParse_ProvenanceSourcePredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE provenance.source = llm`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*ProvenanceSourcePredicate)
	if !ok {
		t.Fatalf("expected ProvenanceSourcePredicate, got %T", activateQuery.Where)
	}

	if pred.Source != "llm" {
		t.Errorf("expected source 'llm', got %q", pred.Source)
	}
}

// TestParse_ProvenanceAgentPredicate tests provenance.agent = "<id>" in WHERE clause.
func TestParse_ProvenanceAgentPredicate(t *testing.T) {
	input := `ACTIVATE FROM v CONTEXT ["x"] WHERE provenance.agent = "agent-42"`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	pred, ok := activateQuery.Where.(*ProvenanceAgentPredicate)
	if !ok {
		t.Fatalf("expected ProvenanceAgentPredicate, got %T", activateQuery.Where)
	}

	if pred.Agent != "agent-42" {
		t.Errorf("expected agent 'agent-42', got %q", pred.Agent)
	}
}

// TestParse_BackwardCompatible_Activate tests that existing ACTIVATE queries still parse.
func TestParse_BackwardCompatible_Activate(t *testing.T) {
	input := `ACTIVATE FROM myvault CONTEXT ["memory", "learning"] WHERE state = active MAX_RESULTS 50 HOPS 3 MIN_RELEVANCE 0.7`
	query, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", query)
	}

	if activateQuery.Vault != "myvault" {
		t.Errorf("expected vault 'myvault', got %q", activateQuery.Vault)
	}

	if len(activateQuery.Context) != 2 {
		t.Errorf("expected 2 context terms, got %d", len(activateQuery.Context))
	}

	if activateQuery.MaxResults != 50 {
		t.Errorf("expected MAX_RESULTS 50, got %d", activateQuery.MaxResults)
	}

	if activateQuery.Hops != 3 {
		t.Errorf("expected HOPS 3, got %d", activateQuery.Hops)
	}

	if activateQuery.MinRelevance != 0.7 {
		t.Errorf("expected MIN_RELEVANCE 0.7, got %f", activateQuery.MinRelevance)
	}
}
