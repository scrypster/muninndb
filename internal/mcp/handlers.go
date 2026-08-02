package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"golang.org/x/text/unicode/norm"
)

// annotationStaleDays is the threshold for marking a recalled memory as stale.
// Memories not accessed in more than this many days are flagged stale=true.
const annotationStaleDays = 30.0

// parseValidityArgs parses the optional valid_from / valid_until args (RFC3339)
// into a WriteRequest. Returns a non-empty error message on a malformed value.
// Shared by muninn_remember and muninn_remember_batch.
func parseValidityArgs(args map[string]any, req *mbp.WriteRequest) string {
	if vfStr, ok := args["valid_from"].(string); ok && vfStr != "" {
		t, err := time.Parse(time.RFC3339, vfStr)
		if err != nil {
			return "invalid 'valid_from': must be ISO 8601 (e.g. 2024-01-15T00:00:00Z)"
		}
		req.ValidFrom = &t
	}
	if vuStr, ok := args["valid_until"].(string); ok && vuStr != "" {
		t, err := time.Parse(time.RFC3339, vuStr)
		if err != nil {
			return "invalid 'valid_until': must be ISO 8601 (e.g. 2025-06-30T00:00:00Z)"
		}
		req.ValidUntil = &t
	}
	return ""
}

// parseImportanceArg extracts the optional "importance" arg as a *float32.
// Returns nil when absent (unset — the use-time type-table default applies).
// Clamping/quantization (explicit 0 → 0.01) is the engine's job
// (importanceFromRequest); this only converts presence. A non-number value is
// rejected by the caller via the ok flag.
func parseImportanceArg(args map[string]any) (*float32, bool) {
	raw, present := args["importance"]
	if !present {
		return nil, true
	}
	f, ok := raw.(float64)
	if !ok {
		return nil, false
	}
	v := float32(f)
	return &v, true
}

// parseEmbedding extracts and validates an optional "embedding" field from args.
// Returns (nil, "") when the field is absent. Returns (nil, errMsg) on validation
// failure. The caller is responsible for the vault dimension check when needed.
func parseEmbeddingArg(args map[string]any) ([]float32, string) {
	embAny, ok := args["embedding"].([]any)
	if !ok || len(embAny) == 0 {
		return nil, ""
	}
	if len(embAny) > 4096 {
		return nil, "invalid params: 'embedding' exceeds maximum length of 4096"
	}
	embedding := make([]float32, len(embAny))
	for i, v := range embAny {
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Sprintf("invalid params: embedding[%d] must be a number", i)
		}
		embedding[i] = float32(f)
	}
	return embedding, ""
}

func (s *MCPServer) handleRemember(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	opID, _ := args["op_id"].(string)
	if opID != "" {
		// Acquire a per-op_id mutex to prevent TOCTOU races: without this lock,
		// two concurrent requests with the same op_id could both pass the nil
		// receipt check and each call Write, producing duplicate engrams.
		// defer mu.Unlock() holds the lock until the handler returns, covering
		// the entire check→write→store-receipt window.
		mu := s.getIdempotencyLock(opID)
		mu.Lock()
		defer mu.Unlock()

		// Re-check inside lock (now safe from concurrent duplicates).
		if receipt, err := s.engine.CheckIdempotency(ctx, opID); err == nil && receipt != nil {
			out, _ := json.Marshal(map[string]any{
				"id":         receipt.EngramID,
				"idempotent": true,
			})
			sendResult(w, id, textContent(string(out)))
			return
		}
	}

	content, ok := args["content"].(string)
	if !ok || strings.TrimSpace(content) == "" {
		sendError(w, id, -32602, "invalid params: 'content' is required (non-empty string)")
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
	if errMsg := parseValidityArgs(args, req); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	}
	if imp, ok := parseImportanceArg(args); !ok {
		sendError(w, id, -32602, "invalid params: 'importance' must be a number in [0,1]")
		return
	} else if imp != nil {
		req.Importance = imp
	}
	unknownType := applyTypeArgs(args, req)
	if t, ok := args["trust"].(string); ok {
		req.Trust = t
	}
	enrich := parseEnrichmentArgs(args, req, s.entityTypeResolver(ctx, vault))
	content = req.Content // [[markup]] may have rewritten the stored text
	if emb, errMsg := parseEmbeddingArg(args); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	} else if len(emb) > 0 {
		if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: embedding dimension %d does not match vault dimension %d", len(emb), vaultDim))
			return
		}
		req.Embedding = emb
	}

	resp, err := s.engine.Write(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if opID != "" {
		if err := s.engine.WriteIdempotency(ctx, opID, resp.ID); err != nil {
			slog.Warn("mcp: failed to record idempotency receipt", "op_id", opID, "engram_id", resp.ID, "err", err)
		}
	}
	result := WriteResult{ID: resp.ID, Concept: req.Concept}
	// #770: a caller that omits `vault` gets the default vault, and the
	// response used to be a bare {id, concept} that said nothing about where
	// the memory went. An agent working in vault X that forgets the parameter
	// once gets ok:true and a fact that is invisible from X. Routing is
	// unchanged and correct — the failure is the SILENCE — so name the
	// resolved vault. Only fires on the no-pinned-vault fallback (the resolved
	// vault is literally "default"): a vault pinned by an mk_ key must never
	// be echoed back, since the key's scope may itself be sensitive.
	if _, hasVaultArg, _ := vaultFromArgs(args); !hasVaultArg && vault == defaultVaultName {
		result.Hint = fmt.Sprintf("Stored in vault %q (no 'vault' specified). Pass vault:<name> to target your working vault.", vault)
	}
	if extra := resp.Hint; extra != "" {
		result.Hint = joinHints(result.Hint, extra)
	} else if len(content) > 500 {
		result.Hint = joinHints(result.Hint, "Tip: memories work best when each one captures a single concept. For future writes, consider using muninn_remember_batch to store multiple focused memories at once.")
	}
	result.Hint = joinHints(result.Hint, enrich.hint())
	// An unrecognised `type` is still accepted and stored — but never silently.
	// Tell the writer it was downgraded to "fact" while they still have the
	// context to correct it (principle #1, degrade-loudly form).
	result.Hint = joinHints(result.Hint, unknownTypeHint(unknownType))
	// THE PUSH: prospective notices — focal set is the caller-supplied inline
	// entities; the created engram is the self-echo guard. Inert unless
	// MUNINN_PROSPECTIVE=1.
	result.Notices = s.rememberNotices(ctx, vault, req.Entities, resp.ID)
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
	enrichReports := make([]enrichmentReport, 0, len(memoriesAny))
	// One resolver for the whole batch: at most one entity-table scan even when
	// every item declares bare names.
	resolve := s.entityTypeResolver(ctx, vault)
	// Per-item unrecognised `type` values, reported on that item's hint so a
	// batch write is never a place where a type is silently swallowed.
	unknownTypes := make([]string, 0, len(memoriesAny))
	for i, mAny := range memoriesAny {
		m, ok := mAny.(map[string]any)
		if !ok {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d] must be an object", i))
			return
		}
		content, ok := m["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
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
		if errMsg := parseValidityArgs(m, req); errMsg != "" {
			sendError(w, id, -32602, fmt.Sprintf("memories[%d]: %s", i, errMsg))
			return
		}
		if imp, ok := parseImportanceArg(m); !ok {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d].importance must be a number in [0,1]", i))
			return
		} else if imp != nil {
			req.Importance = imp
		}
		unknownTypes = append(unknownTypes, applyTypeArgs(m, req))
		if t, ok := m["trust"].(string); ok {
			req.Trust = t
		}
		rep := parseEnrichmentArgs(m, req, resolve)
		if emb, errMsg := parseEmbeddingArg(m); errMsg != "" {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d].%s", i, strings.TrimPrefix(errMsg, "invalid params: ")))
			return
		} else if len(emb) > 0 {
			if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
				sendError(w, id, -32602, fmt.Sprintf("invalid params: memories[%d].embedding dimension %d does not match vault dimension %d", i, len(emb), vaultDim))
				return
			}
			req.Embedding = emb
		}
		reqs = append(reqs, req)
		enrichReports = append(enrichReports, rep)
	}

	responses, errs := s.engine.WriteBatch(ctx, reqs)

	type batchItemResult struct {
		Index   int    `json:"index"`
		ID      string `json:"id,omitempty"`
		Concept string `json:"concept,omitempty"`
		Status  string `json:"status"`
		Error   string `json:"error,omitempty"`
		Hint    string `json:"hint,omitempty"`
	}
	results := make([]batchItemResult, len(reqs))
	for i := range reqs {
		if errs[i] != nil {
			results[i] = batchItemResult{Index: i, Status: "error", Error: errs[i].Error()}
		} else {
			results[i] = batchItemResult{Index: i, ID: responses[i].ID, Concept: reqs[i].Concept, Status: "ok"}
		}
		if h := enrichReports[i].hint(); h != "" {
			results[i].Hint = h
		}
		if h := unknownTypeHint(unknownTypes[i]); h != "" {
			if results[i].Hint != "" {
				results[i].Hint += " "
			}
			results[i].Hint += h
		}
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"results": results,
		"total":   len(results),
	})))
}

