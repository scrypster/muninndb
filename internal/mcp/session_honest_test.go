package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// muninn_session must answer "what happened since X" truthfully on a busy
// vault: the newest activity first, an explicit signal when the list is
// partial, no soft-deleted memories presented as live writes, and an
// `activations` figure that is measured rather than a constant 0.
//
// Fixtures are invented (model railways), through a REAL engine so the
// engine -> adapter -> handler chain is what is under test.

func sessionWrite(t *testing.T, srv *MCPServer, vault, concept, content string, at time.Time) string {
	t.Helper()
	out := callTool(t, srv, "muninn_remember", fmt.Sprintf(
		`{"vault":%q,"concept":%q,"content":%q,"created_at":%q}`,
		vault, concept, content, at.UTC().Format(time.RFC3339)))
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("remember %q returned no id: %v", concept, out)
	}
	return id
}

func sessionIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["writes"].([]any)
	ids := make([]string, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestSession_NewestFirstPagedAndExcludesDeleted(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-session"

	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	// One write BEFORE the window: must never appear or be counted.
	sessionWrite(t, srv, vault, "turntable bearing", "the turntable bearing was regreased before the window", base.Add(-time.Hour))
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, sessionWrite(t, srv, vault,
			fmt.Sprintf("layout note %d", i),
			fmt.Sprintf("layout note %d: siding %d ballast relaid with fine granite chips", i, i),
			base.Add(time.Duration(i+1)*time.Hour)))
	}
	// Forget the newest one: a soft-deleted memory is not live activity.
	callTool(t, srv, "muninn_forget", fmt.Sprintf(`{"vault":%q,"id":%q}`, vault, ids[4]))
	eng.WaitWriteTimeIdle()

	since := base.UTC().Format(time.RFC3339)
	page1 := callTool(t, srv, "muninn_session", fmt.Sprintf(`{"vault":%q,"since":%q,"limit":2}`, vault, since))
	got1 := sessionIDs(t, page1)
	want1 := []string{ids[3], ids[2]}
	if fmt.Sprint(got1) != fmt.Sprint(want1) {
		t.Fatalf("page 1 writes = %v, want newest-first live writes %v (full response %v)", got1, want1, page1)
	}
	if total, _ := page1["total"].(float64); total != 4 {
		t.Errorf("total = %v, want 4 (5 in window minus 1 forgotten)", page1["total"])
	}
	if returned, _ := page1["returned"].(float64); returned != 2 {
		t.Errorf("returned = %v, want 2", page1["returned"])
	}
	if truncated, _ := page1["truncated"].(bool); !truncated {
		t.Errorf("truncated = %v, want true: 2 of 4 returned", page1["truncated"])
	}

	page2 := callTool(t, srv, "muninn_session", fmt.Sprintf(`{"vault":%q,"since":%q,"limit":2,"offset":2}`, vault, since))
	got2 := sessionIDs(t, page2)
	want2 := []string{ids[1], ids[0]}
	if fmt.Sprint(got2) != fmt.Sprint(want2) {
		t.Fatalf("page 2 writes = %v, want %v", got2, want2)
	}
	if truncated, _ := page2["truncated"].(bool); truncated {
		t.Errorf("page 2 truncated = true, want false: offset 2 + 2 returned covers all 4")
	}
}

func TestSession_ActivationsCountsRecallsWithResultsSince(t *testing.T) {
	eng, srv, store, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-activations"
	ctx := context.Background()

	sessionWrite(t, srv, vault, "signal cabin lever frame", "the signal cabin lever frame has twelve levers painted by function", time.Now().Add(-time.Hour))
	eng.WaitWriteTimeIdle()

	// A recall event recorded long before the window: must not be counted.
	old := storage.NewULIDWithTime(time.Now().Add(-72 * time.Hour))
	if err := store.WriteRecallEvent(ctx, store.ResolveVaultPrefix(vault), old, &storage.RecallEvent{
		SurfacedAt: time.Now().Add(-72 * time.Hour).UnixNano(),
		Entries:    []storage.RecallSurfacedEntry{{ID: "x", Score: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	// Two recalls that return the memory, one that returns nothing.
	for i := 0; i < 2; i++ {
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"signal cabin lever frame"}, MaxResults: 5})
		if err != nil || len(resp.Activations) == 0 {
			t.Fatalf("recall %d: want results, got err=%v n=%d", i, err, len(resp.Activations))
		}
	}
	if resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"zzqx unrelated pantograph"}, MaxResults: 5}); err != nil || len(resp.Activations) != 0 {
		t.Fatalf("empty recall: err=%v n=%d", err, len(resp.Activations))
	}

	out := callTool(t, srv, "muninn_session", fmt.Sprintf(`{"vault":%q,"since":%q}`, vault, since))
	if got, _ := out["activations"].(float64); got != 2 {
		t.Fatalf("activations = %v, want 2 (recalls with results since `since`; the empty recall and the pre-window event are excluded). full: %v", out["activations"], out)
	}
}
