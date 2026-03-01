package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func (s *MCPServer) handleRemember(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	content, ok := args["content"].(string)
	if !ok || content == "" {
		sendError(w, id, -32602, "invalid params: 'content' is required")
		return
	}
	req := &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
	}
	if c, ok := args["concept"].(string); ok {
		req.Concept = c
	}
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if s, ok := t.(string); ok && len(s) > 0 && len(s) <= 128 {
				req.Tags = append(req.Tags, s)
			}
		}
		if len(req.Tags) > 50 {
			req.Tags = req.Tags[:50]
		}
	}
	if conf, ok := args["confidence"].(float64); ok {
		if conf < 0 {
			conf = 0
		} else if conf > 1 {
			conf = 1
		}
		req.Confidence = float32(conf)
	}
	if caStr, ok := args["created_at"].(string); ok && caStr != "" {
		t, err := time.Parse(time.RFC3339, caStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'created_at': must be ISO 8601 (e.g. 2026-01-15T09:00:00Z)")
			return
		}
		req.CreatedAt = &t
	}
	applyTypeArgs(args, req)
	applyEnrichmentArgs(args, req)

	resp, err := s.engine.Write(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	result := WriteResult{ID: resp.ID}
	if len(content) > 500 {
		result.Hint = "Tip: memories work best when each one captures a single concept. For future writes, consider using muninn_remember_batch to store multiple focused memories at once."
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleRememberBatch(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	memoriesAny, ok := args["memories"].([]any)
	if !ok || len(memoriesAny) == 0 {
		sendError(w, id, -32602, "invalid params: 'memories' is required and must be a non-empty array")
		return
	}
	if len(memoriesAny) > 50 {
		sendError(w, id, -32602, "invalid params: 'memories' exceeds maximum of 50")
		return
	}

	reqs := make([]*mbp.WriteRequest, 0, len(memoriesAny))
	for i, mAny := range memoriesAny {
		m, ok := mAny.(map[string]any)
		if !ok {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d] must be an object", i))
			return
		}
		content, ok := m["content"].(string)
		if !ok || content == "" {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d].content is required", i))
			return
		}
		req := &mbp.WriteRequest{
			Vault:   vault,
			Content: content,
		}
		if c, ok := m["concept"].(string); ok {
			req.Concept = c
		}
		if tags, ok := m["tags"].([]any); ok {
			for _, t := range tags {
				if s, ok := t.(string); ok && len(s) > 0 && len(s) <= 128 {
					req.Tags = append(req.Tags, s)
				}
			}
			if len(req.Tags) > 50 {
				req.Tags = req.Tags[:50]
			}
		}
		if conf, ok := m["confidence"].(float64); ok {
			if conf < 0 {
				conf = 0
			} else if conf > 1 {
				conf = 1
			}
			req.Confidence = float32(conf)
		}
		if caStr, ok := m["created_at"].(string); ok && caStr != "" {
			t, err := time.Parse(time.RFC3339, caStr)
			if err != nil {
				sendError(w, id, -32602, fmt.Sprintf("invalid 'created_at' in memories[%d]: must be ISO 8601", i))
				return
			}
			req.CreatedAt = &t
		}
		applyTypeArgs(m, req)
		applyEnrichmentArgs(m, req)
		reqs = append(reqs, req)
	}

	responses, errs := s.engine.WriteBatch(ctx, reqs)

	type batchItemResult struct {
		Index  int    `json:"index"`
		ID     string `json:"id,omitempty"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]batchItemResult, len(reqs))
	for i := range reqs {
		if errs[i] != nil {
			results[i] = batchItemResult{Index: i, Status: "error", Error: errs[i].Error()}
		} else {
			results[i] = batchItemResult{Index: i, ID: responses[i].ID, Status: "ok"}
		}
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"results": results,
		"total":   len(results),
	})))
}

func (s *MCPServer) handleRecall(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	ctxArr, ok := args["context"].([]any)
	if !ok || len(ctxArr) == 0 {
		sendError(w, id, -32602, "invalid params: 'context' is required")
		return
	}
	var contexts []string
	for _, c := range ctxArr {
		if str, ok := c.(string); ok {
			contexts = append(contexts, str)
		}
	}

	threshold := float32(0.5)
	if t, ok := args["threshold"].(float64); ok {
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		threshold = float32(t)
	}
	limit := 10
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}

	profile, _ := args["profile"].(string)

	// Mode shortcuts: resolve preset if provided.
	var modePreset RecallMode
	if modeStr, ok := args["mode"].(string); ok && modeStr != "" {
		preset, modeErr := lookupMode(modeStr)
		if modeErr != nil {
			sendError(w, id, -32602, modeErr.Error())
			return
		}
		modePreset = preset
	}

	req := &mbp.ActivateRequest{
		Vault:      vault,
		Context:    contexts,
		Threshold:  threshold,
		MaxResults: limit,
		Profile:    profile,
	}

	// Apply non-zero mode preset fields.
	// Explicit caller threshold/limit args always win (already parsed above).
	if modePreset.Threshold > 0 {
		if _, callerSet := args["threshold"]; !callerSet {
			req.Threshold = modePreset.Threshold
		}
	}
	if modePreset.MaxHops > 0 {
		req.MaxHops = modePreset.MaxHops
	}

	// Apply mode preset scoring weights to the request.
	if modePreset.SemanticSimilarity > 0 || modePreset.FullTextRelevance > 0 || modePreset.Recency > 0 || modePreset.DisableACTR {
		if req.Weights == nil {
			req.Weights = &mbp.Weights{}
		}
		if modePreset.SemanticSimilarity > 0 {
			req.Weights.SemanticSimilarity = modePreset.SemanticSimilarity
		}
		if modePreset.FullTextRelevance > 0 {
			req.Weights.FullTextRelevance = modePreset.FullTextRelevance
		}
		if modePreset.Recency > 0 {
			req.Weights.Recency = modePreset.Recency
		}
		if modePreset.DisableACTR {
			req.Weights.DisableACTR = true
		}
	}

	// Temporal filters: since / before
	if sinceStr, ok := args["since"].(string); ok && sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'since': must be ISO 8601 (e.g. 2026-01-15T00:00:00Z)")
			return
		}
		req.Filters = append(req.Filters, mbp.Filter{Field: "created_after", Op: ">=", Value: t})
	}
	if beforeStr, ok := args["before"].(string); ok && beforeStr != "" {
		t, err := time.Parse(time.RFC3339, beforeStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'before': must be ISO 8601 (e.g. 2026-01-20T00:00:00Z)")
			return
		}
		req.Filters = append(req.Filters, mbp.Filter{Field: "created_before", Op: "<", Value: t})
	}

	resp, err := s.engine.Activate(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	var memories []Memory
	for i := range resp.Activations {
		memories = append(memories, activationToMemory(&resp.Activations[i]))
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"memories": memories,
		"total":    resp.TotalFound,
	})))
}

func (s *MCPServer) handleRead(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	resp, err := s.engine.Read(ctx, &mbp.ReadRequest{ID: engramID, Vault: vault})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(readResponseToMemory(resp))))
}

func (s *MCPServer) handleForget(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	_, err := s.engine.Forget(ctx, &mbp.ForgetRequest{ID: engramID, Hard: false, Vault: vault})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(`{"ok":true}`))
}

func (s *MCPServer) handleLink(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	srcID, ok1 := args["source_id"].(string)
	dstID, ok2 := args["target_id"].(string)
	rel, ok3 := args["relation"].(string)
	if !ok1 || !ok2 || !ok3 {
		sendError(w, id, -32602, "invalid params: 'source_id', 'target_id', 'relation' are required")
		return
	}
	weight := float32(0.8)
	if wf, ok := args["weight"].(float64); ok {
		if wf < 0 {
			wf = 0
		} else if wf > 1 {
			wf = 1
		}
		weight = float32(wf)
	}
	_, err := s.engine.Link(ctx, &mbp.LinkRequest{
		SourceID: srcID,
		TargetID: dstID,
		RelType:  relTypeFromString(rel),
		Weight:   weight,
		Vault:    vault,
	})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(`{"ok":true}`))
}

func (s *MCPServer) handleContradictions(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	pairs, err := s.engine.GetContradictions(ctx, vault)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{"contradictions": pairs})))
}

func (s *MCPServer) handleStatus(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	resp, err := s.engine.Stat(ctx, &mbp.StatRequest{Vault: vault})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	status := VaultStatus{
		Vault:         vault,
		TotalMemories: resp.EngramCount,
		Health:        "good",
	}
	sendResult(w, id, textContent(mustJSON(status)))
}

func (s *MCPServer) handleEvolve(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok1 := args["id"].(string)
	newContent, ok2 := args["new_content"].(string)
	reason, ok3 := args["reason"].(string)
	if !ok1 || !ok2 || !ok3 || engramID == "" || newContent == "" || reason == "" {
		sendError(w, id, -32602, "invalid params: 'id', 'new_content', 'reason' are required")
		return
	}
	result, err := s.engine.Evolve(ctx, vault, engramID, newContent, reason)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleConsolidate(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	idsAny, ok := args["ids"].([]any)
	if !ok || len(idsAny) == 0 {
		sendError(w, id, -32602, "invalid params: 'ids' is required")
		return
	}
	var ids []string
	for _, v := range idsAny {
		if str, ok := v.(string); ok {
			ids = append(ids, str)
		}
	}
	if len(ids) < 2 {
		sendError(w, id, -32602, "invalid params: 'ids' must contain at least 2 valid engram IDs")
		return
	}
	if len(ids) > 50 {
		sendError(w, id, -32602, "invalid params: 'ids' exceeds maximum of 50")
		return
	}
	merged, ok := args["merged_content"].(string)
	if !ok || merged == "" {
		sendError(w, id, -32602, "invalid params: 'merged_content' is required")
		return
	}
	result, err := s.engine.Consolidate(ctx, vault, ids, merged)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleSession(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	sinceStr, ok := args["since"].(string)
	if !ok || sinceStr == "" {
		sendError(w, id, -32602, "invalid params: 'since' is required (ISO 8601)")
		return
	}
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		sendError(w, id, -32602, "invalid params: 'since' must be ISO 8601 (e.g. 2024-01-01T00:00:00Z)")
		return
	}
	result, err := s.engine.Session(ctx, vault, since)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleDecide(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	decision, ok1 := args["decision"].(string)
	rationale, ok2 := args["rationale"].(string)
	if !ok1 || !ok2 || decision == "" || rationale == "" {
		sendError(w, id, -32602, "invalid params: 'decision' and 'rationale' are required")
		return
	}
	var alternatives []string
	if altAny, ok := args["alternatives"].([]any); ok {
		for _, a := range altAny {
			if str, ok := a.(string); ok {
				alternatives = append(alternatives, str)
			}
		}
	}
	var evidenceIDs []string
	if evAny, ok := args["evidence_ids"].([]any); ok {
		for _, e := range evAny {
			if str, ok := e.(string); ok {
				evidenceIDs = append(evidenceIDs, str)
			}
		}
	}
	result, err := s.engine.Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

// Epic 18: handlers for tools 12-17

func (s *MCPServer) handleRestore(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	result, err := s.engine.Restore(ctx, vault, engramID)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":       result.ID,
		"concept":  result.Concept,
		"restored": true,
		"state":    result.State,
	})))
}

func (s *MCPServer) handleTraverse(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	startID, ok := args["start_id"].(string)
	if !ok || startID == "" {
		sendError(w, id, -32602, "invalid params: 'start_id' is required")
		return
	}
	maxHops := 2
	if v, ok := args["max_hops"].(float64); ok {
		maxHops = int(v)
	}
	if maxHops > 5 {
		maxHops = 5
	}
	maxNodes := 20
	if v, ok := args["max_nodes"].(float64); ok {
		maxNodes = int(v)
	}
	if maxNodes > 100 {
		maxNodes = 100
	}
	var relTypes []string
	if arr, ok := args["rel_types"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				relTypes = append(relTypes, s)
			}
		}
	}
	req := &TraverseRequest{
		StartID:  startID,
		MaxHops:  maxHops,
		MaxNodes: maxNodes,
		RelTypes: relTypes,
	}
	result, err := s.engine.Traverse(ctx, vault, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleExplain(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["engram_id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'engram_id' is required")
		return
	}
	var query []string
	if arr, ok := args["query"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				query = append(query, s)
			}
		}
	}
	if len(query) == 0 {
		sendError(w, id, -32602, "invalid params: 'query' is required and must be a non-empty array of strings")
		return
	}
	result, err := s.engine.Explain(ctx, vault, &ExplainRequest{
		EngramID: engramID,
		Query:    query,
	})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

var validLifecycleStates = map[string]bool{
	"planning":  true,
	"active":    true,
	"paused":    true,
	"blocked":   true,
	"completed": true,
	"cancelled": true,
	"archived":  true,
}

func (s *MCPServer) handleState(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	state, ok := args["state"].(string)
	if !ok || state == "" {
		sendError(w, id, -32602, "invalid params: 'state' is required")
		return
	}
	if !validLifecycleStates[state] {
		sendError(w, id, -32602, "invalid params: 'state' must be one of: planning, active, paused, blocked, completed, cancelled, archived")
		return
	}
	reason, _ := args["reason"].(string)
	if err := s.engine.UpdateState(ctx, vault, engramID, state, reason); err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":      engramID,
		"state":   state,
		"updated": true,
	})))
}

func (s *MCPServer) handleListDeleted(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	limit := 20
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if limit > 100 {
		limit = 100
	}
	deleted, err := s.engine.ListDeleted(ctx, vault, limit)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if deleted == nil {
		deleted = []DeletedEngram{}
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"deleted": deleted,
		"count":   len(deleted),
	})))
}

func (s *MCPServer) handleRetryEnrich(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	result, err := s.engine.RetryEnrich(ctx, vault, engramID)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleGuide(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	plasticity, err := s.engine.GetVaultPlasticity(ctx, vault)
	if err != nil {
		// Fall back to defaults if plasticity is unavailable.
		defaults := auth.ResolvePlasticity(nil)
		plasticity = &defaults
	}

	statResp, err := s.engine.Stat(ctx, &mbp.StatRequest{Vault: vault})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	stats := engineStats{
		EngramCount: statResp.EngramCount,
		VaultCount:  statResp.VaultCount,
	}
	guide := generateGuide(vault, *plasticity, stats)
	sendResult(w, id, textContent(guide))
}

func (s *MCPServer) handleRememberTree(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	rootRaw, ok := args["root"]
	if !ok {
		sendError(w, id, -32602, "invalid params: 'root' is required")
		return
	}
	rootBytes, err := json.Marshal(rootRaw)
	if err != nil {
		sendError(w, id, -32602, "invalid params: cannot marshal root")
		return
	}
	var rootInput TreeNodeInput
	if err := json.Unmarshal(rootBytes, &rootInput); err != nil {
		sendError(w, id, -32602, "invalid params: root must match TreeNodeInput schema")
		return
	}
	req := &RememberTreeRequest{Vault: vault, Root: rootInput}
	result, err := s.engine.RememberTree(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleRecallTree(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	rootID, ok := args["root_id"].(string)
	if !ok || rootID == "" {
		sendError(w, id, -32602, "invalid params: 'root_id' is required")
		return
	}
	maxDepth := 10
	if d, ok := args["max_depth"].(float64); ok {
		maxDepth = int(d)
	}
	limit := 0
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	includeCompleted := true
	if ic, ok := args["include_completed"].(bool); ok {
		includeCompleted = ic
	}
	result, err := s.engine.RecallTree(ctx, vault, rootID, maxDepth, limit, includeCompleted)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleAddChild(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	parentID, ok := args["parent_id"].(string)
	if !ok || parentID == "" {
		sendError(w, id, -32602, "invalid params: 'parent_id' is required")
		return
	}
	concept, ok := args["concept"].(string)
	if !ok || concept == "" {
		sendError(w, id, -32602, "invalid params: 'concept' is required")
		return
	}
	content, _ := args["content"].(string)
	child := &AddChildRequest{Concept: concept, Content: content}
	if t, ok := args["type"].(string); ok {
		child.Type = t
	}
	if tags, ok := args["tags"].([]any); ok {
		for _, t := range tags {
			if str, ok := t.(string); ok {
				child.Tags = append(child.Tags, str)
			}
		}
	}
	if ord, ok := args["ordinal"].(float64); ok {
		o := int32(ord)
		child.Ordinal = &o
	}
	result, err := s.engine.AddChild(ctx, vault, parentID, child)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

// applyTypeArgs parses the "type" and "type_label" arguments from an MCP call
// and sets MemoryType + TypeLabel on the WriteRequest accordingly.
func applyTypeArgs(args map[string]any, req *mbp.WriteRequest) {
	typeStr, _ := args["type"].(string)
	explicitLabel, _ := args["type_label"].(string)

	if typeStr != "" {
		if mt, ok := storage.ParseMemoryType(typeStr); ok {
			req.MemoryType = uint8(mt)
			if explicitLabel == "" {
				req.TypeLabel = typeStr
			}
		} else {
			// Not a known enum name — store as free-form label, default to Fact.
			req.MemoryType = uint8(storage.TypeFact)
			if explicitLabel == "" {
				req.TypeLabel = typeStr
			}
		}
	}
	if explicitLabel != "" {
		req.TypeLabel = explicitLabel
	}
}

// applyEnrichmentArgs parses optional inline enrichment fields (summary, entities,
// relationships) from MCP tool call arguments onto the WriteRequest.
func applyEnrichmentArgs(args map[string]any, req *mbp.WriteRequest) {
	if summary, ok := args["summary"].(string); ok && summary != "" {
		req.Summary = summary
	}
	if entitiesAny, ok := args["entities"].([]any); ok {
		for _, eAny := range entitiesAny {
			eMap, ok := eAny.(map[string]any)
			if !ok {
				continue
			}
			name, _ := eMap["name"].(string)
			typ, _ := eMap["type"].(string)
			if name == "" || typ == "" {
				continue
			}
			req.Entities = append(req.Entities, mbp.InlineEntity{Name: name, Type: typ})
		}
	}
	if relsAny, ok := args["relationships"].([]any); ok {
		for _, rAny := range relsAny {
			rMap, ok := rAny.(map[string]any)
			if !ok {
				continue
			}
			targetID, _ := rMap["target_id"].(string)
			relation, _ := rMap["relation"].(string)
			if targetID == "" || relation == "" {
				continue
			}
			weight := float32(0.9)
			if w, ok := rMap["weight"].(float64); ok {
				if w < 0 {
					w = 0
				} else if w > 1 {
					w = 1
				}
				weight = float32(w)
			}
			req.Relationships = append(req.Relationships, mbp.InlineRelationship{
				TargetID: targetID,
				Relation: relation,
				Weight:   weight,
			})
		}
	}
}

var relTypeMap = map[string]storage.RelType{
	"supports":           storage.RelSupports,
	"contradicts":        storage.RelContradicts,
	"depends_on":         storage.RelDependsOn,
	"supersedes":         storage.RelSupersedes,
	"relates_to":         storage.RelRelatesTo,
	"is_part_of":         storage.RelIsPartOf,
	"causes":             storage.RelCauses,
	"preceded_by":        storage.RelPrecededBy,
	"followed_by":        storage.RelFollowedBy,
	"created_by_person":  storage.RelCreatedByPerson,
	"belongs_to_project": storage.RelBelongsToProject,
	"references":         storage.RelReferences,
	"implements":         storage.RelImplements,
	"blocks":             storage.RelBlocks,
	"resolves":           storage.RelResolves,
	"refines":            storage.RelRefines,
}

// relTypeFromString converts a relation string to a uint16 RelType value.
// Maps to the storage.RelType constants so round-tripping is consistent.
// Unknown or empty strings default to storage.RelRelatesTo.
func relTypeFromString(rel string) uint16 {
	if v, ok := relTypeMap[rel]; ok {
		return uint16(v)
	}
	return uint16(storage.RelRelatesTo) // default
}
