package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// === THE PUSH increment 1: MCP wiring ======================================
//
// RED-first: written before the muninn_intend handler and the notices wiring
// existed (failed to compile / dispatch returned "unknown tool").

// prospectiveFake embeds fakeEngine and implements the prospectiveCapable
// surface, recording every call for assertion.
type prospectiveFake struct {
	fakeEngine

	intendID   string
	intendErr  error
	lastIntend struct {
		vault, content string
		cues           []string
		oneShot        bool
		validUntil     *time.Time
		importance     *float32
	}

	notices    []engine.Notice
	noticesErr error

	recallCalled       bool
	lastRecallResults  []engine.ScoredResult
	lastRecallReadOnly bool

	rememberCalled      bool
	lastRememberFocal   []string
	lastRememberCreated string

	lastSessionSeen func(string) bool

	activations []mbp.ActivationItem
}

func (p *prospectiveFake) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return &mbp.ActivateResponse{Activations: p.activations, TotalFound: len(p.activations)}, nil
}

func (p *prospectiveFake) Intend(ctx context.Context, vault, content string, cues []string, validUntil *time.Time, oneShot bool, importance *float32) (string, error) {
	p.lastIntend.vault, p.lastIntend.content = vault, content
	p.lastIntend.cues, p.lastIntend.oneShot = cues, oneShot
	p.lastIntend.validUntil, p.lastIntend.importance = validUntil, importance
	if p.intendErr != nil {
		return "", p.intendErr
	}
	if p.intendID == "" {
		return "01ARZ3NDEKTSV4RRFFQ69G5FAV", nil
	}
	return p.intendID, nil
}

func (p *prospectiveFake) NoticesForRecall(ctx context.Context, vault string, results []engine.ScoredResult, sessionSeen func(string) bool, readOnly bool) ([]engine.Notice, error) {
	p.recallCalled = true
	p.lastRecallResults = results
	p.lastRecallReadOnly = readOnly
	p.lastSessionSeen = sessionSeen
	return p.notices, p.noticesErr
}

func (p *prospectiveFake) NoticesForRemember(ctx context.Context, vault string, focal []string, createdID string, sessionSeen func(string) bool) ([]engine.Notice, error) {
	p.rememberCalled = true
	p.lastRememberFocal = focal
	p.lastRememberCreated = createdID
	p.lastSessionSeen = sessionSeen
	return p.notices, p.noticesErr
}

func newProspectiveServer(pf *prospectiveFake, enabled bool) *MCPServer {
	s := New(":0", pf, "", nil, nil, nil)
	s.prospective = enabled
	return s
}

func postRPCWithSession(t *testing.T, srv *MCPServer, body, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	return w
}

func rpcText(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	return string(b)
}

func TestIntendTool_Classification(t *testing.T) {
	if !isMutatingTool("muninn_intend") {
		t.Error("muninn_intend must be classified as mutating (it writes an engram and arms 0x2D keys)")
	}
	if isReadOnlyTool("muninn_intend") || isAdditiveTool("muninn_intend") {
		t.Error("muninn_intend must be mutating-only: not read-only, not append-additive")
	}
}

func TestHandleIntend_Validation(t *testing.T) {
	pf := &prospectiveFake{}
	srv := newProspectiveServer(pf, true)

	cases := []struct {
		name string
		args string
	}{
		{"missing content", `{"vault":"default","cues":["redis"]}`},
		{"missing cues", `{"vault":"default","content":"remind me"}`},
		{"empty cues", `{"vault":"default","content":"remind me","cues":[]}`},
		{"bad valid_until", `{"vault":"default","content":"remind me","cues":["redis"],"valid_until":"not-a-date"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_intend","arguments":%s}}`, tc.args)
			w := postRPC(t, srv, body)
			var resp JSONRPCResponse
			json.NewDecoder(w.Body).Decode(&resp)
			if resp.Error == nil || resp.Error.Code != -32602 {
				t.Errorf("%s: error = %+v, want -32602", tc.name, resp.Error)
			}
		})
	}
}