func (s *MCPServer) handleRecall(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	raw, exists := args["context"]
	if !exists {
		sendError(w, id, -32602, "invalid params: 'context' is required")
		return
	}
	var ctxArr []any
	switch v := raw.(type) {
	case string:
		// LLM clients sometimes send a bare string instead of a single-element array — coerce it.
		ctxArr = []any{v}
	case []any:
		ctxArr = v
	default:
		sendError(w, id, -32602, fmt.Sprintf("invalid params: 'context' must be a string or array of strings, got %T", raw))
		return
	}
	if len(ctxArr) == 0 {
		sendError(w, id, -32602, "invalid params: 'context' must not be empty")
		return
	}
	var contexts []string
	for _, c := range ctxArr {
		if str, ok := c.(string); ok {
			contexts = append(contexts, str)
		}
	}
	if len(contexts) == 0 {
		sendError(w, id, -32602, "invalid params: 'context' must contain at least one string")
		return
	}

	// Recall mode: validate here (fail fast with a helpful error), but FORWARD
	// the mode instead of stamping preset values into the request — the engine
	// is the single preset decider, because only it knows the effective
	// scoring mode and preset thresholds are scale-bound (#704: stamping
	// deep's ACT-R-calibrated 0.1 here silently emptied rrf vaults).
	mode, _ := args["mode"].(string)
	if mode != "" {
		if _, modeErr := lookupMode(mode); modeErr != nil {
			sendError(w, id, -32602, modeErr.Error())
			return
		}
	}

	// A caller-omitted threshold is forwarded as 0 ("unset") and the ENGINE
	// applies its fusion-aware default (rrf -> 0.001 per #590, ACT-R -> 0.1,
	// weighted_sum -> 0.5 — each the value its scoring path was calibrated
	// against). MCP was the only transport that pre-filled a default here;
	// REST /activate, gRPC and MBP all forward 0 already, and the surface
	// pre-fill is how an ACT-R-calibrated number ended up gating rrf and
	// weighted_sum vaults it was never derived for (COG-6, and the #754
	// review's finding 5). An explicit caller threshold is never modified.
	threshold := float32(0)
	_, thresholdSet := args["threshold"]
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

	readOnly, roErrMsg := resolveReadOnly(ctx, args)
	if roErrMsg != "" {
		sendError(w, id, -32001, roErrMsg)
		return
	}

	req := &mbp.ActivateRequest{
		Vault:      vault,
		Context:    contexts,
		Mode:       mode,
		Threshold:  threshold,
		MaxResults: limit,
		Profile:    profile,
		ReadOnly:   readOnly,
	}

	// Ownership-lease work-queue visibility (#548).
	if caller, ok := args["caller"].(string); ok {
		req.CallerOwner = caller
	}
	if includeLeased, ok := args["include_leased"].(bool); ok {
		req.IncludeLeased = includeLeased
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

	// Valid-time axis: as_of ("what was true at T") and include_invalid
	// ("show history"). Orthogonal to since/before above, which filter on the
	// TRANSACTION axis (CreatedAt).
	if asOfStr, ok := args["as_of"].(string); ok && asOfStr != "" {
		t, err := time.Parse(time.RFC3339, asOfStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'as_of': must be ISO 8601 (e.g. 2026-05-01T00:00:00Z)")
			return
		}
		req.AsOf = &t
	}
	if includeInvalid, ok := args["include_invalid"].(bool); ok {
		req.IncludeInvalid = includeInvalid
	}

	// Tag filters: tags_all (AND), tags_any (OR), tag_filter (prefix value range).
	parseStringArrayArg := func(v any) []string {
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if tags := parseStringArrayArg(args["tags_all"]); len(tags) > 0 {
		req.Filters = append(req.Filters, mbp.Filter{Field: "tags_all", Op: "all", Value: tags})
	}
	if tags := parseStringArrayArg(args["tags_any"]); len(tags) > 0 {
		req.Filters = append(req.Filters, mbp.Filter{Field: "tags_any", Op: "any", Value: tags})
	}
	if raw, present := args["tag_filter"]; present {
		// Validate on presence — do NOT silently drop a malformed tag_filter. A
		// caller who passes a string (a natural mistake: tags_all/tags_any ARE
		// string arrays) or any non-object must get an error, never an unfiltered
		// recall that looks filtered (principle #1). tag_filter is an object:
		// {"prefix":"due:","lte":"2026-06-17"}.
		tf, ok := raw.(map[string]any)
		if !ok {
			sendError(w, id, -32602, `invalid 'tag_filter': must be an object like {"prefix":"due:","lte":"2026-06-17"} (got a non-object)`)
			return
		}
		prefix, _ := tf["prefix"].(string)
		if prefix == "" {
			sendError(w, id, -32602, "invalid 'tag_filter': 'prefix' is required")
			return
		}
		op, bound := "", ""
		for _, cmp := range []string{"lte", "gte", "lt", "gt", "eq"} {
			if b, ok := tf[cmp].(string); ok {
				op, bound = cmp, b
				break
			}
		}
		if op == "" {
			sendError(w, id, -32602, "invalid 'tag_filter': one of lte/gte/lt/gt/eq (string) is required")
			return
		}
		req.Filters = append(req.Filters, mbp.Filter{Field: "tag_prefix", Op: op, Value: [2]string{prefix, bound}})
	}

	if emb, errMsg := parseEmbeddingArg(args); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	} else if len(emb) > 0 {
		if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: embedding dimension %d does not match vault dimension %d", len(emb), vaultDim))
			return
		}
		req.Embedding = emb
	}

	annotate, _ := args["annotate"].(bool)

	resp, err := s.engine.Activate(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	var memories []Memory
	for i := range resp.Activations {
		memories = append(memories, activationToMemory(&resp.Activations[i]))
	}

	if annotate {
		for i, item := range resp.Activations {
			ann, err := s.engine.GetAnnotations(ctx, vault, item.ID, req)
			if err != nil || ann == nil {
				// Non-fatal: log and skip annotations for this result.
				slog.Warn("handleRecall: GetAnnotations failed", "id", item.ID, "err", err)
				continue
			}
			// Augment (not replace) the always-on supersession annotation that
			// activationToMemory already attached, so superseded_by/current_version
			// from the ranking phase are preserved.
			augmentAnnotations(&memories[i], &item, ann)
		}
	}

	result := map[string]any{
		"memories": memories,
		"total":    resp.TotalFound,
	}
	// SemanticDegraded: the vector signal for this recall could not be
	// trusted (embed backend unreachable, an all-zero embedding, or a
	// failed post-load cosine fallback read). Recall still returns results
	// via BM25/decay/Hebbian, but the caller should know semantic ranking
	// was compromised rather than silently trusting it (principle #2).
	if resp.SemanticDegraded {
		result["semantic_degraded"] = true
	}
	// COG-29 (#764): at least two returned memories are declared to
	// contradict each other with the conflict unresolved, so neither is
	// presented as the answer. This map is hand-built and is NOT a mirror of
	// mbp.ActivateResponse — a field added to the struct alone reaches REST
	// and silently vanishes here, which is what
	// TestRecallOverMCP_ConflictBlockAndAnnotations exists to catch.
	if resp.Conflict != nil {
		result["conflict"] = resp.Conflict
	}
	// THE PUSH: prospective notices — focal set derives from the RETURNED
	// results; readOnly (COG-11) suppresses the fired-marker write. Omitted
	// when empty; inert unless MUNINN_PROSPECTIVE=1.
	if notices := s.recallNotices(ctx, vault, resp.Activations, readOnly); len(notices) > 0 {
		result["notices"] = notices
	}
	// Abstention is self-describing: the caller can tell "the vault has no
	// answer" (abstained, with a reason) from a generic empty set. Only ever
	// present on empty results — an annotation on every response would stop
	// meaning anything.
	if resp.Abstained && len(memories) == 0 {
		result["abstained"] = true
		result["abstained_reason"] = resp.AbstainedReason
	}
	if len(memories) == 0 {
		// The hint names the threshold because it is the lever that actually
		// changes the outcome — evaluators called the old advice wrong for
		// suggesting mode='recent' while omitting it.
		hint := "No results cleared the relevance threshold. If you expected a match, retry with a lower 'threshold' (e.g. 0.05) or rephrase closer to the stored wording. For session continuity try mode='recent', or use muninn_where_left_off."
		p, pErr := s.engine.GetVaultPlasticity(ctx, vault)
		if pErr == nil && p != nil && p.MultiUser {
			hint = "No results cleared the relevance threshold. If you expected a match, retry with a lower 'threshold' (e.g. 0.05) or rephrase closer to the stored wording. For session continuity try mode='recent' scoped to your per-user tag (this vault is shared; muninn_where_left_off is vault-global)."
		}
		// COG-6: never clobber an explicit threshold — only hint. An rrf vault's
		// blended finals rarely exceed ~0.15, so a caller-supplied threshold at
		// or above 0.01 can silently filter every result. Only fires when the
		// caller set the threshold explicitly (thresholdSet); the omitted-arg
		// case is already handled mode-aware above.
		if pErr == nil && p != nil && p.ScoringFusion == "rrf" && thresholdSet && threshold >= 0.01 {
			hint += fmt.Sprintf(" Note: this vault uses rrf (rank-based) scoring — scores rarely exceed ~0.15; a threshold of %g filters everything; try <= 0.01.", threshold)
		}
		result["hint"] = hint
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleRead(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	readOnly, roErrMsg := resolveReadOnly(ctx, args)
	if roErrMsg != "" {
		sendError(w, id, -32001, roErrMsg)
		return
	}

	resp, err := s.engine.Read(ctx, &mbp.ReadRequest{ID: engramID, Vault: vault, ReadOnly: readOnly})
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
	// #807: honor the caller's explicit hard flag instead of silently
	// downgrading it to a soft delete. Same authorization as any other
	// mutating tool call — muninn_forget is already isMutatingTool-classified
	// (blocked for observe-mode credentials) and the engine itself refuses
	// EVERY Forget call, hard or soft, from an append-mode credential
	// (SEC-15) — the identical bar gRPC's ForgetRequest.Hard already clears
	// with no additional elevation, so this does not make MCP more
	// permissive than the other transports that already reach hard delete.
	hard, _ := args["hard"].(bool)
	req := &mbp.ForgetRequest{ID: engramID, Hard: hard, Vault: vault}

	// not_true_since: invalidate on the valid-time axis (stamp ValidUntil)
	// instead of soft-deleting. The memory stays recoverable via as_of /
	// include_invalid; default recall stops returning it (COG-19).
	if ntsStr, ok := args["not_true_since"].(string); ok && ntsStr != "" {
		t, err := time.Parse(time.RFC3339, ntsStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'not_true_since': must be ISO 8601 (e.g. 2026-07-01T00:00:00Z)")
			return
		}
		req.NotTrueSince = &t
	}

	_, err := s.engine.Forget(ctx, req)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if req.NotTrueSince != nil {
		sendResult(w, id, textContent(mustJSON(map[string]any{
			"ok":          true,
			"invalidated": true,
			"valid_until": req.NotTrueSince.UTC().Format(time.RFC3339),
			"hint":        "Memory invalidated on the valid-time axis (not deleted). It stays retrievable via as_of or include_invalid; default recall no longer returns it.",
		})))
		return
	}

	// Check if the forgotten engram had children. Ordinal keys for children are NOT
	// cleaned up when the parent is soft-deleted, so CountChildren will still find them.
	childCount, warnErr := s.engine.CountChildren(ctx, vault, engramID)
	if warnErr == nil && childCount > 0 {
		sendResult(w, id, textContent(fmt.Sprintf(`{"ok":true,"warning":"engram had %d child(ren) which are now orphaned; consider forgetting them too"}`, childCount)))
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
	if srcID == dstID {
		// A memory cannot supersede, support, or contradict ITSELF. Accepting a
		// self-link created an edge that annotated the memory as conflicting
		// with itself (observed live by an evaluator), poisoning the one channel
		// — declared edges — the system treats as ground truth. This is a caller
		// error, not a declaration; reject it loudly.
		sendError(w, id, -32602, "invalid params: source_id and target_id are the same memory — a memory cannot be linked to itself")
		return
	}
	relType, unknownRel := relTypeFromStringChecked(rel)
	_, err := s.engine.Link(ctx, &mbp.LinkRequest{
		SourceID: srcID,
		TargetID: dstID,
		RelType:  relType,
		Weight:   weight,
		Vault:    vault,
	})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	// An unrecognised relation is still linked (as the inert relates_to) — but
	// never silently. Tell the caller their declaration was not recorded, and
	// name the valid relations, while they can still re-link.
	if h := unknownRelationHint(unknownRel); h != "" {
		sendResult(w, id, textContent(mustJSON(map[string]any{"ok": true, "hint": h})))
		return
	}
	sendResult(w, id, textContent(`{"ok":true}`))
}

// contradictionReporter is the optional richer contradictions read. Engines
// that implement it report resolved concepts, real detection timestamps, and —
// critically — the contradictions an agent has explicitly linked that the 30s
// batch detector has not flagged yet. See mcpEngineAdapter.GetContradictionReport
// for why this is probed rather than added to the Engine interface.
type contradictionReporter interface {
	GetContradictionReport(ctx context.Context, vault string) (*ContradictionsReport, error)
}

func (s *MCPServer) handleContradictions(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	rep, ok := s.engine.(contradictionReporter)
	if !ok {
		// Legacy shape: markers only, no detection state. Kept byte-compatible
		// so an engine without the richer read still answers the question it
		// can actually answer.
		pairs, err := s.engine.GetContradictions(ctx, vault)
		if err != nil {
			sendError(w, id, -32000, "tool error: "+err.Error())
			return
		}
		sendResult(w, id, textContent(mustJSON(map[string]any{"contradictions": pairs})))
		return
	}

	report, err := rep.GetContradictionReport(ctx, vault)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if report.Contradictions == nil {
		report.Contradictions = []ContradictionPair{}
	}
	// The note exists because an empty or short list used to be read as "there
	// are no contradictions" when the truth was "the detector has not run yet".
	// Say which one this is, in words, every time it is not simply "none".
	switch {
	case !report.ScanComplete:
		report.Note = "contradiction scan hit its cap: pending_count is a lower bound, and pairs declared by an explicit contradicts link may be missing from this list"
	case report.PendingCount > 0:
		report.Note = fmt.Sprintf("%d contradiction(s) are declared by an explicit link and are already honored by recall; the asynchronous confidence penalty for them has not been applied yet (it runs on a ~30s batch interval)", report.PendingCount)
	case report.DetectedCount == 0 && report.ResolvedCount > 0:
		report.Note = fmt.Sprintf("no live contradictions in this vault; %d recorded pair(s) have been resolved (see resolved_by)", report.ResolvedCount)
	case report.DetectedCount == 0:
		report.Note = "no contradictions in this vault, and none awaiting detection"
	}
	sendResult(w, id, textContent(mustJSON(report)))
}

func (s *MCPServer) handleStatus(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	resp, err := s.engine.Stat(ctx, &mbp.StatRequest{Vault: vault})
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	enrichMode := s.engine.GetEnrichmentMode(ctx)
	status := VaultStatus{
		Vault:          vault,
		TotalMemories:  resp.EngramCount,
		Health:         "good",
		EnrichmentMode: enrichMode,
		// Plugins: populated in a future task when plugin registry is accessible via handleStatus.
	}
	sendResult(w, id, textContent(mustJSON(status)))
}

func (s *MCPServer) handleEvolve(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok1 := args["id"].(string)
	newContent, ok2 := args["new_content"].(string)
	reason, ok3 := args["reason"].(string)
	if !ok1 || !ok2 || !ok3 || engramID == "" || newContent == "" || reason == "" {
		var missing []string
		if !ok1 || engramID == "" {
			missing = append(missing, "'id' (engram ID to update)")
		}
		if !ok2 || newContent == "" {
			missing = append(missing, "'new_content' (replacement text)")
		}
		if !ok3 || reason == "" {
			missing = append(missing, "'reason' (why the memory changed)")
		}
		sendError(w, id, -32602, fmt.Sprintf("invalid params: missing required field(s): %s", strings.Join(missing, ", ")))
		return
	}
	var evolveEmb []float32
	if emb, errMsg := parseEmbeddingArg(args); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	} else if len(emb) > 0 {
		if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: embedding dimension %d does not match vault dimension %d", len(emb), vaultDim))
			return
		}
		evolveEmb = emb
	}
	var evolveConcept string
	if c, ok := args["concept"].(string); ok {
		evolveConcept = c
	}
	// Inline [[markup]] is stripped on evolve exactly as on remember — the same
	// input must not produce different stored content per verb. GUARD the
	// return: extractMarkupEntities returns ("", nil) when there is nothing to
	// do (its documented contract, honoured by parseEnrichmentArgs), and an
	// unconditional assign here once destroyed the content of EVERY plain-text
	// evolve — the successor stored empty text while the predecessor was
	// superseded away. Caught by adversarial review (finding 1), not by tests:
	// the evolve fakes never read content back.
	var markupNames []string
	if stripped, names := extractMarkupEntities(newContent); len(names) > 0 {
		newContent = stripped
		markupNames = names
	}
	// Optional inline entities — same shape and normalization as remember's.
	// When present they REPLACE the entity links otherwise carried forward
	// from the predecessor.
	var evolveEntities []mbp.InlineEntity
	if entitiesAny, ok := args["entities"].([]any); ok {
		for i, eAny := range entitiesAny {
			if i >= 20 {
				break
			}
			eMap, ok := eAny.(map[string]any)
			if !ok {
				continue
			}
			name, _ := eMap["name"].(string)
			typ, _ := eMap["type"].(string)
			name = strings.TrimSpace(norm.NFKC.String(name))
			typ = normalizeEntityType(typ)
			if name == "" || typ == "" {
				continue
			}
			evolveEntities = append(evolveEntities, mbp.InlineEntity{Name: name, Type: typ})
		}
	}
	// Markup names become entities ONLY when the caller also supplied explicit
	// entities[]. The engine treats ANY non-empty entity list on evolve as
	// "REPLACE the carried set" (EvolveAt suppresses the carry entirely), so
	// letting markup alone populate the list silently destroyed every
	// predecessor entity link — a side effect the caller never asked for
	// (adversarial review, finding 2). With explicit entities the caller has
	// already opted into replace, and markup appends with the same vault-table
	// type resolution remember uses. Markup-only keeps the carry and WARNS.
	var markupOnlyWarning string
	if len(markupNames) > 0 {
		if len(evolveEntities) > 0 {
			resolve := s.entityTypeResolver(ctx, vault)
			for _, n := range markupNames {
				dup := false
				for _, e := range evolveEntities {
					if strings.EqualFold(e.Name, n) {
						dup = true
						break
					}
				}
				if !dup {
					typ, known := resolve(n)
					if !known {
						typ = "other"
					}
					evolveEntities = append(evolveEntities, mbp.InlineEntity{Name: n, Type: typ})
				}
			}
		} else {
			markupOnlyWarning = fmt.Sprintf(
				"markup entities (%s) were stripped from the content but NOT linked: evolve without an explicit 'entities' list carries the predecessor's entity links forward, and adding markup names would REPLACE that carried set. To link them, pass 'entities' explicitly (which replaces the carry).",
				strings.Join(markupNames, ", "))
		}
	}
	// effective_at: valid-time boundary between predecessor and successor
	// (default now). The old version's ValidUntil and the new version's
	// ValidFrom both become this moment.
	var effectiveAt time.Time
	if eaStr, ok := args["effective_at"].(string); ok && eaStr != "" {
		t, err := time.Parse(time.RFC3339, eaStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'effective_at': must be ISO 8601 (e.g. 2026-06-15T12:00:00Z)")
			return
		}
		effectiveAt = t
	}
	// importance: optional override for the successor; absent inherits the
	// predecessor's explicit importance (unset stays unset).
	evolveImportance, impOK := parseImportanceArg(args)
	if !impOK {
		sendError(w, id, -32602, "invalid params: 'importance' must be a number in [0,1]")
		return
	}
	result, err := s.engine.Evolve(ctx, vault, engramID, newContent, reason, evolveEmb, evolveConcept, evolveEntities, evolveImportance, effectiveAt)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if markupOnlyWarning != "" {
		result.Warnings = append(result.Warnings, markupOnlyWarning)
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
		if v < 0 {
			v = 0
		}
		maxHops = int(v)
	}
	if maxHops > 5 {
		maxHops = 5
	}
	maxNodes := 20
	if v, ok := args["max_nodes"].(float64); ok {
		if v < 0 {
			v = 0
		}
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
	followEntities, _ := args["follow_entities"].(bool)
	req := &TraverseRequest{
		StartID:        startID,
		MaxHops:        maxHops,
		MaxNodes:       maxNodes,
		RelTypes:       relTypes,
		FollowEntities: followEntities,
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
	explainReq := &ExplainRequest{EngramID: engramID, Query: query}
	if emb, errMsg := parseEmbeddingArg(args); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	} else if len(emb) > 0 {
		if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: embedding dimension %d does not match vault dimension %d", len(emb), vaultDim))
			return
		}
		explainReq.Embedding = emb
	}
	result, err := s.engine.Explain(ctx, vault, explainReq)
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
		if v < 0 {
			v = 0
		}
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

func (s *MCPServer) handleWhereLeftOff(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	limit := 10
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}

	// S3: WhereLeftOff has no write side effects regardless of read_only (it
	// never reinforces — see engine_where_left_off.go), so there is nothing
	// to set on the downstream call. Still validate/reject for API
	// consistency with muninn_recall/muninn_read (RFC S3 requires all three).
	if _, roErrMsg := resolveReadOnly(ctx, args); roErrMsg != "" {
		sendError(w, id, -32001, roErrMsg)
		return
	}

	var excludeTypeLabels []string
	if raw, ok := args["exclude_type_labels"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				excludeTypeLabels = append(excludeTypeLabels, s)
			}
		}
	}

	entries, err := s.engine.WhereLeftOff(ctx, vault, limit, excludeTypeLabels)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if entries == nil {
		entries = []WhereLeftOffEntry{}
	}
	hint := "These are your most recently accessed memories. Use them to orient yourself for this session."
	if p, perr := s.engine.GetVaultPlasticity(ctx, vault); perr == nil && p != nil && p.MultiUser {
		hint = "These are the most recently accessed memories across ALL users of this shared vault — not necessarily yours. For your own session context, use muninn_recall scoped to your per-user tag."
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"memories": entries,
		"count":    len(entries),
		"hint":     hint,
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
	if strings.TrimSpace(rootInput.Concept) == "" {
		sendError(w, id, -32602, "invalid params: root.concept is required")
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

// handleRecallTree handles the muninn_recall_tree tool call.
//
// Behavior notes:
//   - max_depth is capped to 50; negative values are normalized to 0 (unlimited).
//   - limit is capped to 1000 per-node children to prevent runaway responses.
//   - include_completed=false filters CHILDREN only. If the root itself is
//     soft-deleted, it is still returned — the caller explicitly requested this
//     root by ID, so the root is always returned regardless of its state. The
//     include_completed flag is a child-level filter, not a root-level guard.
func (s *MCPServer) handleRecallTree(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	rootID, ok := args["root_id"].(string)
	if !ok || rootID == "" {
		sendError(w, id, -32602, "invalid params: 'root_id' is required")
		return
	}
	maxDepth := 10
	if d, ok := args["max_depth"].(float64); ok {
		maxDepth = int(d)
		if maxDepth < 0 {
			maxDepth = 0 // 0 = unlimited; normalize negative values
		}
		if maxDepth > 50 {
			maxDepth = 50
		}
	}
	limit := 0
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
		if limit > 1000 {
			limit = 1000 // cap per-node child limit
		}
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

func (s *MCPServer) handleFindByEntity(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	entityName, ok := args["entity_name"].(string)
	if !ok || entityName == "" {
		sendError(w, id, -32602, "invalid params: 'entity_name' is required")
		return
	}
	limit := 20
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	res, err := s.engine.FindByEntity(ctx, vault, entityName, limit)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if res == nil {
		res = &engine.FindByEntityResult{}
	}
	engrams := res.Engrams
	type engramEntry struct {
		ID               string  `json:"id"`
		Concept          string  `json:"concept"`
		Summary          string  `json:"summary,omitempty"`
		State            string  `json:"state"`
		Type             string  `json:"type"`
		TypeLabel        string  `json:"type_label,omitempty"`
		Importance       float64 `json:"importance"`
		ImportanceSource string  `json:"importance_source"`
	}
	entries := make([]engramEntry, 0, len(engrams))
	for _, e := range engrams {
		imp, impSrc := importanceFields(e.Importance, e.MemoryType, e.Trust)
		entries = append(entries, engramEntry{
			ID:               e.ID.String(),
			Concept:          e.Concept,
			Summary:          e.Summary,
			State:            lifecycleStateLabel(e.State),
			Type:             e.MemoryType.String(),
			TypeLabel:        e.TypeLabel,
			Importance:       imp,
			ImportanceSource: impSrc,
		})
	}
	payload := map[string]any{
		"entity":  entityName,
		"engrams": entries,
		"count":   len(entries),
	}
	// Report the resolution when the serving entity differs from the query
	// (fuzzy match) — never substitute silently (issue #571).
	if res.MatchedEntity != "" {
		payload["matched_entity"] = res.MatchedEntity
	}
	if res.Fuzzy {
		payload["fuzzy"] = true
		if len(res.Candidates) > 0 {
			payload["other_candidates"] = res.Candidates
		}
	}
	out, _ := json.Marshal(payload)
	sendResult(w, id, textContent(string(out)))
}

func (s *MCPServer) handleEntityState(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	entityName, ok := args["entity_name"].(string)
	if !ok || entityName == "" {
		sendError(w, id, -32602, "invalid params: 'entity_name' is required")
		return
	}
	state, ok := args["state"].(string)
	if !ok || state == "" {
		sendError(w, id, -32602, "invalid params: 'state' is required")
		return
	}
	validEntityStates := map[string]bool{
		"active":     true,
		"deprecated": true,
		"merged":     true,
		"resolved":   true,
	}
	if !validEntityStates[state] {
		sendError(w, id, -32602, "invalid params: 'state' must be one of: active, deprecated, merged, resolved")
		return
	}
	mergedInto, _ := args["merged_into"].(string)
	if state == "merged" && mergedInto == "" {
		sendError(w, id, -32602, "invalid params: 'merged_into' is required when state=merged")
		return
	}
	// Normalise + coerce unknown types to "other" so this deliberate user
	// action behaves identically to muninn_remember (issue #501). Empty stays
	// empty: the engine reads "" as "preserve the existing type".
	entityType, _ := args["type"].(string)
	entityType = normalizeEntityType(entityType)

	if err := s.engine.SetEntityState(ctx, entityName, state, mergedInto, entityType); err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	resp := map[string]any{
		"entity": entityName,
		"state":  state,
		"ok":     true,
	}
	if entityType != "" {
		resp["type"] = entityType
	}
	out, _ := json.Marshal(resp)
	sendResult(w, id, textContent(string(out)))
}

func (s *MCPServer) handleEntityStateBatch(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	opsAny, ok := args["operations"].([]any)
	if !ok || len(opsAny) == 0 {
		sendError(w, id, -32602, "invalid params: 'operations' is required and must be a non-empty array")
		return
	}
	if len(opsAny) > 50 {
		sendError(w, id, -32602, "invalid params: 'operations' exceeds maximum of 50")
		return
	}

	validEntityStates := map[string]bool{
		"active": true, "deprecated": true, "merged": true, "resolved": true,
	}

	ops := make([]engine.EntityStateOp, 0, len(opsAny))
	for i, opAny := range opsAny {
		op, ok := opAny.(map[string]any)
		if !ok {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: operations[%d] must be an object", i))
			return
		}
		entityName, ok := op["entity_name"].(string)
		if !ok || entityName == "" {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: operations[%d].entity_name is required", i))
			return
		}
		state, ok := op["state"].(string)
		if !ok || !validEntityStates[state] {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: operations[%d].state must be one of: active, deprecated, merged, resolved", i))
			return
		}
		mergedInto, _ := op["merged_into"].(string)
		if state == "merged" && mergedInto == "" {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: operations[%d].merged_into is required when state=merged", i))
			return
		}
		entityType, _ := op["type"].(string)
		entityType = normalizeEntityType(entityType)
		ops = append(ops, engine.EntityStateOp{
			EntityName: entityName,
			State:      state,
			MergedInto: mergedInto,
			EntityType: entityType,
		})
	}

	errs := s.engine.SetEntityStateBatch(ctx, ops)

	type batchItemResult struct {
		Index  int    `json:"index"`
		Entity string `json:"entity"`
		State  string `json:"state,omitempty"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]batchItemResult, len(ops))
	for i, op := range ops {
		if errs[i] != nil {
			results[i] = batchItemResult{Index: i, Entity: op.EntityName, Status: "error", Error: errs[i].Error()}
		} else {
			results[i] = batchItemResult{Index: i, Entity: op.EntityName, State: op.State, Status: "ok"}
		}
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"results": results,
		"total":   len(results),
	})))
}

func (s *MCPServer) handleAddChild(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	parentID, ok := args["parent_id"].(string)
	if !ok || parentID == "" {
		sendError(w, id, -32602, "invalid params: 'parent_id' is required")
		return
	}
	concept, ok := args["concept"].(string)
	if !ok || strings.TrimSpace(concept) == "" {
		sendError(w, id, -32602, "invalid params: 'concept' is required")
		return
	}
	content, ok := args["content"].(string)
	if !ok || content == "" {
		sendError(w, id, -32602, "invalid params: 'content' is required")
		return
	}
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
	if emb, errMsg := parseEmbeddingArg(args); errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	} else if len(emb) > 0 {
		if vaultDim := s.engine.GetVaultEmbedDim(ctx, vault); vaultDim > 0 && len(emb) != vaultDim {
			sendError(w, id, -32602, fmt.Sprintf("invalid params: embedding dimension %d does not match vault dimension %d", len(emb), vaultDim))
			return
		}
		child.Embedding = emb
	}
	result, err := s.engine.AddChild(ctx, vault, parentID, child)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleEntityClusters(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	minCount := 2
	if v, ok := args["min_count"].(float64); ok {
		if v < 0 {
			v = 0
		}
		minCount = int(v)
	}
	if minCount < 1 {
		minCount = 1
	}
	topN := 20
	if v, ok := args["top_n"].(float64); ok {
		if v < 0 {
			v = 0
		}
		topN = int(v)
	}
	if topN < 1 {
		topN = 1
	}

	clusters, err := s.engine.GetEntityClusters(ctx, vault, minCount, topN)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if clusters == nil {
		clusters = []EntityClusterResult{}
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"clusters": clusters,
		"count":    len(clusters),
	})))
}

func (s *MCPServer) handleExportGraph(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	format := "json-ld"
	if f, ok := args["format"].(string); ok && f != "" {
		format = f
	}
	if format != "json-ld" && format != "graphml" {
		sendError(w, id, -32602, "invalid params: 'format' must be 'json-ld' or 'graphml'")
		return
	}
	includeEngrams, _ := args["include_engrams"].(bool)

	g, err := s.engine.ExportGraph(ctx, vault, includeEngrams)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	var data string
	switch format {
	case "graphml":
		data, err = engine.FormatGraphGraphML(g)
	default:
		data, err = engine.FormatGraphJSONLD(g)
	}
	if err != nil {
		sendError(w, id, -32000, "tool error: format: "+err.Error())
		return
	}

	sendResult(w, id, textContent(mustJSON(map[string]any{
		"format":     format,
		"data":       data,
		"node_count": len(g.Nodes),
		"edge_count": len(g.Edges),
	})))
}

// applyTypeArgs parses the "type" and "type_label" arguments from an MCP call
// and sets MemoryType + TypeLabel on the WriteRequest accordingly.
// memoryTypeNames is the caller-facing vocabulary ParseMemoryType accepts. It is
// the single source of truth for what unknownTypeHint offers, so a new
// MemoryType cannot be added to the enum without the nudge learning to name it
// (pinned by TestApplyTypeArgs_NoValidTypeIsEverReportedUnknown).
var memoryTypeNames = []string{
	"fact", "decision", "observation", "preference", "issue", "bugfix",
	"bug_report", "task", "procedure", "event", "experience", "goal", "constraint",
}

// unknownTypeHint renders the loud-but-graceful notice for a `type` value that
// is not a recognised MemoryType. Empty rejected value → empty hint (nothing was
// discarded, so there is nothing to report).
func unknownTypeHint(rejected string) string {
	if rejected == "" {
		return ""
	}
	return fmt.Sprintf(
		"Note: type %q is not a recognised memory type, so this memory was stored as \"fact\" and %q was kept only as a display label. "+
			"Typed memories participate in recall and graph features that plain facts do not. Valid types: %s.",
		rejected, rejected, strings.Join(memoryTypeNames, ", "))
}

// applyTypeArgs maps the caller's `type`/`type_label` arguments onto the write
// request, and RETURNS the `type` value if it was not a recognised MemoryType.
//
// The storage behaviour is deliberately unchanged — an unrecognised type is
// still accepted and still stored as TypeFact with the caller's string kept as
// TypeLabel, so no existing writer breaks and no memory is ever rejected. What
// changed is that the substitution is no longer SILENT: callers surface the
// returned value via unknownTypeHint so the writer learns their type was
// discarded while they still have the context to correct it.
//
// This is the project's first principle applied to the write path ("explicit
// config is never silently substituted") in its degrade-loudly form (#578/#740):
// accept and continue, but never pretend nothing happened. A real-corpus census
// found 66.3% of memories typed by their writer but untyped by the type system
// as a direct result of the previous silent behaviour.
func applyTypeArgs(args map[string]any, req *mbp.WriteRequest) (unknownType string) {
	typeStr, _ := args["type"].(string)
	explicitLabel, _ := args["type_label"].(string)

	if typeStr != "" {
		if mt, ok := storage.ParseMemoryType(typeStr); ok {
			req.MemoryType = uint8(mt)
			if explicitLabel == "" {
				req.TypeLabel = typeStr
			}
		} else {
			// Not a known enum name — store as free-form label, default to Fact,
			// and report it so the caller can be told (never silently swallowed).
			req.MemoryType = uint8(storage.TypeFact)
			if explicitLabel == "" {
				req.TypeLabel = typeStr
			}
			unknownType = typeStr
		}
	}
	if explicitLabel != "" {
		req.TypeLabel = explicitLabel
	}
	return unknownType
}

// validEntityTypes is the single source of truth for the 14 recognised entity
// types accepted on every user-facing MCP write path (remember, remember_batch,
// entity_state, entity_state_batch, apply_enrichment).
var validEntityTypes = map[string]bool{
	"person": true, "organization": true, "location": true, "concept": true,
	"technology": true, "project": true, "tool": true, "database": true,
	"service": true, "framework": true, "language": true, "product": true,
	"event": true, "other": true,
}

// normalizeEntityType lowercases and trims an entity type, then coerces any
// unrecognised value to "other" — matching muninn_remember's inline-entity
// behavior so all user-facing write paths treat the same input identically.
//
// An empty type is preserved as empty: callers use "" to mean "omitted", which
// the engine interprets as "leave the existing type unchanged". Coercing "" to
// "other" would silently overwrite a previously-correct type, so it is excluded.
//
// This intentionally does NOT govern the server-side enrich plugin (internal/
// plugin/enrich/parse.go), which deliberately passes unknown LLM-produced types
// through per #334; coercion here only covers the explicit MCP write paths.
func normalizeEntityType(typ string) string {
	typ = strings.ToLower(strings.TrimSpace(typ))
	if typ == "" {
		return ""
	}
	if !validEntityTypes[typ] {
		return "other"
	}
	return typ
}

// ── the middle gear: cheap entity declaration ────────────────────────────────
//
// A bare muninn_remember(content) costs ~20 tokens and produces a memory that is
// invisible to every entity-based tool. A fully-declared write costs roughly 8x
// the content in JSON. Four independent evaluators, asked what they would
// actually do mid-task with a user waiting, all answered "the cheap one" — so
// the cheap one is what the graph gets. Measured on a 4,216-engram corpus: 4.81%
// of writes carried any declaration, 29.9% carried an entity.
//
// Two middle gears close that gap without an LLM anywhere in the server:
//
//  1. BARE STRING entities — `entities:["PostgreSQL","Auth Service"]`. The type
//     is resolved from the vault's OWN entity table (a name it already knows
//     keeps its known type; anything else becomes "other" and is REPORTED).
//  2. Inline [[markup]] in the content — `"Migrated [[Auth Service]] to
//     [[PostgreSQL]]"`. A tokenizer, not a model: the brackets are stripped and
//     the bracketed names become entities.
//
// Both mechanisms only ever resolve TYPES for names the CALLER supplied. Neither
// invents a name from content — #713's counterfactual measured hand-adjudicated
// precision of ~0.76 for INFERRED entities, and that is the pollution this
// project keeps having to dig out of. [[markup]] is caller intent, expressed in
// the content instead of in JSON.
//
// The honest counter-argument, recorded rather than argued away: once agents
// learn the server resolves types for them, the explicit-declaration rate may
// fall further, and the graph's typing ceiling becomes whatever the entity table
// already knows. A vault's FIRST mention of an entity will be typed "other"
// unless the caller declares it. We take that trade because an untyped entity is
// worth strictly more than no entity at all — the same reasoning that removed
// the type enum — but it is a trade, not a free win.

const (
	maxInlineEntities = 20
	// entityTypeScanLimit caps the vault entity table pulled in to resolve bare
	// names. Entities are returned mention-count descending, so the names a
	// writer is most likely to reuse are the ones inside the cap.
	entityTypeScanLimit = 5000
	// maxMarkupNameLen bounds what [[...]] is willing to treat as an entity name.
	// Anything longer is prose or code, not a declaration, and is left alone.
	maxMarkupNameLen = 64
)

// entityTypeLookup resolves a caller-supplied entity NAME to a type the vault
// already knows. A nil lookup, or one that reports !ok, means "unknown" — never
// an error, never a rejection.
type entityTypeLookup func(name string) (typ string, known bool)

// entityTypeResolver returns a lazy, single-scan lookup over the vault's entity
// table. The scan is only paid for if a caller actually supplies a name with no
// type, and at most once per tool call.
func (s *MCPServer) entityTypeResolver(ctx context.Context, vault string) entityTypeLookup {
	var (
		loaded bool
		idx    map[string]string
	)
	return func(name string) (string, bool) {
		if !loaded {
			loaded = true
			idx = make(map[string]string)
			rows, err := s.engine.ListEntities(ctx, vault, entityTypeScanLimit, "")
			if err != nil {
				// Degrade loudly-but-gracefully: bare names still become "other"
				// and the caller is told, rather than losing the entity.
				slog.Warn("mcp: entity-type resolution unavailable; bare-string entities will be typed \"other\"",
					"vault", vault, "err", err)
			}
			for _, r := range rows {
				if r.Type == "" {
					continue
				}
				if _, dup := idx[strings.ToLower(r.Name)]; !dup {
					idx[strings.ToLower(r.Name)] = r.Type
				}
			}
		}
		t, ok := idx[strings.ToLower(name)]
		return t, ok
	}
}

// enrichmentReport is what the write path owes the caller about how their
// enrichment arguments were actually interpreted. Every field here is something
// that used to happen silently.
type enrichmentReport struct {
	malformed       int      // items that carried no usable name at all
	unresolvedNames []string // caller-supplied names with no known type -> "other"
	coercedTypes    []string // caller-supplied types outside the 14 -> "other"
	markup          int      // entities lifted out of [[...]] in the content
	renamedKeys     []string // near-miss item keys accepted, e.g. entity_name -> name
	unknownArgs     []string // near-miss top-level parameters that were ignored
}

// nearMissArgs maps parameter names that evaluators actually sent to the ones
// this schema accepts. A wrong name currently produces an opaque -32602 (or,
// worse, a silently dropped field) with no indication of what was expected.
var nearMissArgs = map[string]string{
	"entity":               "entities",
	"entity_list":          "entities",
	"entity_relationship":  "entity_relationships",
	"entity_relations":     "entity_relationships",
	"relationship":         "relationships",
	"relation":             "relationships",
	"related":              "relationships",
	"summarization":        "summary",
	"summary_text":         "summary",
	"abstract":             "summary",
	"entity_names":         "entities",
	"named_entities":       "entities",
	"entities_with_types":  "entities",
	"entity_relationships": "", // sentinel: correct name, never reported
}

// entityNameKeys / entityTypeKeys are the near-miss key names accepted INSIDE an
// entity object. Accepting them keeps the entity (coverage is the scarce thing);
// naming them keeps it from being a silent schema fork.
var entityNameKeys = []string{"entity_name", "entity", "label", "value", "text"}
var entityTypeKeys = []string{"entity_type", "kind", "category"}

func (r enrichmentReport) hint() string {
	var parts []string
	if r.malformed > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d entity item(s) carried no usable name and were skipped — 'entities' accepts a bare string name (\"PostgreSQL\") or an object {\"name\":\"...\",\"type\":\"...\"}.",
			r.malformed))
	}
	if n := len(r.unresolvedNames); n > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d entity name(s) are not yet known to this vault and were stored with type \"other\" (%s). Pass {\"name\":\"...\",\"type\":\"...\"} to type them.",
			n, joinCapped(r.unresolvedNames, 5)))
	}
	if n := len(r.coercedTypes); n > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d entity type(s) are not recognised and were stored as \"other\" (%s). Recognised: person, organization, location, concept, technology, project, tool, database, service, framework, language, product, event, other.",
			n, joinCapped(r.coercedTypes, 5)))
	}
	if r.markup > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d entity(ies) were read from [[...]] markup in the content; the brackets were removed from the stored text.",
			r.markup))
	}
	if n := len(r.renamedKeys); n > 0 {
		parts = append(parts, fmt.Sprintf(
			"%s: not part of the entities schema — the accepted keys are 'name' and 'type'. The value was used anyway.",
			joinCapped(r.renamedKeys, 5)))
	}
	if n := len(r.unknownArgs); n > 0 {
		parts = append(parts, fmt.Sprintf(
			"Ignored unknown parameter(s) %s — did you mean %s?",
			joinCapped(r.unknownArgs, 5), joinCapped(r.unknownArgSuggestions(), 5)))
	}
	return strings.Join(parts, " ")
}

