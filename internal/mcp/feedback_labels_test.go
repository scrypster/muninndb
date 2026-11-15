package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Label pipeline (slice 2a) MCP-surface tests. Vault names, tokens and memory
// text are invented (trail maintenance).

// recordingFeedbackEngine records whether — and with what — the engine was
// reached, so a validation test can prove a rejected call never got there.
type recordingFeedbackEngine struct {
	fakeEngine
	legacyCalls int
	labelCalls  int
	lastUseful  []bool
}

func (e *recordingFeedbackEngine) RecordFeedback(_ context.Context, _, _ string, useful bool) error {
	e.legacyCalls++
	e.lastUseful = append(e.lastUseful, useful)
	return nil
}

func (e *recordingFeedbackEngine) RecordFeedbackLabels(_ context.Context, _ string, groups []FeedbackGroup, _ string, _ bool) ([]FeedbackOutcome, error) {
	e.labelCalls++
	for _, g := range groups {
		e.lastUseful = append(e.lastUseful, g.Useful)
	}
	return nil, nil
}

const testULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const testULID2 = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

func feedbackRPC(t *testing.T, srv *MCPServer, args string) JSONRPCResponse {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":` + args + `}}`
	return decodeResp(t, postRPC(t, srv, body).Body.String())
}

// TestFeedback_UsefulRequired: an omitted `useful` is a -32602 in every form,
// and the engine is never reached. (The old handler turned it into false: a
// NEGATIVE label nobody sent.)
func TestFeedback_UsefulRequired(t *testing.T) {
	for name, args := range map[string]string{
		"legacy": `{"engram_id":"` + testULID + `"}`,
		"single": `{"query_id":"` + testULID + `","engram_ids":["` + testULID2 + `"]}`,
		"batch":  `{"groups":[{"query_id":"` + testULID + `","ids":["` + testULID2 + `"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			fe := &recordingFeedbackEngine{}
			resp := feedbackRPC(t, newTestServerWith(fe), args)
			require.NotNil(t, resp.Error, "omitted useful must be an error")
			require.Equal(t, -32602, resp.Error.Code)
			require.Contains(t, resp.Error.Message, "useful is required and must be a JSON boolean")
			require.Zero(t, fe.legacyCalls+fe.labelCalls, "the engine must not be reached")
		})
	}
}

// TestFeedback_UsefulStringTrueRejected: "true" as a string is not a boolean.
// Today it was silently recorded as false.
func TestFeedback_UsefulStringTrueRejected(t *testing.T) {
	for name, args := range map[string]string{
		"legacy": `{"engram_id":"` + testULID + `","useful":"true"}`,
		"single": `{"query_id":"` + testULID + `","engram_ids":["` + testULID2 + `"],"useful":"true"}`,
		"batch":  `{"groups":[{"query_id":"` + testULID + `","ids":["` + testULID2 + `"],"useful":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			fe := &recordingFeedbackEngine{}
			resp := feedbackRPC(t, newTestServerWith(fe), args)
			require.NotNil(t, resp.Error, "a non-boolean useful must be an error, not a negative label (engine got %v)", fe.lastUseful)
			require.Equal(t, -32602, resp.Error.Code)
			require.Zero(t, fe.legacyCalls+fe.labelCalls)
		})
	}
}

// TestFeedback_UsefulErrorShowsExactCall: the error shows the exact call, and
// in the batch form names the group.
func TestFeedback_UsefulErrorShowsExactCall(t *testing.T) {
	resp := feedbackRPC(t, newTestServerWith(&recordingFeedbackEngine{}), `{"query_id":"`+testULID+`","engram_ids":["`+testULID2+`"]}`)
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, `Example: {"query_id":"<from muninn_recall>","engram_ids":["<id>"],"useful":true}`)

	resp = feedbackRPC(t, newTestServerWith(&recordingFeedbackEngine{}),
		`{"groups":[{"query_id":"`+testULID+`","ids":["`+testULID2+`"],"useful":true},{"query_id":"`+testULID+`","ids":["`+testULID2+`"]}]}`)
	require.NotNil(t, resp.Error)
	require.True(t, strings.HasPrefix(resp.Error.Message, "groups[1]: useful is required"), resp.Error.Message)
}

