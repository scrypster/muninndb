package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// muninn_recall's only required input was `context`, an array of strings.
// Measured: ~12% of real recall calls (75% for one orchestrator model) fail
// INSIDE the client as malformed JSON while emitting that array, so the server
// never sees them. A plain string field is easier to emit. Fixtures invented
// (model railways).

func recallIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	mems, _ := out["memories"].([]any)
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		mm, _ := m.(map[string]any)
		id, _ := mm["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func seedRecallVault(t *testing.T, srv *MCPServer, vault string) {
	t.Helper()
	for i, c := range []string{
		"the signal box at the junction controls twelve points",
		"the junction signal box was repainted green last spring",
		"the goods shed roof needs new slates",
	} {
		callTool(t, srv, "muninn_remember", fmt.Sprintf(`{"vault":%q,"concept":"note %d","content":%q}`, vault, i, c))
	}
}

func TestRecall_QueryStringIsEquivalentToContextArray(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-recall"
	seedRecallVault(t, srv, vault)
	eng.WaitWriteTimeIdle()

	viaContext := recallIDs(t, callTool(t, srv, "muninn_recall", fmt.Sprintf(`{"vault":%q,"context":["junction signal box"],"read_only":true}`, vault)))
	viaQuery := recallIDs(t, callTool(t, srv, "muninn_recall", fmt.Sprintf(`{"vault":%q,"query":"junction signal box","read_only":true}`, vault)))
	if len(viaContext) == 0 {
		t.Fatal("fixture: context recall returned nothing")
	}
	if fmt.Sprint(viaQuery) != fmt.Sprint(viaContext) {
		t.Fatalf("query recall = %v, context recall = %v; want identical rows", viaQuery, viaContext)
	}
	viaBare := recallIDs(t, callTool(t, srv, "muninn_recall", fmt.Sprintf(`{"vault":%q,"context":"junction signal box","read_only":true}`, vault)))
	if fmt.Sprint(viaBare) != fmt.Sprint(viaContext) {
		t.Fatalf("bare-string context recall = %v, want %v", viaBare, viaContext)
	}
}

func TestRecall_NeitherQueryNorContextNamesBoth(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	for _, args := range []string{`{}`, `{"query":""}`, `{"query":"   ","context":[]}`} {
		msg := callToolRPCError(t, srv, "muninn_recall", args)
		if !strings.Contains(msg, "'query'") || !strings.Contains(msg, "'context'") {
			t.Errorf("args %s: error %q must name both 'query' and 'context'", args, msg)
		}
	}
}

func TestRecall_SchemaOffersQueryFirstAndDoesNotRequireContext(t *testing.T) {
	var recall ToolDefinition
	for _, td := range allToolDefinitions() {
		if td.Name == "muninn_recall" {
			recall = td
		}
	}
	raw, err := json.Marshal(recall)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InputSchema struct {
			Properties json.RawMessage `json:"properties"`
			Required   []string        `json:"required"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, r := range wire.InputSchema.Required {
		if r == "context" {
			t.Fatalf("schema still requires 'context': %v", wire.InputSchema.Required)
		}
	}
	// First property on the wire must be query (models fill the first simple field).
	dec := json.NewDecoder(strings.NewReader(string(wire.InputSchema.Properties)))
	if _, err := dec.Token(); err != nil { // {
		t.Fatal(err)
	}
	first, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	if first != "query" {
		t.Fatalf("first schema property = %v, want query", first)
	}
	var props map[string]map[string]any
	if err := json.Unmarshal(wire.InputSchema.Properties, &props); err != nil {
		t.Fatal(err)
	}
	if props["query"]["type"] != "string" {
		t.Errorf("query type = %v, want string", props["query"]["type"])
	}
	if !strings.Contains(recall.Description, `"query"`) {
		t.Errorf("description should show an example using query: %q", recall.Description)
	}
}

// TestSchemaPropertyOrderKeyNeverReachesTheWire: the ordering hint is a
// Go-side key; no client may see it.
func TestSchemaPropertyOrderKeyNeverReachesTheWire(t *testing.T) {
	raw, err := json.Marshal(allToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), schemaPropertyOrderKey) {
		t.Fatalf("%s leaked into tools/list output", schemaPropertyOrderKey)
	}
}

// TestRecall_NullOptionalFieldIsAbsent: a JSON null for query or context
// must be treated as absent — `{"query":null,"context":[...]}` worked before
// query existed and must keep working.
func TestRecall_NullOptionalFieldIsAbsent(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-null"
	seedRecallVault(t, srv, vault)
	eng.WaitWriteTimeIdle()
	want := fmt.Sprint(recallIDs(t, callTool(t, srv, "muninn_recall", fmt.Sprintf(`{"vault":%q,"context":["junction signal box"],"read_only":true}`, vault))))
	for _, args := range []string{
		fmt.Sprintf(`{"vault":%q,"query":null,"context":["junction signal box"],"read_only":true}`, vault),
		fmt.Sprintf(`{"vault":%q,"context":null,"query":"junction signal box","read_only":true}`, vault),
	} {
		if got := fmt.Sprint(recallIDs(t, callTool(t, srv, "muninn_recall", args))); got != want {
			t.Errorf("%s: rows %s, want %s", args, got, want)
		}
	}
}