func (r enrichmentReport) unknownArgSuggestions() []string {
	out := make([]string, 0, len(r.unknownArgs))
	for _, a := range r.unknownArgs {
		if want := nearMissArgs[a]; want != "" {
			out = append(out, "'"+want+"'")
		}
	}
	return out
}

func joinCapped(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(", … (+%d more)", len(items)-max)
}

// applyEnrichmentArgs is the no-resolver form, kept for call sites that have no
// vault context. Bare names still work; they simply all resolve to "other".
func applyEnrichmentArgs(args map[string]any, req *mbp.WriteRequest) int {
	return parseEnrichmentArgs(args, req, nil).malformed
}

// parseEnrichmentArgs parses optional inline enrichment fields (summary, entities,
// relationships) from MCP tool call arguments onto the WriteRequest, resolving
// untyped entity names against the vault's own entity table and lifting any
// [[markup]] entities out of req.Content.
func parseEnrichmentArgs(args map[string]any, req *mbp.WriteRequest, lookup entityTypeLookup) enrichmentReport {
	var rep enrichmentReport
	if summary, ok := args["summary"].(string); ok && summary != "" {
		req.Summary = summary
	}
	for k := range args {
		if want, isNearMiss := nearMissArgs[k]; isNearMiss && want != "" {
			if _, correctPresent := args[want]; !correctPresent {
				rep.unknownArgs = append(rep.unknownArgs, k)
			}
		}
	}
	sort.Strings(rep.unknownArgs)

	seen := make(map[string]bool)
	add := func(name, declaredType string) {
		name = strings.TrimSpace(norm.NFKC.String(name))
		if name == "" {
			rep.malformed++
			return
		}
		if len(req.Entities) >= maxInlineEntities || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		typ := resolveEntityType(name, declaredType, lookup, &rep)
		req.Entities = append(req.Entities, mbp.InlineEntity{Name: name, Type: typ})
	}

	if entitiesAny, ok := args["entities"].([]any); ok {
		for i, eAny := range entitiesAny {
			if i >= maxInlineEntities {
				break
			}
			switch e := eAny.(type) {
			case string:
				// The middle gear: a bare name. ~15 tokens for a whole entity
				// set instead of typed JSON objects.
				add(e, "")
			case map[string]any:
				name, _ := e["name"].(string)
				typ, _ := e["type"].(string)
				if strings.TrimSpace(name) == "" {
					name, rep.renamedKeys = pickAlt(e, entityNameKeys, rep.renamedKeys)
				}
				if strings.TrimSpace(typ) == "" {
					typ, rep.renamedKeys = pickAlt(e, entityTypeKeys, rep.renamedKeys)
				}
				add(name, typ)
			default:
				rep.malformed++
			}
		}
	}

	// Inline markup last, so an explicitly-declared spelling/type always wins
	// over the same name written in brackets.
	if stripped, names := extractMarkupEntities(req.Content); len(names) > 0 {
		req.Content = stripped
		for _, n := range names {
			add(n, "")
		}
		// Counted from the markup found, not from what survived dedup: the
		// content was rewritten either way, and that is what must be reported.
		rep.markup = len(names)
	}

	if relsAny, ok := args["relationships"].([]any); ok {
		for i, rAny := range relsAny {
			if i >= 30 {
				break
			}
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
	if erAny, ok := args["entity_relationships"].([]any); ok {
		for i, eAny := range erAny {
			if i >= 30 {
				break
			}
			eMap, ok := eAny.(map[string]any)
			if !ok {
				continue
			}
			fromEntity, _ := eMap["from_entity"].(string)
			toEntity, _ := eMap["to_entity"].(string)
			relType, _ := eMap["rel_type"].(string)
			if fromEntity == "" || toEntity == "" || relType == "" {
				continue
			}
			weight := float32(0.9)
			if w, ok := eMap["weight"].(float64); ok {
				if w < 0 {
					w = 0
				} else if w > 1 {
					w = 1
				}
				weight = float32(w)
			}
			req.EntityRelationships = append(req.EntityRelationships, mbp.InlineEntityRelationship{
				FromEntity: fromEntity,
				ToEntity:   toEntity,
				RelType:    relType,
				Weight:     weight,
			})
		}
	}
	return rep
}

// resolveEntityType decides the type for one caller-supplied entity name.
//
// A DECLARED type always wins (principle #1: explicit config is never silently
// substituted) — it is only lowercased, and coerced to "other" if it is outside
// the 14, which is counted. An OMITTED type is resolved from the vault's own
// entity table; a name the vault has never seen becomes "other" and is counted.
// Nothing here reads the content, so no name can be manufactured.
func resolveEntityType(name, declared string, lookup entityTypeLookup, rep *enrichmentReport) string {
	if d := strings.TrimSpace(declared); d != "" {
		typ := normalizeEntityType(d)
		if typ == "other" && strings.ToLower(d) != "other" {
			rep.coercedTypes = append(rep.coercedTypes, d)
		}
		return typ
	}
	if lookup != nil {
		if typ, known := lookup(name); known {
			if norm := normalizeEntityType(typ); norm != "" {
				return norm
			}
		}
	}
	rep.unresolvedNames = append(rep.unresolvedNames, name)
	return "other"
}

// pickAlt pulls a value out of an entity object under a near-miss key, recording
// which wrong key was used so the caller learns the real one.
func pickAlt(obj map[string]any, keys []string, seen []string) (string, []string) {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return v, append(seen, "'"+k+"'")
		}
	}
	return "", seen
}

