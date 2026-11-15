package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
)

// salienceActive reports whether any ranking reads feedback labels. It is
// false for the whole of the label-pipeline slice (2a): labels are recorded
// for measurement only. The slice that makes ranking read them (2b) flips
// this, and TestRecallFeedbackNote_MatchesSalienceState forces the agent-
// facing note to flip with it — the note must never claim an effect the
// engine does not have.
const salienceActive = false

// recallFeedbackNote is the `feedback.note` text of every recall response.
func recallFeedbackNote() string {
	if salienceActive {
		return "Send this once with the ids that actually helped. Labels are recorded and used to rank the memories you use above near-equal ones."
	}
	return "Send this once with the ids that actually helped. Labels are recorded for measurement; they do not change ranking yet."
}

// recallFeedbackCall is the ready-to-copy feedback call for a recall: the
// exact tool name and arguments, with useful already true and the query id
// filled in. The ids are a placeholder ON PURPOSE — pre-filling the returned
// ids would make the lazy copy label every row useful, and pre-filling the
// top row would manufacture exactly the position bias the labels exist to
// measure. The placeholder sent verbatim is a clear -32602, never a silent
// no-op.
func recallFeedbackCall(queryID string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<ids that helped>" readable
	_ = enc.Encode(struct {
		QueryID   string   `json:"query_id"`
		EngramIDs []string `json:"engram_ids"`
		Useful    bool     `json:"useful"`
	}{QueryID: queryID, EngramIDs: []string{feedbackIDPlaceholder}, Useful: true})
	return "muninn_feedback " + strings.TrimSpace(buf.String())
}

// recallFeedbackBlock is the `feedback` block of a recall response that
// returned at least one row.
func recallFeedbackBlock(queryID string) map[string]any {
	return map[string]any{
		"query_id": queryID,
		"call":     recallFeedbackCall(queryID),
		"note":     recallFeedbackNote(),
	}
}