func TestHandleIntend_Success(t *testing.T) {
	pf := &prospectiveFake{intendID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_intend","arguments":{"vault":"default","content":"raise the eviction bug","cues":["redis","cache-layer"],"one_shot":false,"importance":0.8,"valid_until":"2099-01-01T00:00:00Z"}}}`
	w := postRPC(t, srv, body)
	text := rpcText(t, w)
	if !strings.Contains(text, "01ARZ3NDEKTSV4RRFFQ69G5FAV") {
		t.Errorf("result must carry the intention id; got %s", text)
	}
	if pf.lastIntend.content != "raise the eviction bug" || len(pf.lastIntend.cues) != 2 || pf.lastIntend.oneShot {
		t.Errorf("Intend args not threaded: %+v", pf.lastIntend)
	}
	if pf.lastIntend.importance == nil || *pf.lastIntend.importance != 0.8 {
		t.Errorf("importance not threaded: %v", pf.lastIntend.importance)
	}
	if pf.lastIntend.validUntil == nil {
		t.Errorf("valid_until not threaded")
	}
}

func TestHandleIntend_InvalidIntentionMapsToInvalidParams(t *testing.T) {
	pf := &prospectiveFake{intendErr: fmt.Errorf("%w: cue \"muninndb\" is too ubiquitous", engine.ErrInvalidIntention)}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_intend","arguments":{"vault":"default","content":"x","cues":["muninndb"]}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("error = %+v, want -32602 for ErrInvalidIntention", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "muninndb") {
		t.Errorf("rejection must surface the engine's cue-naming message; got %q", resp.Error.Message)
	}
}

func TestHandleIntend_UnsupportedEngine(t *testing.T) {
	// A plain fakeEngine does not implement prospectiveCapable.
	srv := New(":0", &fakeEngine{}, "", nil, nil, nil)
	srv.prospective = true
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_intend","arguments":{"vault":"default","content":"x","cues":["redis"]}}}`
	w := postRPC(t, srv, body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("error = %+v, want -32000 when the engine lacks prospective support", resp.Error)
	}
}

func TestRecallNotices_AttachedWhenEnabled(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9, Concept: "c", Content: "x"}},
		notices: []engine.Notice{{
			Kind: "intention", MemoryID: "01BX5ZZKBKACTAV9WEVGEMMVRZ",
			Note: "raise the eviction bug", Cue: "redis",
			Why: `armed intention: cue entity "redis" is focal`, DedupKey: "01BX5ZZKBKACTAV9WEVGEMMVRZ",
		}},
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis eviction"]}}}`
	w := postRPC(t, srv, body)
	text := rpcText(t, w)
	if !strings.Contains(text, `\"notices\"`) || !strings.Contains(text, "raise the eviction bug") {
		t.Errorf("notices not attached to recall response: %s", text)
	}
	if len(pf.lastRecallResults) != 1 || pf.lastRecallResults[0].ID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
		t.Errorf("returned result IDs not threaded: %+v", pf.lastRecallResults)
	}
	if pf.lastRecallReadOnly {
		t.Errorf("readOnly = true for a plain recall, want false")
	}
}

func TestRecallNotices_OmittedWhenDisabled(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9}},
		notices:     []engine.Notice{{Kind: "intention", MemoryID: "x", Note: "n", DedupKey: "x"}},
	}
	srv := newProspectiveServer(pf, false) // MUNINN_PROSPECTIVE unset: inert
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis eviction"]}}}`
	w := postRPC(t, srv, body)
	text := rpcText(t, w)
	if strings.Contains(text, `\"notices\"`) {
		t.Errorf("notices attached with the mechanism disabled: %s", text)
	}
	if pf.recallCalled {
		t.Errorf("NoticesForRecall consulted with the mechanism disabled (must be fully inert)")
	}
}

func TestRecallNotices_OmittedWhenEmpty(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9}},
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis eviction"]}}}`
	w := postRPC(t, srv, body)
	text := rpcText(t, w)
	if strings.Contains(text, `\"notices\"`) {
		t.Errorf("empty notices must be OMITTED (zero token cost on the empty path): %s", text)
	}
}

func TestRecallNotices_ReadOnlyThreaded(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9}},
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis"],"read_only":true}}}`
	postRPC(t, srv, body)
	if !pf.recallCalled || !pf.lastRecallReadOnly {
		t.Errorf("read_only=true not threaded to NoticesForRecall (called=%v readOnly=%v)", pf.recallCalled, pf.lastRecallReadOnly)
	}
}