// extractMarkupEntities strips [[Entity Name]] markup out of content and returns
// the cleaned text plus the bracketed names, in order.
//
// This is a TOKENIZER, not a model — it reads only what the caller explicitly
// bracketed. It deliberately refuses anything that does not look like a
// declaration: nested or unbalanced brackets, embedded newlines, and names
// longer than maxMarkupNameLen are left byte-identical, so ordinary code and
// prose containing brackets pass through untouched.
//
// Returns ("", nil) when there is nothing to do, so the caller can leave
// req.Content alone rather than reassign an identical string.
func extractMarkupEntities(content string) (string, []string) {
	if !strings.Contains(content, "[[") {
		return "", nil
	}
	var (
		b     strings.Builder
		names []string
		i     int
	)
	for i < len(content) {
		open := strings.Index(content[i:], "[[")
		if open < 0 {
			break
		}
		open += i
		inner := open + 2
		end := strings.Index(content[inner:], "]]")
		if end < 0 {
			break
		}
		end += inner
		name := content[inner:end]
		if !plausibleMarkupName(name) {
			// Not a declaration — emit the opening bracket pair verbatim and
			// keep scanning after it.
			b.WriteString(content[i : open+2])
			i = open + 2
			continue
		}
		b.WriteString(content[i:open])
		b.WriteString(name)
		names = append(names, strings.TrimSpace(name))
		i = end + 2
		if len(names) >= maxInlineEntities {
			break
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	b.WriteString(content[i:])
	return b.String(), names
}

// plausibleMarkupName gates what [[...]] is allowed to claim is an entity.
func plausibleMarkupName(s string) bool {
	if t := strings.TrimSpace(s); t == "" || len(t) > maxMarkupNameLen {
		return false
	}
	return !strings.ContainsAny(s, "[]\n\r")
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
//
// Prefer relTypeFromStringChecked on any path that can report back to the
// caller: this variant cannot tell them their relation was discarded.
func relTypeFromString(rel string) uint16 {
	v, _ := relTypeFromStringChecked(rel)
	return v
}

// relTypeFromStringChecked is relTypeFromString plus the name of the relation
// when it was NOT recognised, so the caller can be told rather than silently
// handed an inert edge.
//
// An unrecognised relation still resolves to relates_to — storage behaviour is
// unchanged and no link is rejected — but relates_to is the one relation with no
// downstream consumer, so the coercion does not file the declaration in a
// different bucket, it DELETES it. link(contradicts) mistyped as
// "contradicted_by" produces no contradiction flag, no confidence update and no
// adversarial-profile boost; a mistyped supersedes produces no ValidUntil stamp
// and no chain demotion, leaving the stale fact leading recall.
//
// Empty is not reported: handleLink rejects a missing relation upstream, and the
// engine's inline-relationship paths treat "" as "unspecified".
func relTypeFromStringChecked(rel string) (relType uint16, unknown string) {
	if v, ok := relTypeMap[rel]; ok {
		return uint16(v), ""
	}
	if rel != "" {
		unknown = rel
	}
	return uint16(storage.RelRelatesTo), unknown
}

// relationNames returns the recognised relation names in sorted order, so the
// hint text is deterministic and adding a RelType cannot silently omit it.
func relationNames() []string {
	names := make([]string, 0, len(relTypeMap))
	for k := range relTypeMap {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// unknownRelationHint renders the loud-but-graceful notice for a relation that
// was not recognised. Empty rejected value → empty hint.
//
// Declarations are the scarcest resource in the system: measured on a real
// 4,216-engram corpus (aggregate counts only), just 0.135% of association edges
// were authored by an agent rather than by the Hebbian or cosine workers, and a
// counterfactual replay recovered 0 of 114 declared supersessions from content
// alone. Discarding one silently discards information nothing else can rebuild.
func unknownRelationHint(rejected string) string {
	if rejected == "" {
		return ""
	}
	return fmt.Sprintf(
		"Note: relation %q is not recognised, so this link was stored as \"relates_to\", which carries no "+
			"meaning for recall — the relationship you intended was not recorded. Re-link with one of: %s.",
		rejected, strings.Join(relationNames(), ", "))
}

// relTypeReverseMap is the inverse of relTypeMap, built once at package init.
// Used by relTypeToString for O(1) deterministic lookup.
var relTypeReverseMap = func() map[storage.RelType]string {
	m := make(map[storage.RelType]string, len(relTypeMap))
	for s, v := range relTypeMap {
		m[v] = s
	}
	return m
}()

// relTypeToString converts a storage.RelType to its canonical string name.
// Returns "" for unknown or zero-value types (e.g. synthetic entity-hop edges).
func relTypeToString(r storage.RelType) string {
	if s, ok := relTypeReverseMap[r]; ok {
		return s
	}
	return ""
}

func (s *MCPServer) handleSimilarEntities(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	if vault == "" {
		sendError(w, id, -32602, "invalid params: 'vault' is required")
		return
	}
	threshold := 0.85
	if v, ok := args["threshold"].(float64); ok {
		if v < 0 || v > 1 {
			sendError(w, id, -32602, "invalid params: 'threshold' must be between 0.0 and 1.0")
			return
		}
		threshold = v
	}
	topN := 20
	if v, ok := args["top_n"].(float64); ok {
		if v < 0 {
			v = 0
		}
		topN = int(v)
	}
	if topN < 1 {
		topN = 1
	}
	if topN > 100 {
		topN = 100
	}

	pairs, err := s.engine.FindSimilarEntities(ctx, vault, threshold, topN)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	type similarPair struct {
		EntityA    string  `json:"entity_a"`
		EntityB    string  `json:"entity_b"`
		Similarity float64 `json:"similarity"`
	}
	out := make([]similarPair, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, similarPair{
			EntityA:    p.EntityA,
			EntityB:    p.EntityB,
			Similarity: p.Similarity,
		})
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"similar": out,
		"count":   len(out),
	})))
}

func (s *MCPServer) handleMergeEntity(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	if vault == "" {
		sendError(w, id, -32602, "invalid params: 'vault' is required")
		return
	}
	entityA, ok1 := args["entity_a"].(string)
	entityB, ok2 := args["entity_b"].(string)
	if !ok1 || entityA == "" || !ok2 || entityB == "" {
		sendError(w, id, -32602, "invalid params: 'entity_a' and 'entity_b' are required")
		return
	}
	dryRun, _ := args["dry_run"].(bool)

	result, err := s.engine.MergeEntity(ctx, vault, entityA, entityB, dryRun)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"merged":           !dryRun,
		"entity_a":         result.EntityA,
		"entity_b":         result.EntityB,
		"engrams_relinked": result.EngramsRelinked,
		"dry_run":          result.DryRun,
	})))
}

