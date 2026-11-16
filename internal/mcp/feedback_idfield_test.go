package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// One name for the memory ids: engram_ids is canonical, ids is accepted as
// an alias in both forms, and a bare string means an array of one.
type idsCapture struct {
	recordingFeedbackEngine
	groups []FeedbackGroup
}

func (e *idsCapture) RecordFeedbackLabels(_ context.Context, _ string, groups []FeedbackGroup, _ string, _ bool) ([]FeedbackOutcome, error) {
	e.labelCalls++
	e.groups = groups
	return nil, nil
}

func TestFeedback_IDFieldAliases(t *testing.T) {
	q := `"query_id":"` + testULID + `"`
	cases := map[string]string{
		"single with ids":        `{` + q + `,"ids":["` + testULID2 + `"],"useful":true}`,
		"batch group engram_ids": `{"groups":[{` + q + `,"engram_ids":["` + testULID2 + `"],"useful":true}]}`,
		"both equal":             `{` + q + `,"engram_ids":["` + testULID2 + `"],"ids":["` + testULID2 + `"],"useful":true}`,
		"bare string":            `{` + q + `,"engram_ids":"` + testULID2 + `","useful":true}`,
		"bare string in batch":   `{"groups":[{` + q + `,"ids":"` + testULID2 + `","useful":true}]}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fe := &idsCapture{}
			resp := feedbackRPC(t, newTestServerWith(fe), args)
			require.Nil(t, resp.Error, "%s: %v", name, resp.Error)
			require.Len(t, fe.groups, 1)
			require.Equal(t, []string{testULID2}, fe.groups[0].IDs)
		})
	}
}

func TestFeedback_IDFieldConflictAndShapeErrors(t *testing.T) {
	q := `"query_id":"` + testULID + `"`
	resp := feedbackRPC(t, newTestServerWith(&idsCapture{}), `{`+q+`,"engram_ids":["`+testULID2+`"],"ids":["`+testULID+`"],"useful":true}`)
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, "engram_ids and ids were both given and differ")

	resp = feedbackRPC(t, newTestServerWith(&idsCapture{}), `{`+q+`,"useful":true}`)
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, `engram_ids (or ids)`)
	require.Contains(t, resp.Error.Message, `{"vault":"<vault>","query_id":"<from muninn_recall>","engram_ids":["<id>"],"useful":true}`)

	resp = feedbackRPC(t, newTestServerWith(&idsCapture{}), `{"groups":[{`+q+`,"useful":true}]}`)
	require.NotNil(t, resp.Error)
	require.Contains(t, resp.Error.Message, `groups[0].engram_ids (or ids)`)
	require.Contains(t, resp.Error.Message, `{"groups":[{"query_id":"<from muninn_recall>","engram_ids":["<id>"],"useful":true}]}`)
}
