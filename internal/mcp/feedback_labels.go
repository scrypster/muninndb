package mcp

import (
	"bytes"
	"context"
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
//
// The call carries the VAULT the recall ran in: on a server whose credential
// is not pinned to a vault, an omitted vault resolves to "default", and a
// label for a recall in any other vault would be lost.
func recallFeedbackCall(queryID, vault string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep "<ids that helped>" readable
	_ = enc.Encode(struct {
		Vault     string   `json:"vault"`
		QueryID   string   `json:"query_id"`
		EngramIDs []string `json:"engram_ids"`
		Useful    bool     `json:"useful"`
	}{Vault: vault, QueryID: queryID, EngramIDs: []string{feedbackIDPlaceholder}, Useful: true})
	return "muninn_feedback " + strings.TrimSpace(buf.String())
}

// recallFeedbackBlock is the `feedback` block of a recall response that
// returned at least one row.
func recallFeedbackBlock(queryID, vault string) map[string]any {
	return map[string]any{
		"query_id": queryID,
		"vault":    vault,
		"call":     recallFeedbackCall(queryID, vault),
		"note":     recallFeedbackNote(),
	}
}

// feedbackAccess is what dispatch knows about the caller that the feedback
// handler needs: whether it can read the vault (and so may see per-id
// outcomes) and whether it may read every vault (and so may be told which
// vault a misdirected query id belongs to).
type feedbackAccess struct {
	canRead   bool
	allVaults bool
}

type feedbackAccessKey struct{}

func withFeedbackAccess(ctx context.Context, a feedbackAccess) context.Context {
	return context.WithValue(ctx, feedbackAccessKey{}, a)
}

// feedbackAccessFrom fails closed: no value → a non-reader.
func feedbackAccessFrom(ctx context.Context) feedbackAccess {
	a, _ := ctx.Value(feedbackAccessKey{}).(feedbackAccess)
	return a
}

// feedbackHint is the self-correction text for a rejected outcome.
func feedbackHint(o FeedbackOutcome) string {
	switch o.Reason {
	case "not_in_recall":
		return "that recall did not return this id; label only ids from the same recall's memories"
	case "substituted":
		return "that recall returned this memory's current version " + o.HeadID + " in its place; label " + o.HeadID
	case "unknown_query":
		return "no recall with this query_id in this vault; pass the vault the recall ran in (feedback.call carries it)"
	case "wrong_vault":
		return "this query_id belongs to vault " + o.Vault + "; send the label with \"vault\":\"" + o.Vault + "\""
	case "untagged_row":
		return "a read-only query_id covers only its first 100 rows"
	case "no_key":
		return "this server has no auth secret, so read-only query_ids cannot be labelled"
	}
	return ""
}