func (s *MCPServer) handleProvenance(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "id is required")
		return
	}
	entries, err := s.engine.GetProvenance(ctx, vault, engramID)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if entries == nil {
		entries = []ProvenanceEntry{}
	}
	sendResult(w, id, textContent(mustJSON(&ProvenanceResult{ID: engramID, Entries: entries})))
}

func (s *MCPServer) handleGetEnrichmentCandidates(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	if vault == "" {
		sendError(w, id, -32602, "invalid params: 'vault' is required")
		return
	}
	stages, errMsg := parseStageArgs(args)
	if errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	}
	limit := 50
	if v, ok := args["limit"].(float64); ok {
		if v < 0 {
			v = 0
		}
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	cursor, _ := args["cursor"].(string) // optional; "" means start from beginning
	if cursor != "" {
		if _, err := storage.ParseULID(cursor); err != nil {
			sendError(w, id, -32602, "invalid params: cursor is not a valid ULID")
			return
		}
	}
	result, err := s.engine.GetEnrichmentCandidates(ctx, vault, stages, cursor, limit)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleApplyEnrichment(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	if vault == "" {
		sendError(w, id, -32602, "invalid params: 'vault' is required")
		return
	}
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	expectedUpdatedAt, ok := args["expected_updated_at"].(string)
	if !ok || expectedUpdatedAt == "" {
		sendError(w, id, -32602, "invalid params: 'expected_updated_at' is required")
		return
	}
	stages, errMsg := parseStageArgsFromKey(args, "stages_completed")
	if errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	}

	req := &ApplyEnrichmentRequest{
		ID:                engramID,
		ExpectedUpdatedAt: expectedUpdatedAt,
		Summary:           stringArg(args, "summary"),
		MemoryType:        stringArg(args, "memory_type"),
		TypeLabel:         stringArg(args, "type_label"),
		StagesCompleted:   stages,
		Source:            stringArg(args, "source"),
	}
	if entitiesAny, ok := args["entities"].([]any); ok {
		req.Entities = make([]ApplyEnrichmentEntity, 0, len(entitiesAny))
		for i, raw := range entitiesAny {
			m, ok := raw.(map[string]any)
			if !ok {
				sendError(w, id, -32602, fmt.Sprintf("invalid params: entities[%d] must be an object", i))
				return
			}
			name, _ := m["name"].(string)
			etype, _ := m["type"].(string)
			if name == "" || strings.TrimSpace(etype) == "" {
				sendError(w, id, -32602, fmt.Sprintf("invalid params: entities[%d] requires non-empty 'name' and 'type'", i))
				return
			}
			// Normalise + coerce unknown types to "other" so apply_enrichment
			// matches muninn_remember instead of storing the type verbatim (#501).
			etype = normalizeEntityType(etype)
			entity := ApplyEnrichmentEntity{Name: name, Type: etype}
			if v, ok := m["confidence"].(float64); ok {
				entity.Confidence = float32(v)
			}
			req.Entities = append(req.Entities, entity)
		}
	}
	if relsAny, ok := args["relationships"].([]any); ok {
		req.Relationships = make([]ApplyEnrichmentRelationship, 0, len(relsAny))
		for i, raw := range relsAny {
			m, ok := raw.(map[string]any)
			if !ok {
				sendError(w, id, -32602, fmt.Sprintf("invalid params: relationships[%d] must be an object", i))
				return
			}
			fromEntity, _ := m["from_entity"].(string)
			toEntity, _ := m["to_entity"].(string)
			relType, _ := m["rel_type"].(string)
			if fromEntity == "" || toEntity == "" || relType == "" {
				sendError(w, id, -32602, fmt.Sprintf("invalid params: relationships[%d] requires non-empty 'from_entity', 'to_entity', and 'rel_type'", i))
				return
			}
			rel := ApplyEnrichmentRelationship{FromEntity: fromEntity, ToEntity: toEntity, RelType: relType}
			if v, ok := m["weight"].(float64); ok {
				rel.Weight = float32(v)
			}
			req.Relationships = append(req.Relationships, rel)
		}
	}

	result, err := s.engine.ApplyEnrichment(ctx, vault, req)
	if err != nil {
		if errors.Is(err, engine.ErrEnrichmentConflict) {
			sendError(w, id, -32009, "tool conflict: "+err.Error())
			return
		}
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

func (s *MCPServer) handleReplayEnrichment(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	if vault == "" {
		sendError(w, id, -32602, "invalid params: 'vault' is required")
		return
	}

	stages, errMsg := parseStageArgs(args)
	if errMsg != "" {
		sendError(w, id, -32602, errMsg)
		return
	}

	// Parse limit (optional, default 50, max 200).
	limit := 50
	if v, ok := args["limit"].(float64); ok {
		if v < 0 {
			v = 0
		}
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}

	// Parse dry_run (optional, default false).
	dryRun, _ := args["dry_run"].(bool)

	result, err := s.engine.ReplayEnrichment(ctx, vault, stages, limit, dryRun)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}

	sendResult(w, id, textContent(mustJSON(map[string]any{
		"processed":  result.Processed,
		"skipped":    result.Skipped,
		"failed":     result.Failed,
		"remaining":  result.Remaining,
		"stages_run": result.StagesRun,
		"dry_run":    result.DryRun,
	})))
}

func parseStageArgs(args map[string]any) ([]string, string) {
	return parseStageArgsFromKey(args, "stages")
}

func parseStageArgsFromKey(args map[string]any, key string) ([]string, string) {
	rawStages, ok := args[key]
	if !ok {
		return nil, ""
	}
	stagesAny, ok := rawStages.([]any)
	if !ok {
		return nil, fmt.Sprintf("invalid params: '%s' must be an array of strings", key)
	}
	stages := make([]string, 0, len(stagesAny))
	for i, v := range stagesAny {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Sprintf("invalid params: %s[%d] must be a non-empty string", key, i)
		}
		stages = append(stages, s)
	}
	return stages, ""
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func (s *MCPServer) handleFeedback(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["engram_id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "engram_id is required")
		return
	}
	useful, _ := args["useful"].(bool)
	if err := s.engine.RecordFeedback(ctx, vault, engramID, useful); err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{"ok": true, "engram_id": engramID, "useful": useful})))
}

