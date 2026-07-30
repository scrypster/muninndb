package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// THE PUSH increment 1 — MCP surface for prospective memory.
//
// muninn_intend arms an intention; notices are delivered ONLY as an additive
// `notices` field piggybacked on muninn_recall / muninn_remember responses
// ("push at the next natural exchange" — MCP has no honest interrupt channel,
// and the stillborn EventBus stays dormant). The whole delivery path is inert
// unless the server opts in via MUNINN_PROSPECTIVE=1; muninn_intend itself
// always works (intentions are durable state and survive the flag flipping
// on later).

// prospectiveCapable is the optional engine surface for prospective memory.
// The production adapter (mcpEngineAdapter) implements it; test fakes that
// predate THE PUSH simply don't, and the handler fails loudly for them.
type prospectiveCapable interface {
	Intend(ctx context.Context, vault, content string, cues []string, validUntil *time.Time, oneShot bool, importance *float32) (string, error)
	NoticesForRecall(ctx context.Context, vault string, results []engine.ScoredResult, sessionSeen func(string) bool, readOnly bool) ([]engine.Notice, error)
	NoticesForRemember(ctx context.Context, vault string, focal []string, createdID string, sessionSeen func(string) bool) ([]engine.Notice, error)
}

// --- per-session delivered-notice tracking ---------------------------------

// noticeSessionCtxKey carries the notice-session identity on the request
// context: the Mcp-Session-Id header for Streamable HTTP POSTs, the SSE
// session ID for the SSE transport. Empty falls back to noticeSessionAnon.
type noticeSessionCtxKey struct{}

// noticeSessionAnon is the shared dedup bucket for callers that present no
// session identity at all (bare POSTs without an Mcp-Session-Id header).
// Cross-caller dedup in that bucket is an accepted increment-1 residual —
// better one suppressed re-delivery than a nag loop.
const noticeSessionAnon = "anon"

const (
	maxNoticeSessions       = 1024
	maxNoticeKeysPerSession = 1024
)

func withNoticeSession(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, noticeSessionCtxKey{}, key)
}

func noticeSessionFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(noticeSessionCtxKey{}).(string); ok && v != "" {
		return v
	}
	return noticeSessionAnon
}

// noticeSessionSeenFunc returns the sessionSeen predicate for one session.
func (s *MCPServer) noticeSessionSeenFunc(sessionKey string) func(string) bool {
	return func(dedupKey string) bool {
		s.noticeMu.Lock()
		defer s.noticeMu.Unlock()
		set, ok := s.noticeSeen[sessionKey]
		if !ok {
			return false
		}
		_, seen := set[dedupKey]
		return seen
	}
}

// markNoticesDelivered records delivered notices in the session's seen-set.
// Bounded: at most maxNoticeSessions sessions (arbitrary eviction — dedup is
// best-effort advisory state) and maxNoticeKeysPerSession keys per session.
func (s *MCPServer) markNoticesDelivered(sessionKey string, notices []engine.Notice) {
	if len(notices) == 0 {
		return
	}
	s.noticeMu.Lock()
	defer s.noticeMu.Unlock()
	set, ok := s.noticeSeen[sessionKey]
	if !ok {
		if len(s.noticeSeen) >= maxNoticeSessions {
			for k := range s.noticeSeen { // evict one arbitrary session
				delete(s.noticeSeen, k)
				break
			}
		}
		set = make(map[string]struct{}, 4)
		s.noticeSeen[sessionKey] = set
	}
	for _, n := range notices {
		if len(set) >= maxNoticeKeysPerSession {
			break
		}
		if n.DedupKey != "" {
			set[n.DedupKey] = struct{}{}
		}
	}
}

// --- muninn_intend ----------------------------------------------------------