func TestRecallNotices_ErrorDegradesGracefully(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9}},
		noticesErr:  fmt.Errorf("boom"),
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis"]}}}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (notice failure must not fail recall)", w.Code)
	}
	text := rpcText(t, w) // rpcText fails the test on an RPC error
	if strings.Contains(text, `\"notices\"`) {
		t.Errorf("failed notices computation must attach nothing: %s", text)
	}
}

func TestRememberNotices_FocalFromInlineEntities(t *testing.T) {
	pf := &prospectiveFake{
		notices: []engine.Notice{{Kind: "intention", MemoryID: "01BX5ZZKBKACTAV9WEVGEMMVRZ", Note: "n", Cue: "redis", DedupKey: "01BX5ZZKBKACTAV9WEVGEMMVRZ"}},
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"redis lag spiked","entities":[{"name":"redis","type":"technology"}]}}}`
	w := postRPC(t, srv, body)
	text := rpcText(t, w)
	if !strings.Contains(text, `\"notices\"`) {
		t.Errorf("notices not attached to remember response: %s", text)
	}
	if len(pf.lastRememberFocal) != 1 || pf.lastRememberFocal[0] != "redis" {
		t.Errorf("focal = %v, want the caller-supplied inline entities", pf.lastRememberFocal)
	}
	if pf.lastRememberCreated != "fake-id" {
		t.Errorf("createdID = %q, want the created engram id", pf.lastRememberCreated)
	}
}

func TestRememberNotices_NoEntitiesNoConsult(t *testing.T) {
	pf := &prospectiveFake{}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"no entities here"}}}`
	postRPC(t, srv, body)
	if pf.rememberCalled {
		t.Errorf("NoticesForRemember consulted with no inline entities (no focal set exists)")
	}
}

// TestNoticeSessionDedup_Concurrent hammers the shared session-dedup map from
// many goroutines (mark + check across sessions) so the race detector has a
// real interleaving to examine — the map is the one piece of shared mutable
// state THE PUSH adds to the MCP server.
func TestNoticeSessionDedup_Concurrent(t *testing.T) {
	srv := newProspectiveServer(&prospectiveFake{}, true)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			key := fmt.Sprintf("session-%d", g%3)
			seen := srv.noticeSessionSeenFunc(key)
			for i := 0; i < 200; i++ {
				srv.markNoticesDelivered(key, []engine.Notice{{DedupKey: fmt.Sprintf("dk-%d-%d", g, i)}})
				_ = seen(fmt.Sprintf("dk-%d-%d", g, i))
				_ = seen("never-delivered")
			}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	// A key delivered under one session must be visible to that session.
	if !srv.noticeSessionSeenFunc("session-0")("dk-0-0") {
		t.Error("delivered key not visible after concurrent marking")
	}
}

// TestNoticeSessionDedup pins the per-session delivered-notice tracking: a
// notice delivered under one session ID is reported as seen on the next call
// of the SAME session, and unseen under a different session ID.
func TestNoticeSessionDedup(t *testing.T) {
	pf := &prospectiveFake{
		activations: []mbp.ActivationItem{{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Score: 0.9}},
		notices:     []engine.Notice{{Kind: "intention", MemoryID: "01BX5ZZKBKACTAV9WEVGEMMVRZ", Note: "n", DedupKey: "dk-1"}},
	}
	srv := newProspectiveServer(pf, true)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["redis"]}}}`

	postRPCWithSession(t, srv, body, "session-A") // delivers dk-1, marks it
	pf.notices = nil                              // second call: nothing new to deliver

	postRPCWithSession(t, srv, body, "session-A")
	if pf.lastSessionSeen == nil || !pf.lastSessionSeen("dk-1") {
		t.Errorf("session-A second call: sessionSeen(dk-1) = false, want true (delivered last call)")
	}

	postRPCWithSession(t, srv, body, "session-B")
	if pf.lastSessionSeen == nil || pf.lastSessionSeen("dk-1") {
		t.Errorf("session-B: sessionSeen(dk-1) = true, want false (different session)")
	}
}
