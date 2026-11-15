package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/scrypster/muninndb/internal/metrics"
)

// Near-miss arguments used to lose writes outright: over 30 days of real agent
// traffic, a fifth of muninn_evolve calls failed, most because the caller sent
// `content` instead of `new_content`, and 44% of muninn_decide calls failed
// because a long decision became an over-cap concept. Fixtures are invented
// (model railways).

func callToolRPCError(t *testing.T, srv *MCPServer, name, args string) string {
	t.Helper()
	rpc := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":%q,"arguments":%s}}`, name, args)
	resp := decodeResp(t, postRPC(t, srv, rpc).Body.String())
	if resp.Error == nil {
		t.Fatalf("%s %s: want a JSON-RPC error, got result %v", name, args, resp.Result)
	}
	return resp.Error.Message
}

func notesOf(out map[string]any) []string {
	raw, _ := out["notes"].([]any)
	var s []string
	for _, r := range raw {
		if v, ok := r.(string); ok {
			s = append(s, v)
		}
	}
	return s
}

func TestEvolve_AcceptsContentAsNewContentAndSaysSo(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-evolve"
	id := sessionWrite(t, srv, vault, "station clock", "the station clock is wound on Mondays", nowMinus(t))
	eng.WaitWriteTimeIdle()

	out := callTool(t, srv, "muninn_evolve", fmt.Sprintf(
		`{"vault":%q,"id":%q,"content":"the station clock is wound on Mondays and Thursdays","reason":"second winding added"}`, vault, id))
	newID, _ := out["id"].(string)
	if newID == "" || newID == id {
		t.Fatalf("evolve with `content` did not produce a successor: %v", out)
	}
	notes := notesOf(out)
	if len(notes) == 0 || !strings.Contains(notes[0], "`content`") || !strings.Contains(notes[0], "`new_content`") {
		t.Errorf("notes = %v, want a note that `content` was accepted as `new_content`", notes)
	}
	read := callTool(t, srv, "muninn_read", fmt.Sprintf(`{"vault":%q,"id":%q}`, vault, newID))
	if c, _ := read["content"].(string); !strings.Contains(c, "Thursdays") {
		t.Errorf("successor content = %q, want the aliased text stored", c)
	}
}

func TestEvolve_ConflictingContentAndNewContentIsRejected(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	msg := callToolRPCError(t, srv, "muninn_evolve",
		`{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","new_content":"version A","content":"version B","reason":"x"}`)
	if !strings.Contains(msg, "new_content") || !strings.Contains(msg, "content") || !strings.Contains(strings.ToLower(msg), "differ") {
		t.Errorf("error = %q, want it to say new_content and content differ", msg)
	}
}

func TestEvolve_MissingReasonNamesTheFieldAndShowsAnExample(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	before := testutil.ToFloat64(metrics.MCPArgRejectionsTotal.WithLabelValues("muninn_evolve", "missing_field"))
	msg := callToolRPCError(t, srv, "muninn_evolve", `{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","new_content":"version A"}`)
	if !strings.Contains(msg, "'reason'") {
		t.Errorf("error = %q, want it to name 'reason'", msg)
	}
	if !strings.Contains(msg, `"reason":`) || !strings.Contains(msg, `"new_content":`) {
		t.Errorf("error = %q, want a one-line valid example call", msg)
	}
	if after := testutil.ToFloat64(metrics.MCPArgRejectionsTotal.WithLabelValues("muninn_evolve", "missing_field")); after != before+1 {
		t.Errorf("muninn_mcp_arg_rejections_total{tool=muninn_evolve,reason=missing_field} = %v, want %v", after, before+1)
	}
}

// TestArgRejectionCounter_CoversEveryToolGenerically: a -32602 from ANY tool
// handler is counted, so a tool nobody instrumented by hand is still visible.
func TestArgRejectionCounter_CoversEveryToolGenerically(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	before := testutil.ToFloat64(metrics.MCPArgRejectionsTotal.WithLabelValues("muninn_session", "invalid_params"))
	callToolRPCError(t, srv, "muninn_session", `{}`)
	if after := testutil.ToFloat64(metrics.MCPArgRejectionsTotal.WithLabelValues("muninn_session", "invalid_params")); after != before+1 {
		t.Errorf("generic rejection not counted: %v -> %v", before, after)
	}
}

func TestEvolve_SupersededTargetErrorReachesTheCaller(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-fork"
	id := sessionWrite(t, srv, vault, "platform length", "platform two holds four coaches", nowMinus(t))
	eng.WaitWriteTimeIdle()
	out := callTool(t, srv, "muninn_evolve", fmt.Sprintf(`{"vault":%q,"id":%q,"new_content":"platform two holds five coaches","reason":"extended"}`, vault, id))
	head, _ := out["id"].(string)
	msg := callToolRPCError(t, srv, "muninn_evolve", fmt.Sprintf(`{"vault":%q,"id":%q,"new_content":"platform two holds six coaches","reason":"stale edit"}`, vault, id))
	if !strings.Contains(msg, head) || !strings.Contains(msg, "evolve "+head) {
		t.Errorf("error = %q, want it to name the current head %s and say to evolve it", msg, head)
	}
}

func nowMinus(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Add(-time.Hour)
}