// TestFeedback_FormValidation: exactly one form; shape errors name the index
// and never echo the value; limits; source.
func TestFeedback_FormValidation(t *testing.T) {
	ids21 := make([]string, 21)
	for i := range ids21 {
		ids21[i] = `"` + testULID + `"`
	}
	groups51 := make([]string, 51)
	for i := range groups51 {
		groups51[i] = `{"query_id":"` + testULID + `","ids":["` + testULID2 + `"],"useful":true}`
	}
	groups11x20 := make([]string, 11)
	for i := range groups11x20 {
		groups11x20[i] = `{"query_id":"` + testULID + `","ids":[` + strings.Join(ids21[:20], ",") + `],"useful":true}`
	}
	cases := map[string]struct{ args, want string }{
		"two forms":       {`{"engram_id":"` + testULID + `","query_id":"` + testULID + `","engram_ids":["` + testULID + `"],"useful":true}`, "send exactly one of"},
		"no form":         {`{"useful":true}`, "send exactly one of"},
		"bad query id":    {`{"query_id":"q-0000000000000001","engram_ids":["` + testULID + `"],"useful":true}`, "query_id must be the query_id returned by muninn_recall"},
		"bad memory id":   {`{"query_id":"` + testULID + `","engram_ids":["` + testULID + `","` + testULID2 + `","SECRET-VALUE"],"useful":true}`, "engram_ids[2] is not a memory id"},
		"placeholder":     {`{"query_id":"` + testULID + `","engram_ids":["<ids that helped>"],"useful":true}`, "engram_ids[0] is the placeholder"},
		"group too large": {`{"query_id":"` + testULID + `","engram_ids":[` + strings.Join(ids21, ",") + `],"useful":true}`, "max 20 per recall"},
		"too many groups": {`{"groups":[` + strings.Join(groups51, ",") + `]}`, "max 50"},
		"too many ids":    {`{"groups":[` + strings.Join(groups11x20, ",") + `]}`, "too many ids in one feedback call (max 200); split into several calls"},
		"bad source":      {`{"query_id":"` + testULID + `","engram_ids":["` + testULID + `"],"useful":true,"source":"cron"}`, `source must be "direct"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fe := &recordingFeedbackEngine{}
			resp := feedbackRPC(t, newTestServerWith(fe), c.args)
			require.NotNil(t, resp.Error)
			require.Equal(t, -32602, resp.Error.Code)
			require.Contains(t, resp.Error.Message, c.want)
			require.NotContains(t, resp.Error.Message, "SECRET-VALUE", "never echo an id value")
			require.Zero(t, fe.legacyCalls+fe.labelCalls)
		})
	}
	// A malformed-MAC ro1. id is SHAPED correctly: it is not a format error.
	fe := &recordingFeedbackEngine{}
	ro := "ro1." + testULID + ".0011223344556677.8899aabbccddeeff.AAAAAA.AAAAAAAAAAAAAAAAAAAAAA"
	resp := feedbackRPC(t, newTestServerWith(fe), `{"query_id":"`+ro+`","engram_ids":["`+testULID+`"],"useful":true}`)
	require.Nil(t, resp.Error)
	require.Equal(t, 1, fe.labelCalls)
}

// ------------------------------------------------------ real engine ----

type labelServer struct {
	eng   *engine.Engine
	store *storage.PebbleStore
	srv   *MCPServer
}

// newLabelServer wires a real engine (with an auth secret) behind an MCP
// server that accepts the given mk_ keys and cap_ store.
func newLabelServer(t *testing.T, keys apiKeyValidator, caps capabilityValidator) *labelServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-mcp-labels-*")
	require.NoError(t, err)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	require.NoError(t, err)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder, QueryIDSecret: []byte("mcp-label-test-secret")})
	token := ""
	if keys != nil || caps != nil {
		token = "mdb_static"
	}
	t.Cleanup(func() { eng.Stop(); store.Close(); os.RemoveAll(dir) })
	return &labelServer{eng: eng, store: store, srv: New(":0", NewEngineAdapter(eng, nil, nil), token, keys, caps, nil)}
}

func (ls *labelServer) seed(t *testing.T, vault string) []string {
	t.Helper()
	var ids []string
	for _, c := range []string{
		"drainage culverts on the ridge trail are spaced every forty meters",
		"waterbars on the ridge trail drainage are cut at thirty degrees",
		"switchback walls on the ridge trail need annual drainage inspection",
	} {
		r, err := ls.eng.Write(context.Background(), &mbp.WriteRequest{Vault: vault, Content: c})
		require.NoError(t, err)
		ids = append(ids, r.ID)
	}
	ls.eng.WaitWriteTimeIdle()
	return ids
}

func (ls *labelServer) post(t *testing.T, token, tool string, args map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := mkToolCallBody(tool, args)
	if token == "" {
		return postRPC(t, ls.srv, string(body))
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(mcpSessionHeader, "sess-"+token)
	w := httptest.NewRecorder()
	ls.srv.srv.Handler.ServeHTTP(w, r)
	return w
}

func (ls *labelServer) recall(t *testing.T, token, vault, query string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"vault": vault, "query": query, "threshold": 0.001}
	for k, v := range extra {
		args[k] = v
	}
	return extractInnerJSON(t, decodeResp(t, ls.post(t, token, "muninn_recall", args).Body.String()))
}

func (ls *labelServer) labelRows(t *testing.T, vault string) []storage.RecallLabelRow {
	t.Helper()
	var out []storage.RecallLabelRow
	require.NoError(t, ls.store.ScanRecallLabels(context.Background(), ls.store.ResolveVaultPrefix(vault), storage.RecallPurposeLabel,
		func(r storage.RecallLabelRow) error { out = append(out, r); return nil }))
	return out
}

func memoryIDs(t *testing.T, res map[string]any) []string {
	t.Helper()
	mems, _ := res["memories"].([]any)
	var ids []string
	for _, m := range mems {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	return ids
}

// TestRecallOverMCP_QueryIDAndFeedbackBlockPresent is the hand-built-map trap:
// query_id rides every recall response (empty and read-only included), and
// the feedback block rides every non-empty one.
func TestRecallOverMCP_QueryIDAndFeedbackBlockPresent(t *testing.T) {
	ls := newLabelServer(t, nil, nil)
	ls.seed(t, "default")

	res := ls.recall(t, "", "default", "ridge trail drainage", nil)
	qid, _ := res["query_id"].(string)
	_, err := storage.ParseULID(qid)
	require.NoError(t, err, "query_id %q must be the event ULID", qid)
	fb, ok := res["feedback"].(map[string]any)
	require.True(t, ok, "a non-empty recall carries the feedback block: %v", res)
	require.Equal(t, qid, fb["query_id"])
	call, _ := fb["call"].(string)
	require.Equal(t, `muninn_feedback {"vault":"default","query_id":"`+qid+`","engram_ids":["<ids that helped>"],"useful":true}`, call)
	require.Equal(t, recallFeedbackNote(), fb["note"])

	ro := ls.recall(t, "", "default", "ridge trail drainage", map[string]any{"read_only": true})
	roID, _ := ro["query_id"].(string)
	require.True(t, strings.HasPrefix(roID, "ro1."), "read-only query_id %q", roID)
	require.Equal(t, roID, ro["feedback"].(map[string]any)["query_id"])

	empty := ls.recall(t, "", "default", "zzqx nothing matches", map[string]any{"threshold": 0.99})
	require.Empty(t, empty["memories"])
	require.NotEmpty(t, empty["query_id"], "query_id is present on an empty recall too")
	require.NotContains(t, empty, "feedback", "no feedback block when nothing was returned")
}

// TestRecallFeedbackNote_MatchesSalienceState: the note must not claim a
// ranking effect the engine does not have. While salience is off it says so;
// the slice that turns it on must flip both together.
func TestRecallFeedbackNote_MatchesSalienceState(t *testing.T) {
	note := strings.ToLower(recallFeedbackNote())
	if salienceActive {
		require.NotContains(t, note, "do not change ranking")
	} else {
		require.Contains(t, note, "do not change ranking yet")
	}
	var desc string
	for _, td := range allToolDefinitions() {
		if td.Name == "muninn_feedback" {
			desc = strings.ToLower(td.Description)
		}
	}
	require.Equal(t, !salienceActive, strings.Contains(desc, "do not change ranking yet"), "tool description must match the salience state")
}

// TestRecallFeedbackCall_IsAcceptedFeedbackCall: the copyable call, with its
// placeholder replaced by an id the recall returned, is a valid muninn_feedback
// call that the server accepts and records as a verified label. Sent verbatim
// (placeholder still in), it is a clear -32602 — never a silent no-op.
func TestRecallFeedbackCall_IsAcceptedFeedbackCall(t *testing.T) {
	ls := newLabelServer(t, nil, nil)
	ls.seed(t, "default")
	res := ls.recall(t, "", "default", "ridge trail drainage", nil)
	call := res["feedback"].(map[string]any)["call"].(string)
	tool, argJSON, ok := strings.Cut(call, " ")
	require.True(t, ok)
	require.Equal(t, "muninn_feedback", tool)
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(argJSON), &args), "the call's arguments are valid JSON")

	verbatim := ls.post(t, "", tool, args)
	vr := decodeResp(t, verbatim.Body.String())
	require.NotNil(t, vr.Error)
	require.Contains(t, vr.Error.Message, "placeholder")

	used := memoryIDs(t, res)[1]
	args["engram_ids"] = []any{used}
	ok2 := extractInnerJSON(t, decodeResp(t, ls.post(t, "", tool, args).Body.String()))
	require.Equal(t, true, ok2["ok"])
	rows := ls.labelRows(t, "default")
	require.Len(t, rows, 1)
	require.Equal(t, used, rows[0].EngramID.String())
	require.Equal(t, storage.LabelMembershipVerified, rows[0].Label.Membership)
	require.Equal(t, uint8(1), rows[0].Label.Polarity)
	require.Equal(t, uint16(2), rows[0].Label.Rank)
}

// TestFeedback_ObserveRefused (dispatch): an observe key cannot call
// muninn_feedback at all.
func TestFeedback_ObserveRefused(t *testing.T) {
	ls := newLabelServer(t, newMockKeyStore(auth.APIKey{ID: "obs", Vault: "walled", Mode: auth.ModeObserve}), nil)
	resp := decodeResp(t, ls.post(t, "mk_obs", "muninn_feedback", map[string]any{"query_id": testULID, "engram_ids": []any{testULID2}, "useful": true}).Body.String())
	require.NotNil(t, resp.Error)
	require.Equal(t, -32001, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "observe-mode key cannot call this tool")
}

// TestFeedback_AppendAllowed (dispatch + engine): an append key labels.
func TestFeedback_AppendAllowed(t *testing.T) {
	ls := newLabelServer(t, newMockKeyStore(
		auth.APIKey{ID: "app", Vault: "walled", Mode: auth.ModeAppend},
	), nil)
	ls.seed(t, "walled")
	res := ls.recall(t, "mk_app", "walled", "ridge trail drainage", nil)
	resp := decodeResp(t, ls.post(t, "mk_app", "muninn_feedback", map[string]any{
		"query_id": res["query_id"], "engram_ids": []any{memoryIDs(t, res)[0]}, "useful": true,
	}).Body.String())
	require.Nil(t, resp.Error, "append-mode credentials may label")
	require.Len(t, ls.labelRows(t, "walled"), 1)
}

// TestFeedback_WriteModeResponseUniform: a write-mode key (which cannot recall
// or read) gets byte-identical responses for an existing-and-returned id, an
// existing-not-returned id, a nonexistent id, and an unknown query id — it
// cannot learn existence through feedback.
func TestFeedback_WriteModeResponseUniform(t *testing.T) {
	ls := newLabelServer(t, newMockKeyStore(
		auth.APIKey{ID: "full", Vault: "walled", Mode: auth.ModeFull},
		auth.APIKey{ID: "wo", Vault: "walled", Mode: auth.ModeWrite},
	), nil)
	ids := ls.seed(t, "walled")
	res := ls.recall(t, "mk_full", "walled", "culverts forty meters", map[string]any{"limit": 1})
	returned := memoryIDs(t, res)
	require.Len(t, returned, 1)
	var notReturned string
	for _, id := range ids {
		if id != returned[0] {
			notReturned = id
		}
	}
	qid := res["query_id"].(string)
	bodies := map[string]string{}
	for name, a := range map[string]map[string]any{
		"returned":      {"query_id": qid, "engram_ids": []any{returned[0]}, "useful": true},
		"not returned":  {"query_id": qid, "engram_ids": []any{notReturned}, "useful": true},
		"nonexistent":   {"query_id": qid, "engram_ids": []any{storage.NewULID().String()}, "useful": true},
		"unknown query": {"query_id": storage.NewULID().String(), "engram_ids": []any{returned[0]}, "useful": true},
	} {
		bodies[name] = ls.post(t, "mk_wo", "muninn_feedback", a).Body.String()
	}
	for name, b := range bodies {
		require.Equal(t, bodies["returned"], b, "response for %q differs from the returned-id response", name)
	}
	require.NotContains(t, bodies["returned"], "error")
	require.Len(t, ls.labelRows(t, "walled"), 1, "only the returned id was labelled")
}

// TestFeedback_CapTokenPinnedToVault: a cap_ token is vault-pinned at
// dispatch (SEC-7) — a differing vault argument is refused, and an omitted
// one resolves to the pinned vault.
func TestFeedback_CapTokenPinnedToVault(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	ls := newLabelServer(t, nil, stubCapStore{cap: auth.Capability{Vault: "wf-trail", Mode: auth.ModeFull, ExpiresAt: &exp}})
	ls.seed(t, "wf-trail")
	ls.seed(t, "other-vault")
	res := ls.recall(t, "cap_x", "wf-trail", "ridge trail drainage", nil)
	args := map[string]any{"vault": "other-vault", "query_id": res["query_id"], "engram_ids": []any{memoryIDs(t, res)[0]}, "useful": true}
	resp := decodeResp(t, ls.post(t, "cap_x", "muninn_feedback", args).Body.String())
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, "vault mismatch")
	delete(args, "vault")
	resp = decodeResp(t, ls.post(t, "cap_x", "muninn_feedback", args).Body.String())
	require.Nil(t, resp.Error)
	require.Len(t, ls.labelRows(t, "wf-trail"), 1)
	require.Empty(t, ls.labelRows(t, "other-vault"))
}

// TestStatus_LabelsBlock: muninn_status reports label counts (no ids).
func TestStatus_LabelsBlock(t *testing.T) {
	ls := newLabelServer(t, nil, nil)
	ls.seed(t, "default")
	res := ls.recall(t, "", "default", "ridge trail drainage", nil)
	ids := memoryIDs(t, res)
	ls.post(t, "", "muninn_feedback", map[string]any{"query_id": res["query_id"], "engram_ids": []any{ids[0]}, "useful": true})
	ls.post(t, "", "muninn_feedback", map[string]any{"query_id": storage.NewULID().String(), "engram_ids": []any{ids[0]}, "useful": true})
	st := extractInnerJSON(t, decodeResp(t, ls.post(t, "", "muninn_status", map[string]any{"vault": "default"}).Body.String()))
	lb, ok := st["labels"].(map[string]any)
	require.True(t, ok, "status carries a labels block: %v", st)
	require.Equal(t, float64(1), lb["verified"])
	require.Equal(t, float64(1), lb["positive"])
	require.Equal(t, float64(1), lb["rejected"].(map[string]any)["unknown_event"])
	raw, _ := json.Marshal(lb)
	require.NotContains(t, string(raw), ids[0], "no ids in status")
	_ = fmt.Sprint
}
