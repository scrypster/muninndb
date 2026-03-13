//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// allMCPTools is the canonical list of every MCP tool the server must expose.
// Adding a tool to the server without adding it here causes TestSmoke_RegistryParity to fail.
// Removing a tool from the server without removing it here also causes the test to fail.
var allMCPTools = []string{
	"muninn_remember",
	"muninn_remember_batch",
	"muninn_recall",
	"muninn_read",
	"muninn_forget",
	"muninn_link",
	"muninn_contradictions",
	"muninn_status",
	"muninn_evolve",
	"muninn_consolidate",
	"muninn_session",
	"muninn_decide",
	"muninn_restore",
	"muninn_traverse",
	"muninn_explain",
	"muninn_state",
	"muninn_list_deleted",
	"muninn_retry_enrich",
	"muninn_guide",
	"muninn_where_left_off",
	"muninn_find_by_entity",
	"muninn_entity_state",
	"muninn_entity_state_batch",
	"muninn_remember_tree",
	"muninn_recall_tree",
	"muninn_entity_clusters",
	"muninn_export_graph",
	"muninn_add_child",
	"muninn_similar_entities",
	"muninn_merge_entity",
	"muninn_replay_enrichment",
	"muninn_provenance",
	"muninn_entity_timeline",
	"muninn_feedback",
	"muninn_entity",
	"muninn_entities",
}

// adminLogin POSTs to the UI login endpoint (:8476) and returns the muninn_session cookie.
func adminLogin(t *testing.T) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "root", "password": "password"})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8476/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Use a client that does NOT follow redirects so we can capture the cookie directly.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("adminLogin: POST /api/auth/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("adminLogin: expected 200, got %d: %s", resp.StatusCode, b)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "muninn_session" {
			return c.Value
		}
	}
	t.Fatalf("adminLogin: no muninn_session cookie in response")
	return ""
}