func (s *MCPServer) handleIntend(ctx context.Context, w http.ResponseWriter, id json.RawMessage, vault string, args map[string]any) {
	pe, ok := s.engine.(prospectiveCapable)
	if !ok {
		sendError(w, id, -32000, "tool error: prospective memory is not supported by this engine")
		return
	}
	content, okC := args["content"].(string)
	if !okC || content == "" {
		sendError(w, id, -32602, "invalid params: 'content' is required (what to surface when a cue becomes focal)")
		return
	}
	cuesAny, okCues := args["cues"].([]any)
	var cues []string
	for _, c := range cuesAny {
		if cs, ok := c.(string); ok && cs != "" {
			cues = append(cues, cs)
		}
	}
	if !okCues || len(cues) == 0 {
		sendError(w, id, -32602, "invalid params: 'cues' is required — at least one entity name to arm the intention on")
		return
	}
	var validUntil *time.Time
	if vuStr, ok := args["valid_until"].(string); ok && vuStr != "" {
		t, err := time.Parse(time.RFC3339, vuStr)
		if err != nil {
			sendError(w, id, -32602, "invalid 'valid_until': must be ISO 8601 (e.g. 2026-09-01T00:00:00Z)")
			return
		}
		validUntil = &t
	}
	oneShot := true // default: fire once, then disarm (the safe attention posture)
	if os, ok := args["one_shot"].(bool); ok {
		oneShot = os
	}
	imp, okImp := parseImportanceArg(args)
	if !okImp {
		sendError(w, id, -32602, "invalid params: 'importance' must be a number in [0,1]")
		return
	}

	intentionID, err := pe.Intend(ctx, vault, content, cues, validUntil, oneShot, imp)
	if err != nil {
		if errors.Is(err, engine.ErrInvalidIntention) {
			sendError(w, id, -32602, "invalid params: "+err.Error())
			return
		}
		sendError(w, id, -32000, "tool error: "+err.Error())
		return
	}
	result := map[string]any{
		"id":       intentionID,
		"cues":     cues,
		"one_shot": oneShot,
	}
	if !s.prospective {
		result["hint"] = "Intention stored and armed, but notice delivery is DISABLED on this server (set MUNINN_PROSPECTIVE=1 to enable notices on recall/remember). The intention is durable and will start firing once the flag is enabled."
	}
	sendResult(w, id, textContent(mustJSON(result)))
}

// --- notice attachment helpers (recall / remember) --------------------------

// recallNotices computes notices for a recall response. Returns nil (and logs)
// on any failure — a notice-path fault must never fail the recall itself.
// Inert unless MUNINN_PROSPECTIVE=1 and the engine supports it.
func (s *MCPServer) recallNotices(ctx context.Context, vault string, activations []mbp.ActivationItem, readOnly bool) []engine.Notice {
	if !s.prospective || len(activations) == 0 {
		return nil
	}
	pe, ok := s.engine.(prospectiveCapable)
	if !ok {
		return nil
	}
	results := make([]engine.ScoredResult, 0, len(activations))
	for _, a := range activations {
		results = append(results, engine.ScoredResult{ID: a.ID, Score: float64(a.Score)})
	}
	sessionKey := noticeSessionFromContext(ctx)
	notices, err := pe.NoticesForRecall(ctx, vault, results, s.noticeSessionSeenFunc(sessionKey), readOnly)
	if err != nil {
		slog.Warn("mcp: notices computation failed on recall (degrading without notices)", "vault", vault, "err", err)
		return nil
	}
	s.markNoticesDelivered(sessionKey, notices)
	return notices
}

// rememberNotices computes notices for a remember response; the focal set is
// the caller-supplied inline entities and createdID is the self-echo guard.
func (s *MCPServer) rememberNotices(ctx context.Context, vault string, entities []mbp.InlineEntity, createdID string) []engine.Notice {
	if !s.prospective || len(entities) == 0 {
		return nil
	}
	pe, ok := s.engine.(prospectiveCapable)
	if !ok {
		return nil
	}
	focal := make([]string, 0, len(entities))
	for _, e := range entities {
		if e.Name != "" {
			focal = append(focal, e.Name)
		}
	}
	if len(focal) == 0 {
		return nil
	}
	sessionKey := noticeSessionFromContext(ctx)
	notices, err := pe.NoticesForRemember(ctx, vault, focal, createdID, s.noticeSessionSeenFunc(sessionKey))
	if err != nil {
		slog.Warn("mcp: notices computation failed on remember (degrading without notices)", "vault", vault, "err", err)
		return nil
	}
	s.markNoticesDelivered(sessionKey, notices)
	return notices
}
