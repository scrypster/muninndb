package muninn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func mockHandler(status int, response any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(response)
	}
}

func TestWrite(t *testing.T) {
	srv := httptest.NewServer(mockHandler(201, WriteResponse{ID: "test-123", CreatedAt: 1700000000}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	id, err := client.Write(context.Background(), "default", "concept", "content", []string{"tag1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "test-123" {
		t.Errorf("expected id test-123, got %s", id)
	}
}

func TestRead(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, Engram{
		ID: "test-123", Concept: "test", Content: "test content", Confidence: 0.9,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	engram, err := client.Read(context.Background(), "test-123", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if engram.Concept != "test" {
		t.Errorf("expected concept 'test', got '%s'", engram.Concept)
	}
}

func TestActivate(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ActivateResponse{
		QueryID: "q1", TotalFound: 1,
		Activations: []ActivationItem{{ID: "a1", Concept: "match", Score: 0.85}},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Activate(context.Background(), "default", []string{"query"}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.TotalFound != 1 {
		t.Errorf("expected 1 result, got %d", resp.TotalFound)
	}
}

func TestLink(t *testing.T) {
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	err := client.Link(context.Background(), "default", "src", "tgt", 5, 1.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedPath != "/api/link" {
		t.Errorf("expected path /api/link, got %s", receivedPath)
	}
}

func TestForget(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, map[string]bool{"ok": true}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	err := client.Forget(context.Background(), "test-123", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, map[string]any{"status": "ok"}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	ok, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected healthy")
	}
}

func TestEvolve(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, EvolveResponse{ID: "evolved-1"}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Evolve(context.Background(), "default", "old-id", "new content", "reason")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "evolved-1" {
		t.Errorf("expected evolved-1, got %s", resp.ID)
	}
}

func TestConsolidate(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ConsolidateResponse{
		ID: "merged-1", Archived: []string{"a", "b"},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Consolidate(context.Background(), "default", []string{"a", "b"}, "merged content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "merged-1" {
		t.Errorf("expected merged-1, got %s", resp.ID)
	}
	if len(resp.Archived) != 2 {
		t.Errorf("expected 2 archived, got %d", len(resp.Archived))
	}
}

func TestDecide(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, DecideResponse{ID: "decision-1"}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Decide(context.Background(), "default", "use X", "because Y", []string{"alt"}, []string{"ev1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ID != "decision-1" {
		t.Errorf("expected decision-1, got %s", resp.ID)
	}
}

func TestRestore(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, RestoreResponse{ID: "r1", Concept: "restored", Restored: true, State: "active"}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Restore(context.Background(), "r1", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Restored {
		t.Error("expected restored=true")
	}
}

func TestTraverse(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, TraverseResponse{
		Nodes:          []TraversalNode{{ID: "n1", Concept: "start", HopDist: 0}},
		Edges:          []TraversalEdge{},
		TotalReachable: 1,
		QueryMs:        2.5,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Traverse(context.Background(), "default", "n1", 2, 20, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Errorf("expected 1 node, got %d", len(resp.Nodes))
	}
}

func TestExplain(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ExplainResponse{
		EngramID: "e1", Concept: "test", FinalScore: 0.85, WouldReturn: true, Threshold: 0.1,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Explain(context.Background(), "default", "e1", []string{"query"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.FinalScore != 0.85 {
		t.Errorf("expected score 0.85, got %f", resp.FinalScore)
	}
}

func TestSetState(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, SetStateResponse{ID: "s1", State: "active", Updated: true}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.SetState(context.Background(), "default", "s1", "active", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Updated {
		t.Error("expected updated=true")
	}
}

func TestListDeleted(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ListDeletedResponse{
		Deleted: []DeletedEngram{{ID: "d1", Concept: "deleted"}}, Count: 1,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.ListDeleted(context.Background(), "default", 20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Count != 1 {
		t.Errorf("expected count 1, got %d", resp.Count)
	}
}

func TestRetryEnrich(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, RetryEnrichResponse{
		EngramID: "e1", PluginsQueued: []string{"embed"},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.RetryEnrich(context.Background(), "e1", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.EngramID != "e1" {
		t.Errorf("expected e1, got %s", resp.EngramID)
	}
}

func TestContradictions(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ContradictionsResponse{
		Contradictions: []ContradictionItem{{IDa: "a1", IDb: "b1"}},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Contradictions(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Contradictions) != 1 {
		t.Errorf("expected 1 contradiction, got %d", len(resp.Contradictions))
	}
}

func TestGuide(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, GuideResponse{Guide: "test guide text"}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	guide, err := client.Guide(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if guide != "test guide text" {
		t.Errorf("expected 'test guide text', got '%s'", guide)
	}
}

func TestListEngrams(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, ListEngramsResponse{
		Engrams: []EngramItem{{ID: "e1", Concept: "test"}}, Total: 1, Limit: 20, Offset: 0,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.ListEngrams(context.Background(), "default", 20, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
}

func TestGetLinks(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, []AssociationItem{
		{TargetID: "t1", RelType: 5, Weight: 0.8},
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	links, err := client.GetLinks(context.Background(), "e1", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 {
		t.Errorf("expected 1 link, got %d", len(links))
	}
}

func TestListVaults(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, struct {
		Vaults []string `json:"vaults"`
	}{Vaults: []string{"default", "work"}}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	vaults, err := client.ListVaults(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vaults) != 2 {
		t.Errorf("expected 2 vaults, got %d", len(vaults))
	}
}

func TestSession(t *testing.T) {
	srv := httptest.NewServer(mockHandler(200, SessionResponse{
		Entries: []SessionEntry{{ID: "s1", Concept: "test"}}, Total: 1,
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Session(context.Background(), "default", "2020-01-01T00:00:00Z", 50, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total 1, got %d", resp.Total)
	}
}

func TestRequest_ServerError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	client := NewClientWithOptions(srv.URL, "test-token", 5*time.Second, 2, 10*time.Millisecond)
	_, err := client.Health(context.Background())
	if err == nil {
		t.Error("expected error on 500")
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (1 initial + 2 retries), got %d", attempts)
	}
}

func TestWriteWithOptions_ZeroConfidence_NotOverwritten(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(WriteResponse{ID: "x", CreatedAt: 1700000000})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	_, err := client.WriteWithOptions(context.Background(), WriteRequest{
		Vault:   "default",
		Concept: "test",
		Content: "body",
		// Confidence and Stability explicitly zero — should NOT be defaulted to 0.9/0.5
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// confidence=0 is omitempty so it won't appear in the JSON — that's fine.
	// The key assertion is that WriteWithOptions doesn't silently overwrite with 0.9.
	if v, ok := receivedBody["confidence"]; ok && v == 0.9 {
		t.Error("WriteWithOptions must not default confidence=0 to 0.9; caller controls the value")
	}
}

func TestTraverse_FollowEntities_Sent(t *testing.T) {
	var receivedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(TraverseResponse{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	_, err := client.Traverse(context.Background(), "default", "start-id", 2, 20, nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := receivedBody["follow_entities"]; !ok || v != true {
		t.Errorf("expected follow_entities=true in request body, got: %v", receivedBody)
	}
}

func TestStats_WithVault(t *testing.T) {
	var receivedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(StatsResponse{EngramCount: 42})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-token")
	resp, err := client.Stats(context.Background(), "myvault")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.EngramCount != 42 {
		t.Errorf("expected 42 engrams, got %d", resp.EngramCount)
	}
	if receivedQuery != "vault=myvault" {
		t.Errorf("expected vault=myvault in query, got: %s", receivedQuery)
	}
}

func TestAssociationItem_JSONTags(t *testing.T) {
	// Verify that AssociationItem deserializes from server wire format (snake_case).
	data := `{"target_id":"t1","rel_type":5,"weight":0.8,"co_activation_count":3}`
	var item AssociationItem
	if err := json.Unmarshal([]byte(data), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.TargetID != "t1" {
		t.Errorf("TargetID: want t1, got %q", item.TargetID)
	}
	if item.RelType != 5 {
		t.Errorf("RelType: want 5, got %d", item.RelType)
	}
	if item.CoActivationCount != 3 {
		t.Errorf("CoActivationCount: want 3, got %d", item.CoActivationCount)
	}
}

func TestEngramItem_JSONTags(t *testing.T) {
	// Verify that EngramItem deserializes with created_at (snake_case) from server.
	data := `{"id":"e1","concept":"c","content":"x","confidence":0.7,"vault":"default","created_at":1700000000}`
	var item EngramItem
	if err := json.Unmarshal([]byte(data), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.CreatedAt != 1700000000 {
		t.Errorf("CreatedAt: want 1700000000, got %d", item.CreatedAt)
	}
}

func TestRequest_AuthHeader(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "my-secret-token")
	client.Health(context.Background())
	if receivedAuth != "Bearer my-secret-token" {
		t.Errorf("expected 'Bearer my-secret-token', got '%s'", receivedAuth)
	}
}