func (s *MCPServer) handleEntity(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	name, _ := args["name"].(string)
	if name == "" {
		sendError(w, id, -32602, "name is required")
		return
	}
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	agg, err := s.engine.GetEntityAggregate(ctx, vault, name, limit)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	if agg == nil {
		sendError(w, id, -32602, "entity not found: "+name)
		return
	}
	sendResult(w, id, textContent(mustJSON(agg)))
}

func (s *MCPServer) handleEntities(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	limit := 50
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	state, _ := args["state"].(string)
	summaries, err := s.engine.ListEntities(ctx, vault, limit, state)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{"entities": summaries, "count": len(summaries)})))
}

func (s *MCPServer) handleEntityTimeline(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	entityName, ok := args["entity_name"].(string)
	if !ok || entityName == "" {
		sendError(w, id, -32602, "invalid params: 'entity_name' is required")
		return
	}
	limit := 10
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	timeline, err := s.engine.GetEntityTimeline(ctx, vault, entityName, limit)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(timeline)))
}

// buildAnnotations constructs a MemoryAnnotations from engine annotation data
// and the activation item. Staleness is derived from item.LastAccess (nanoseconds
// Unix timestamp).
// augmentAnnotations fills the annotate=true fields (staleness, conflicts,
// provenance) onto a Memory, preserving any always-on supersession annotation
// (superseded_by/current_version) that the ranking phase already attached. The
// supersession fields are authoritative from the ranking; data.SupersededBy from
// the reverse-edge lookup only fills in when the ranking didn't set it.
func augmentAnnotations(m *Memory, item *mbp.ActivationItem, data *engine.AnnotationData) {
	if m.Annotations == nil {
		m.Annotations = &MemoryAnnotations{}
	}
	ann := m.Annotations
	staleDays := math.Round(time.Since(time.Unix(0, item.LastAccess)).Hours()/24.0*10) / 10
	ann.Stale = staleDays > annotationStaleDays
	ann.StaleDays = staleDays
	ann.ConflictsWith = data.ConflictsWith
	if ann.SupersededBy == "" {
		ann.SupersededBy = data.SupersededBy
	}
	if data.LastVerified != nil {
		ann.LastVerified = data.LastVerified.UTC().Format(time.RFC3339)
	}
}

