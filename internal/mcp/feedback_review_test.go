package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
)

// Adversarial review of slice 2a, items 1 and 4.

// postCopiedCall posts a recall's feedback.call verbatim except for the id
// placeholder, which it replaces with mem — exactly what an agent does.
func postCopiedCall(t *testing.T, ls *labelServer, token string, rec map[string]any, mem string) map[string]any {
	t.Helper()
	call := rec["feedback"].(map[string]any)["call"].(string)
	tool, argJSON, ok := strings.Cut(call, " ")
	require.True(t, ok)
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(argJSON), &args))
	args["engram_ids"] = []any{mem}
	return extractInnerJSON(t, decodeResp(t, ls.post(t, token, tool, args).Body.String()))
}

// TestRecallFeedbackCall_NonDefaultVaultUnpinned (item 1): on a server whose
// credential is not pinned to a vault, the copied call must land its label
// in the vault the recall ran in — for both id kinds.
func TestRecallFeedbackCall_NonDefaultVaultUnpinned(t *testing.T) {
	ls := newLabelServer(t, nil, nil)
	ls.seed(t, "trail-crew")
	ls.seed(t, "default")
	for _, ro := range []bool{false, true} {
		rec := ls.recall(t, "", "trail-crew", "ridge trail drainage", map[string]any{"read_only": ro})
		postCopiedCall(t, ls, "", rec, memoryIDs(t, rec)[0])
	}
	require.Len(t, ls.labelRows(t, "trail-crew"), 2, "both labels land in the recall's vault")
	require.Empty(t, ls.labelRows(t, "default"))
}

// TestFeedback_WrongVaultIsNamedToAnAllVaultReader (item 1a/1b): a label sent
// to the wrong vault is rejected with wrong_vault and the right vault — for a
// caller that may read every vault.
func TestFeedback_WrongVaultIsNamedToAnAllVaultReader(t *testing.T) {
	ls := newLabelServer(t, nil, nil)
	ls.seed(t, "trail-crew")
	ls.seed(t, "default")
	for _, ro := range []bool{false, true} {
		rec := ls.recall(t, "", "trail-crew", "ridge trail drainage", map[string]any{"read_only": ro})
		res := extractInnerJSON(t, decodeResp(t, ls.post(t, "", "muninn_feedback", map[string]any{
			"query_id": rec["query_id"], "engram_ids": []any{memoryIDs(t, rec)[0]}, "useful": true, // no vault → default
		}).Body.String()))
		results, _ := res["results"].([]any)
		require.Len(t, results, 1, "a reader sees per-id outcomes: %v", res)
		r := results[0].(map[string]any)
		require.Equal(t, "rejected", r["outcome"])
		require.Equal(t, "wrong_vault", r["reason"])
		require.Equal(t, "trail-crew", r["vault"])
		require.Contains(t, r["hint"], `"vault":"trail-crew"`)
	}
	require.Empty(t, ls.labelRows(t, "default"))
}

// TestFeedback_ReaderSeesOutcomesWriterDoesNot (item 1b): a full-mode key
// sees why its label was rejected; a write-mode key in the same vault gets
// the uniform body with no results.
func TestFeedback_ReaderSeesOutcomesWriterDoesNot(t *testing.T) {
	ls := newLabelServer(t, newMockKeyStore(
		auth.APIKey{ID: "full", Vault: "walled", Mode: auth.ModeFull},
		auth.APIKey{ID: "wo", Vault: "walled", Mode: auth.ModeWrite},
	), nil)
	ids := ls.seed(t, "walled")
	rec := ls.recall(t, "mk_full", "walled", "culverts forty meters", map[string]any{"limit": 1})
	returned := memoryIDs(t, rec)[0]
	var other string
	for _, id := range ids {
		if id != returned {
			other = id
		}
	}
	args := map[string]any{"query_id": rec["query_id"], "engram_ids": []any{returned, other}, "useful": true}
	full := extractInnerJSON(t, decodeResp(t, ls.post(t, "mk_full", "muninn_feedback", args).Body.String()))
	results := full["results"].([]any)
	require.Equal(t, "recorded", results[0].(map[string]any)["outcome"])
	require.Equal(t, "not_in_recall", results[1].(map[string]any)["reason"])
	require.NotContains(t, results[1].(map[string]any), "vault", "a pinned key is never told about other vaults")

	wo := ls.post(t, "mk_wo", "muninn_feedback", args).Body.String()
	inner := extractInnerJSON(t, decodeResp(t, wo))
	require.NotContains(t, inner, "results")
	require.Equal(t, map[string]any{"ok": true, "received_groups": float64(1), "received_ids": float64(2)}, inner)
}

// TestFeedback_LegacyEchoAndEmptyFieldsAsAbsent (item 4): the legacy response
// keeps its engram_id/useful echo, and null/""/[] in the other forms' fields
// are absent, not a second form.
func TestFeedback_LegacyEchoAndEmptyFieldsAsAbsent(t *testing.T) {
	for _, extra := range []string{``, `,"query_id":null`, `,"query_id":""`, `,"engram_ids":[]`, `,"groups":null`} {
		fe := &recordingFeedbackEngine{}
		resp := feedbackRPC(t, newTestServerWith(fe), `{"vault":"default","engram_id":"`+testULID+`","useful":true`+extra+`}`)
		require.Nil(t, resp.Error, "legacy call with %q", extra)
		out := extractInnerJSON(t, resp)
		require.Equal(t, testULID, out["engram_id"])
		require.Equal(t, true, out["useful"])
		require.Equal(t, 1, fe.legacyCalls)
	}
}