// mcpToolsList calls the JSON-RPC tools/list method and returns the list of tool names.
func mcpToolsList(t *testing.T, token string) []string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8750/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcpToolsList: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mcpToolsList: HTTP %d: %s", resp.StatusCode, b)
	}
	var rpcResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	rawBody, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(rawBody, &rpcResp); err != nil {
		t.Fatalf("mcpToolsList: decode: %v\nbody: %s", err, rawBody)
	}
	if rpcResp.Error != nil {
		t.Fatalf("mcpToolsList: RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	names := make([]string, 0, len(rpcResp.Result.Tools))
	for _, tool := range rpcResp.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// mcpToolText sends a tools/call request and returns the raw text payload (not parsed as JSON).
// Used for tools like muninn_guide that return plain text instead of JSON.
func mcpToolText(t *testing.T, token, toolName string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8750/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcpToolText %s: %v", toolName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcpToolText %s: HTTP %d", toolName, resp.StatusCode)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &rpcResp); err != nil {
		t.Fatalf("mcpToolText %s: decode: %v\nbody: %s", toolName, err, rawBody)
	}
	if rpcResp.Error != nil {
		t.Fatalf("mcpToolText %s: RPC error %d: %s", toolName, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if len(rpcResp.Result.Content) == 0 {
		t.Fatalf("mcpToolText %s: empty result.content\nbody: %s", toolName, rawBody)
	}
	return rpcResp.Result.Content[0].Text
}

// mcpToolNoFail is like mcpTool but returns (result, error) instead of failing.
// Used for tools where an empty-but-valid response is acceptable.
func mcpToolNoFail(t *testing.T, token, toolName string, args map[string]any) (map[string]any, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8750/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rawBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode: %v, body: %s", err, rawBody)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if len(rpcResp.Result.Content) == 0 {
		return nil, fmt.Errorf("empty result.content")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &result); err != nil {
		return nil, fmt.Errorf("parse text payload: %v", err)
	}
	return result, nil
}

// TestSmoke_RegistryParity verifies the live server tool list matches allMCPTools exactly.
// It fails if any tool is on the server but NOT in our list (untested new tool),
// or in our list but NOT on the server (removed without updating the list).
func TestSmoke_RegistryParity(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	tok := readTokenFile()
	serverTools := mcpToolsList(t, tok)

	// Build lookup sets.
	inServer := make(map[string]bool, len(serverTools))
	for _, name := range serverTools {
		inServer[name] = true
	}
	inCanonical := make(map[string]bool, len(allMCPTools))
	for _, name := range allMCPTools {
		inCanonical[name] = true
	}

	// Check for tools on server but not in our canonical list (untested new tools).
	var missingFromCanonical []string
	for _, name := range serverTools {
		if !inCanonical[name] {
			missingFromCanonical = append(missingFromCanonical, name)
		}
	}
	if len(missingFromCanonical) > 0 {
		t.Errorf("server exposes tools NOT in allMCPTools (add them and write subtests):\n  %s",
			strings.Join(missingFromCanonical, "\n  "))
	}

	// Check for tools in our canonical list but not on server (removed/renamed).
	var missingFromServer []string
	for _, name := range allMCPTools {
		if !inServer[name] {
			missingFromServer = append(missingFromServer, name)
		}
	}
	if len(missingFromServer) > 0 {
		t.Errorf("allMCPTools lists tools NOT on server (remove or rename them):\n  %s",
			strings.Join(missingFromServer, "\n  "))
	}

	t.Logf("registry parity: server has %d tools, canonical list has %d tools", len(serverTools), len(allMCPTools))
}

// TestSmoke_AllMCPTools fires all 35 MCP tools against a live daemon.
// One daemon is shared for the entire test. Subtests are named by tool.
func TestSmoke_AllMCPTools(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	tok := readTokenFile()
	const vault = "default"

	// Seed two memories for tools that need IDs.
	// We include inline entities so the entity registry is populated for entity-pipeline tools.
	seedA := mcpTool(t, tok, "muninn_remember", map[string]any{
		"vault":   vault,
		"concept": "Alice the explorer",
		"content": "Alice explored the ancient ruins and discovered an EntityA artifact.",
		"entities": []map[string]any{
			{"name": "Alice", "type": "person"},
			{"name": "EntityA", "type": "concept"},
		},
	})
	idA, ok := seedA["id"].(string)
	if !ok || idA == "" {
		t.Fatalf("seed A: expected non-empty id, got: %v", seedA)
	}

	seedB := mcpTool(t, tok, "muninn_remember", map[string]any{
		"vault":   vault,
		"concept": "EntityB discovery",
		"content": "EntityB was found near the ruins where Alice had been exploring.",
		"entities": []map[string]any{
			{"name": "EntityB", "type": "concept"},
			{"name": "Alice", "type": "person"},
		},
	})
	idB, ok := seedB["id"].(string)
	if !ok || idB == "" {
		t.Fatalf("seed B: expected non-empty id, got: %v", seedB)
	}

	// Link A→B.
	mcpTool(t, tok, "muninn_link", map[string]any{
		"vault":     vault,
		"source_id": idA,
		"target_id": idB,
		"relation":  "relates_to",
		"weight":    0.8,
	})

	// ── Per-tool subtests ──────────────────────────────────────────────────────

	t.Run("muninn_remember", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "smoke test",
			"content": "smoke test remember",
		})
		if id, _ := result["id"].(string); id == "" {
			t.Errorf("expected id in result, got: %v", result)
		}
	})

	t.Run("muninn_remember_batch", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_remember_batch", map[string]any{
			"vault": vault,
			"memories": []map[string]any{
				{"concept": "batch item 1", "content": "first batch memory"},
				{"concept": "batch item 2", "content": "second batch memory"},
			},
		})
		// batch returns {"results": [...], "total": N}
		results, _ := result["results"].([]any)
		if len(results) == 0 {
			t.Errorf("expected results array in batch result, got: %v", result)
		}
	})

	t.Run("muninn_recall", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_recall", map[string]any{
			"vault":   vault,
			"context": []string{"explorer ruins Alice"},
			"limit":   5,
		})
		// memories may be empty if FTS not yet indexed; just verify no error key.
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_recall returned error field: %v", errVal)
		}
	})

	t.Run("muninn_read", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_read", map[string]any{
			"vault": vault,
			"id":    idA,
		})
		if content, _ := result["content"].(string); content == "" {
			t.Errorf("expected content in read result, got: %v", result)
		}
	})

	t.Run("muninn_forget", func(t *testing.T) {
		// Create a memory specifically to forget.
		r := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "to be forgotten",
			"content": "this memory will be forgotten in the subtest",
		})
		forgetID, _ := r["id"].(string)
		if forgetID == "" {
			t.Fatal("failed to create memory to forget")
		}
		result := mcpTool(t, tok, "muninn_forget", map[string]any{
			"vault": vault,
			"id":    forgetID,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_forget returned error field: %v", errVal)
		}
	})

	t.Run("muninn_link", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_link", map[string]any{
			"vault":     vault,
			"source_id": idA,
			"target_id": idB,
			"relation":  "relates_to",
			"weight":    0.9,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_link returned error field: %v", errVal)
		}
	})

	t.Run("muninn_contradictions", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_contradictions", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_contradictions returned error field: %v", errVal)
		}
	})

	t.Run("muninn_status", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_status", map[string]any{
			"vault": vault,
		})
		// status should return some metric field; vault key is always present.
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_status returned error field: %v", errVal)
		}
	})

	t.Run("muninn_evolve", func(t *testing.T) {
		// muninn_evolve requires id, new_content, and reason.
		result := mcpTool(t, tok, "muninn_evolve", map[string]any{
			"vault":       vault,
			"id":          idA,
			"new_content": "Alice explored the ancient ruins and discovered an EntityA artifact. Updated: she also found a map.",
			"reason":      "new information discovered",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_evolve returned error field: %v", errVal)
		}
	})

	t.Run("muninn_consolidate", func(t *testing.T) {
		// Create two memories to consolidate.
		rC1 := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "consolidate A",
			"content": "first memory to consolidate",
		})
		rC2 := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "consolidate B",
			"content": "second memory to consolidate",
		})
		idC1, _ := rC1["id"].(string)
		idC2, _ := rC2["id"].(string)
		if idC1 == "" || idC2 == "" {
			t.Fatal("failed to create memories for consolidation")
		}
		result := mcpTool(t, tok, "muninn_consolidate", map[string]any{
			"vault":          vault,
			"ids":            []string{idC1, idC2},
			"merged_content": "consolidated: first and second memories combined",
		})
		if newID, _ := result["id"].(string); newID == "" {
			t.Errorf("expected id in consolidate result, got: %v", result)
		}
	})

	t.Run("muninn_session", func(t *testing.T) {
		// muninn_session requires 'since' as an ISO 8601 timestamp.
		result := mcpTool(t, tok, "muninn_session", map[string]any{
			"vault": vault,
			"since": "2020-01-01T00:00:00Z",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_session returned error field: %v", errVal)
		}
	})

	t.Run("muninn_decide", func(t *testing.T) {
		// muninn_decide requires 'decision' and 'rationale'; options is 'alternatives'.
		result := mcpTool(t, tok, "muninn_decide", map[string]any{
			"vault":        vault,
			"decision":     "explore the eastern ruins now",
			"rationale":    "the weather is good and we have supplies",
			"alternatives": []string{"wait for backup", "postpone until next season"},
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_decide returned error field: %v", errVal)
		}
	})

	t.Run("muninn_restore", func(t *testing.T) {
		// Forget something then restore it.
		rRestore := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "to restore",
			"content": "this memory will be forgotten then restored",
		})
		restoreID, _ := rRestore["id"].(string)
		if restoreID == "" {
			t.Fatal("failed to create memory for restore test")
		}
		mcpTool(t, tok, "muninn_forget", map[string]any{
			"vault": vault,
			"id":    restoreID,
		})
		result := mcpTool(t, tok, "muninn_restore", map[string]any{
			"vault": vault,
			"id":    restoreID,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_restore returned error field: %v", errVal)
		}
	})

	t.Run("muninn_traverse", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_traverse", map[string]any{
			"vault":     vault,
			"start_id":  idA,
			"max_hops":  2,
			"max_nodes": 20,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_traverse returned error field: %v", errVal)
		}
		if _, hasNodes := result["nodes"]; !hasNodes {
			t.Errorf("muninn_traverse missing nodes field: %v", result)
		}
	})

	t.Run("muninn_explain", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_explain", map[string]any{
			"vault":     vault,
			"engram_id": idA,
			"query":     []string{"Alice explorer ruins"},
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_explain returned error field: %v", errVal)
		}
	})

	t.Run("muninn_state", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_state", map[string]any{
			"vault": vault,
			"id":    idB,
			"state": "active",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_state returned error field: %v", errVal)
		}
	})

	t.Run("muninn_list_deleted", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_list_deleted", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_list_deleted returned error field: %v", errVal)
		}
	})

	t.Run("muninn_retry_enrich", func(t *testing.T) {
		result, err := mcpToolNoFail(t, tok, "muninn_retry_enrich", map[string]any{
			"vault": vault,
			"id":    idA,
		})
		if err != nil {
			t.Logf("muninn_retry_enrich: %v (non-fatal — no enricher configured)", err)
			return
		}
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_retry_enrich returned error field: %v", errVal)
		}
	})

	t.Run("muninn_guide", func(t *testing.T) {
		// muninn_guide returns plain text (markdown), not JSON.
		text := mcpToolText(t, tok, "muninn_guide", map[string]any{
			"vault": vault,
		})
		// Guide should return non-empty text.
		if len(strings.TrimSpace(text)) == 0 {
			t.Errorf("muninn_guide returned empty text")
		}
	})

	t.Run("muninn_where_left_off", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_where_left_off", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_where_left_off returned error field: %v", errVal)
		}
	})

	t.Run("muninn_find_by_entity", func(t *testing.T) {
		// muninn_find_by_entity uses 'entity_name' not 'entity'.
		result := mcpTool(t, tok, "muninn_find_by_entity", map[string]any{
			"vault":       vault,
			"entity_name": "Alice",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_find_by_entity returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entity_state", func(t *testing.T) {
		// muninn_entity_state uses 'entity_name' and requires a 'state' value.
		result := mcpTool(t, tok, "muninn_entity_state", map[string]any{
			"vault":       vault,
			"entity_name": "Alice",
			"state":       "active",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entity_state returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entity_state_batch", func(t *testing.T) {
		// muninn_entity_state_batch accepts an operations array; Alice was seeded above.
		result := mcpTool(t, tok, "muninn_entity_state_batch", map[string]any{
			"vault": vault,
			"operations": []map[string]any{
				{"entity_name": "Alice", "state": "active"},
			},
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entity_state_batch returned error field: %v", errVal)
		}
	})

	t.Run("muninn_remember_tree", func(t *testing.T) {
		// muninn_remember_tree requires a 'root' object with concept/content inside it.
		result := mcpTool(t, tok, "muninn_remember_tree", map[string]any{
			"vault": vault,
			"root": map[string]any{
				"concept": "root concept",
				"content": "root memory for tree test",
			},
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_remember_tree returned error field: %v", errVal)
		}
	})

	t.Run("muninn_recall_tree", func(t *testing.T) {
		// muninn_recall_tree uses 'root_id' not 'id'.
		result := mcpTool(t, tok, "muninn_recall_tree", map[string]any{
			"vault":   vault,
			"root_id": idA,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_recall_tree returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entity_clusters", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_entity_clusters", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entity_clusters returned error field: %v", errVal)
		}
	})

	t.Run("muninn_export_graph", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_export_graph", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_export_graph returned error field: %v", errVal)
		}
	})

	t.Run("muninn_add_child", func(t *testing.T) {
		// muninn_add_child takes parent_id + child concept/content inline (not child_id).
		rParent := mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "parent node",
			"content": "parent memory for tree hierarchy test",
		})
		parentID, _ := rParent["id"].(string)
		if parentID == "" {
			t.Fatal("failed to create parent memory")
		}
		result := mcpTool(t, tok, "muninn_add_child", map[string]any{
			"vault":     vault,
			"parent_id": parentID,
			"concept":   "child node",
			"content":   "child memory for tree hierarchy test",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_add_child returned error field: %v", errVal)
		}
	})

	t.Run("muninn_similar_entities", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_similar_entities", map[string]any{
			"vault":  vault,
			"entity": "Alice",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_similar_entities returned error field: %v", errVal)
		}
	})

	t.Run("muninn_merge_entity", func(t *testing.T) {
		// muninn_merge_entity uses 'entity_a' and 'entity_b' not 'source'/'target'.
		// Seed memories with inline entities to populate the entity registry.
		mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":    vault,
			"concept":  "MergeEntityA",
			"content":  "MergeEntityA is a concept in the system.",
			"entities": []map[string]any{{"name": "MergeEntityA", "type": "concept"}},
		})
		mcpTool(t, tok, "muninn_remember", map[string]any{
			"vault":    vault,
			"concept":  "MergeEntityB",
			"content":  "MergeEntityB is related to MergeEntityA.",
			"entities": []map[string]any{{"name": "MergeEntityB", "type": "concept"}},
		})
		result := mcpTool(t, tok, "muninn_merge_entity", map[string]any{
			"vault":    vault,
			"entity_a": "MergeEntityA",
			"entity_b": "MergeEntityB",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_merge_entity returned error field: %v", errVal)
		}
	})

	t.Run("muninn_replay_enrichment", func(t *testing.T) {
		result, err := mcpToolNoFail(t, tok, "muninn_replay_enrichment", map[string]any{
			"vault": vault,
			"id":    idB,
		})
		if err != nil {
			t.Logf("muninn_replay_enrichment: %v (non-fatal — no enricher configured)", err)
			return
		}
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_replay_enrichment returned error field: %v", errVal)
		}
	})

	t.Run("muninn_provenance", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_provenance", map[string]any{
			"vault": vault,
			"id":    idA,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_provenance returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entity_timeline", func(t *testing.T) {
		// muninn_entity_timeline uses 'entity_name' not 'entity'.
		result := mcpTool(t, tok, "muninn_entity_timeline", map[string]any{
			"vault":       vault,
			"entity_name": "Alice",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entity_timeline returned error field: %v", errVal)
		}
	})

	t.Run("muninn_feedback", func(t *testing.T) {
		// muninn_feedback uses 'engram_id' not 'id', and 'useful' (bool) not 'signal'.
		result := mcpTool(t, tok, "muninn_feedback", map[string]any{
			"vault":     vault,
			"engram_id": idA,
			"useful":    true,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_feedback returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entity", func(t *testing.T) {
		// muninn_entity uses 'name' not 'entity'.
		result := mcpTool(t, tok, "muninn_entity", map[string]any{
			"vault": vault,
			"name":  "Alice",
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entity returned error field: %v", errVal)
		}
	})

	t.Run("muninn_entities", func(t *testing.T) {
		result := mcpTool(t, tok, "muninn_entities", map[string]any{
			"vault": vault,
		})
		if errVal, hasErr := result["error"]; hasErr {
			t.Errorf("muninn_entities returned error field: %v", errVal)
		}
	})
}

// doREST is a minimal HTTP helper for REST route smoke tests.
// It sends a request to the REST API at :8475 and returns the status code and body.
func doREST(t *testing.T, method, path string, body []byte, headers map[string]string) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:8475"+path, bodyReader)
	if err != nil {
		t.Fatalf("doREST %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("doREST %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// doRESTWithCookie sends a request to :8475 with a cookie.
func doRESTWithCookie(t *testing.T, method, path string, body []byte, cookieName, cookieValue string) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:8475"+path, bodyReader)
	if err != nil {
		t.Fatalf("doRESTWithCookie %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("doRESTWithCookie %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestSmoke_RESTRoutes hits all REST route groups against a live daemon.
//
// Auth design note: the REST vault routes require either (a) a vault-scoped API key
// (Bearer token) or (b) an admin session cookie (muninn_session). The MCP bearer
// token (from readTokenFile) is the MCP server's shared secret — it is NOT a vault
// API key. For vault-authenticated REST routes we use the admin session cookie bypass,
// which grants full write-mode access to any vault. This mirrors how the web UI works.
//
// The `default` vault is bootstrapped as "public" (no API key required for observe-mode
// access), so testing the auth boundary requires a non-public vault.
func TestSmoke_RESTRoutes(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// Get admin session cookie — used for both admin routes AND vault routes via bypass.
	adminCookie := adminLogin(t)

	// ── Public routes (no auth) ──────────────────────────────────────────────

	t.Run("GET /api/health", func(t *testing.T) {
		code, body := doREST(t, "GET", "/api/health", nil, nil)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/ready", func(t *testing.T) {
		code, body := doREST(t, "GET", "/api/ready", nil, nil)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/workers", func(t *testing.T) {
		code, body := doREST(t, "GET", "/api/workers", nil, nil)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/openapi.yaml", func(t *testing.T) {
		code, body := doREST(t, "GET", "/api/openapi.yaml", nil, nil)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
		if !strings.Contains(string(body), "openapi") {
			t.Errorf("openapi.yaml response missing 'openapi' keyword")
		}
	})

	// ── Authenticated vault routes (via admin session cookie bypass) ──────────

	t.Run("GET /api/stats", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/stats?vault=default", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/vaults", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/vaults", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("POST /api/engrams", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]any{
			"vault":   "default",
			"concept": "REST smoke test",
			"content": "testing POST /api/engrams from smoke suite",
		})
		code, body := doRESTWithCookie(t, "POST", "/api/engrams", payload, "muninn_session", adminCookie)
		if code != http.StatusCreated && code != http.StatusOK {
			t.Errorf("expected 200 or 201, got %d: %s", code, body)
		}
	})

	t.Run("POST /api/activate", func(t *testing.T) {
		payload, _ := json.Marshal(map[string]any{
			"vault":   "default",
			"context": []string{"smoke test"},
			"limit":   5,
		})
		code, body := doRESTWithCookie(t, "POST", "/api/activate", payload, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/guide", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/guide?vault=default", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	// ── Admin routes (session cookie auth) ───────────────────────────────────

	t.Run("GET /api/admin/keys", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/admin/keys", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/admin/plugins", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/admin/plugins", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/admin/mcp-info", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/admin/mcp-info", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	t.Run("GET /api/admin/embed/status", func(t *testing.T) {
		code, body := doRESTWithCookie(t, "GET", "/api/admin/embed/status", nil, "muninn_session", adminCookie)
		if code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", code, body)
		}
	})

	// ── Auth boundary checks ─────────────────────────────────────────────────

	// Admin routes must reject plain Bearer tokens (only session cookies work).
	t.Run("auth_boundary: admin route with Bearer token -> 401", func(t *testing.T) {
		fakeTok := "fake-vault-token-not-a-session-cookie"
		code, _ := doREST(t, "GET", "/api/admin/keys", nil, map[string]string{
			"Authorization": "Bearer " + fakeTok,
		})
		if code == http.StatusOK {
			t.Errorf("admin route should NOT be accessible with Bearer token, got %d", code)
		}
	})

	// Admin routes must reject requests with no auth at all.
	t.Run("auth_boundary: admin route with no auth -> 401", func(t *testing.T) {
		code, _ := doREST(t, "GET", "/api/admin/keys", nil, nil)
		if code == http.StatusOK {
			t.Errorf("admin route should NOT be accessible with no auth, got %d", code)
		}
	})

	// Vault routes must reject invalid Bearer tokens (when provided).
	t.Run("auth_boundary: vault route with invalid Bearer token -> 401", func(t *testing.T) {
		code, _ := doREST(t, "GET", "/api/stats?vault=default", nil, map[string]string{
			"Authorization": "Bearer invalid-token-12345",
		})
		if code == http.StatusOK {
			t.Errorf("vault route should NOT accept invalid Bearer token, got %d", code)
		}
	})
}

// TestRegression_VaultCreationE2E is the most important regression test.
// It must never be deleted. It exercises the full vault creation flow:
// admin login → create vault → create API key → hello with vault token → store memory → read it back.
// This test catches regressions in issue #19 (vault not registered until hello is called).
func TestRegression_VaultCreationE2E(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// Step 1: Login as admin to get session cookie.
	adminCookie := adminLogin(t)

	// Step 2: Create vault via PUT /api/admin/vaults/config.
	const testVault = "regression-e2e-vault"
	vaultConfigPayload, _ := json.Marshal(map[string]any{
		"vaults": []string{testVault},
	})
	code, body := doRESTWithCookie(t, "PUT", "/api/admin/vaults/config", vaultConfigPayload, "muninn_session", adminCookie)
	if code != http.StatusOK {
		t.Fatalf("step 2: create vault: expected 200, got %d: %s", code, body)
	}

	// Step 3: Create API key via POST /api/admin/keys.
	keyPayload, _ := json.Marshal(map[string]any{
		"vault": testVault,
		"label": "regression-test-key",
		"mode":  "full",
	})
	code, body = doRESTWithCookie(t, "POST", "/api/admin/keys", keyPayload, "muninn_session", adminCookie)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("step 3: create API key: expected 200 or 201, got %d: %s", code, body)
	}

	// Step 4: Extract token from the key creation response.
	var keyResp map[string]any
	if err := json.Unmarshal(body, &keyResp); err != nil {
		t.Fatalf("step 4: parse key response: %v\nbody: %s", err, body)
	}
	vaultToken, _ := keyResp["token"].(string)
	if vaultToken == "" {
		// Some responses nest the token inside a "key" object.
		if keyObj, ok := keyResp["key"].(map[string]any); ok {
			vaultToken, _ = keyObj["token"].(string)
		}
	}
	if vaultToken == "" {
		t.Fatalf("step 4: no token in key creation response: %v", keyResp)
	}

	// Step 5: Hello with vault token to register the vault (required for issue #19 fix).
	helloPayload, _ := json.Marshal(map[string]any{
		"version": "1",
		"vault":   testVault,
		"token":   vaultToken,
	})
	code, body = doREST(t, "POST", "/api/hello", helloPayload, map[string]string{
		"Authorization": "Bearer " + vaultToken,
	})
	if code != http.StatusOK {
		t.Fatalf("step 5: hello with vault token: expected 200, got %d: %s", code, body)
	}

	// Step 6: Store a memory via MCP.
	// The MCP endpoint uses the server-level shared token (from readTokenFile), not
	// vault API keys. The vault is specified in the tool arguments.
	mcpTok := readTokenFile()
	writeResult := mcpTool(t, mcpTok, "muninn_remember", map[string]any{
		"vault":   testVault,
		"concept": "regression e2e memory",
		"content": "vault creation e2e regression test — issue #19",
	})
	memID, _ := writeResult["id"].(string)
	if memID == "" {
		t.Fatalf("step 6: muninn_remember: expected non-empty id, got: %v", writeResult)
	}

	// Step 7: Read it back via muninn_read and assert content is present.
	readResult := mcpTool(t, mcpTok, "muninn_read", map[string]any{
		"vault": testVault,
		"id":    memID,
	})
	content, _ := readResult["content"].(string)
	if content == "" {
		t.Fatalf("step 7: muninn_read: content missing or empty: %v", readResult)
	}
	const wantSubstr = "vault creation e2e regression test"
	if !strings.Contains(content, wantSubstr) {
		t.Errorf("step 7: content %q does not contain %q", content, wantSubstr)
	}
	t.Logf("TestRegression_VaultCreationE2E passed: vault=%s, memID=%s", testVault, memID)
}