func (s *MCPServer) handleSetTrust(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	trustStr, ok := args["trust"].(string)
	if !ok || trustStr == "" {
		sendError(w, id, -32602, "invalid params: 'trust' is required (one of: verified, inferred, external, untrusted)")
		return
	}
	if _, err := storage.ParseTrustLevel(trustStr); err != nil {
		sendError(w, id, -32602, "invalid params: "+err.Error())
		return
	}
	if err := s.engine.SetTrust(ctx, vault, engramID, trustStr); err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":    engramID,
		"trust": trustStr,
		"ok":    true,
	})))
}

func (s *MCPServer) handleCompareAndSet(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	var expectState, setState *string
	if v, ok := args["expect_state"].(string); ok && v != "" {
		expectState = &v
	}
	if v, ok := args["set_state"].(string); ok && v != "" {
		setState = &v
	}
	if setState == nil {
		sendError(w, id, -32602, "invalid params: 'set_state' is required")
		return
	}
	applied, state, owner, err := s.engine.CompareAndSet(ctx, vault, engramID, expectState, setState)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":      engramID,
		"applied": applied,
		"current": map[string]any{"state": state, "owner": owner},
	})))
}

func (s *MCPServer) handleClaim(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	owner, ok := args["owner"].(string)
	if !ok || owner == "" {
		sendError(w, id, -32602, "invalid params: 'owner' is required")
		return
	}
	ttlFloat, ok := args["ttl_secs"].(float64)
	if !ok || ttlFloat <= 0 {
		sendError(w, id, -32602, "invalid params: 'ttl_secs' is required and must be a positive number")
		return
	}
	status, curOwner, heartbeat, err := s.engine.Claim(ctx, vault, engramID, owner, int64(ttlFloat))
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":        engramID,
		"status":    status,
		"owner":     curOwner,
		"heartbeat": heartbeat,
	})))
}

