package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// TestRememberBatch_ReportsContentDuplicate: an item whose content exactly
// matches an existing live memory is deduplicated by the engine (no new
// engram; the existing one is reinforced). The single-item muninn_remember
// says so via its hint; the batch response used to report the item as a plain
// fresh write. It must say which existing memory the item resolved to.
func TestRememberBatch_ReportsContentDuplicate(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-batch"
	const dup = "the branch line timetable runs a railcar every forty minutes"

	first := callTool(t, srv, "muninn_remember", fmt.Sprintf(`{"vault":%q,"concept":"branch timetable","content":%q}`, vault, dup))
	existing, _ := first["id"].(string)
	eng.WaitWriteTimeIdle()

	out := callTool(t, srv, "muninn_remember_batch", fmt.Sprintf(
		`{"vault":%q,"memories":[{"concept":"branch timetable again","content":%q},{"concept":"goods yard","content":"the goods yard shunter is a small diesel"}]}`,
		vault, dup))
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results = %v", out)
	}
	r0, _ := results[0].(map[string]any)
	if r0["id"] != existing {
		t.Fatalf("duplicate item id = %v, want the existing memory %s", r0["id"], existing)
	}
	if r0["duplicate_of"] != existing {
		t.Errorf("duplicate item duplicate_of = %v, want %s (full item %v)", r0["duplicate_of"], existing, r0)
	}
	if h, _ := r0["hint"].(string); !strings.Contains(h, "duplicate_content") {
		t.Errorf("duplicate item hint = %q, want it to carry duplicate_content like muninn_remember does", h)
	}
	r1, _ := results[1].(map[string]any)
	if _, has := r1["duplicate_of"]; has {
		t.Errorf("fresh item must not carry duplicate_of: %v", r1)
	}
	if r1["status"] != "ok" || r0["status"] != "ok" {
		t.Errorf("status changed: %v / %v (kept \"ok\" for compatibility; the duplicate signal is additive)", r0["status"], r1["status"])
	}
}