func (s *MCPServer) handleRelease(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	engramID, ok := args["id"].(string)
	if !ok || engramID == "" {
		sendError(w, id, -32602, "invalid params: 'id' is required")
		return
	}
	owner, ok := args["owner"].(string)
	if !ok || owner == "" {
		sendError(w, id, -32602, "invalid params: 'owner' is required")
		return
	}
	released, curOwner, err := s.engine.Release(ctx, vault, engramID, owner)
	if err != nil {
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	sendResult(w, id, textContent(mustJSON(map[string]any{
		"id":       engramID,
		"released": released,
		"owner":    curOwner,
	})))
}

// isWorkflowVaultName reports whether name is a valid workflow-vault identifier:
// it MUST start with the "wf-" namespace prefix and satisfy the general vault
// name format (1-64 chars, lowercase alphanumeric, hyphen, underscore).
// This is the structural anti-clobber guard (RedTeam finding CRITICAL #1):
// it makes muninn_create_workflow_vault incapable of targeting an operator
// vault such as "default" or "production", because those names lack the prefix.
func isWorkflowVaultName(name string) bool {
	const prefix = "wf-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) <= len(prefix) {
		return false // "wf-" alone has no body
	}
	return auth.IsValidVaultName(name)
}

// handleCreateWorkflowVault implements muninn_create_workflow_vault (RFC #597).
// It creates a shared working vault and mints a scoped, TTL'd cap_ capability
// token for worker agents. The tool is privileged: dispatchToolCall's recursion
// guard verifies an mk_ full-mode key BEFORE this handler runs, so capabilities
// (IsCapability, not IsAPIKey) can never reach it — structural recursion
// prevention. The capability_secret is shown ONCE.
func (s *MCPServer) handleCreateWorkflowVault(ctx context.Context, w http.ResponseWriter, id json.RawMessage, _ string, args map[string]any) {
	if s.authStore == nil {
		sendError(w, id, -32603, "vault creation unavailable: auth store not configured on this server")
		return
	}

	name, _ := args["name"].(string)
	if name == "" {
		rb := make([]byte, 4)
		if _, err := rand.Read(rb); err != nil {
			sendError(w, id, -32603, "generate vault name: "+err.Error())
			return
		}
		name = "wf-" + hex.EncodeToString(rb)
	} else if !isWorkflowVaultName(name) {
		// Structural anti-clobber guard (RedTeam finding CRITICAL #1): a caller-
		// supplied name MUST be namespaced to workflow vaults (wf-*). This makes
		// the tool structurally incapable of targeting an operator vault
		// (default, production, etc.) — IsValidVaultName alone was format-only
		// and allowed any well-formed name, so SetVaultConfig could overwrite
		// an existing vault's config and mint a cap_ against it.
		sendError(w, id, -32602, "invalid vault name: must start with 'wf-' and be workflow-scoped (1-64 lowercase alphanumeric or hyphen)")
		return
	}

	// Existence check (RedTeam finding CRITICAL #1): reject if the vault is
	// already registered. RegisterVaultName is idempotent and SetVaultConfig is
	// a destructive overwrite, so without this gate a second call against an
	// existing wf-* vault would silently clobber its config + mint a fresh cap.
	// Auto-generated names should not collide in practice (4 bytes of entropy)
	// but are checked anyway — cheap and fails-closed.
	if s.engine.VaultNameExists(name) {
		sendError(w, id, -32602, "vault already exists: "+name)
		return
	}

	label, _ := args["label"].(string)
	if label == "" {
		label = "agent-minted"
	}

	// TTL floor + cap (RedTeam finding NOTABLE #4): floor sub-hour fractions
	// (0.5h → int(0)=0 → born-expired) to 1h; cap at 168h (7 days, the working
	// preset retention) — minting a cap that outlives the vault's data is
	// pointless and the prior 24*365 ceiling contradicted the documented 168h
	// default.
	const ttlCeiling = 168
	ttlHours := ttlCeiling
	if v, ok := args["ttl_hours"].(float64); ok && v > 0 {
		ttlHours = int(v) // JSON numbers arrive as float64
		if ttlHours < 1 {
			ttlHours = 1 // floor: reject sub-hour truncation to zero
		}
		if ttlHours > ttlCeiling {
			ttlHours = ttlCeiling // cap: don't outlive the vault's 7-day retention
		}
	}
	if ev := os.Getenv("MUNINN_WORKFLOW_CAP_TTL_HOURS"); ev != "" {
		if n, err := strconv.Atoi(ev); err == nil && n > 0 && n < ttlHours {
			ttlHours = n // env is a fleet-wide ceiling; a smaller caller ttl_hours is honored
		}
	}

	// 1. Register vault name (idempotent 2-key write in the engine's vault registry).
	if err := s.engine.RegisterVaultName(name); err != nil {
		sendError(w, id, -32603, "register vault: "+err.Error())
		return
	}

	// 2. Configure the vault: working preset (default cognition + 7-day
	// auto-evaporation) + multi_user (guidance steers toward per-user recall).
	mu := true
	if err := s.authStore.SetVaultConfig(auth.VaultConfig{
		Name:       name,
		Plasticity: &auth.PlasticityConfig{Preset: "working", MultiUser: &mu},
	}); err != nil {
		sendError(w, id, -32603, "set vault config: "+err.Error())
		return
	}

	// 3. Mint a full-mode capability with the TTL. The token is shown once.
	expiresAt := time.Now().Add(time.Duration(ttlHours) * time.Hour)
	token, cap, err := s.authStore.GenerateCapability(name, label, auth.ModeFull, "workflow_vault", &expiresAt)
	if err != nil {
		sendError(w, id, -32603, "mint capability: "+err.Error())
		return
	}

	sendResult(w, id, map[string]any{
		"vault":             name,
		"capability_id":     cap.ID,
		"capability_secret": token, // shown once
		"mode":              auth.ModeFull,
		"expires_at":        expiresAt.Format(time.RFC3339),
		"auto_evap_days":    7,
		"warning":           "capability_secret is shown once; distribute it to worker agents. The vault auto-evaporates engrams after 7 days.",
	})
}
