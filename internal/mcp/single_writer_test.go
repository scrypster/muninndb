package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestMCP_MutatingToolsRefusedOnFollower is the MCP half of #596: the exact
// reported reproduction — muninn_remember delivered to a Lobe, which returned
// 200 with an engram id that no other node would ever see.
func TestMCP_MutatingToolsRefusedOnFollower(t *testing.T) {
	srv := newTestServer()
	srv.SetWriteGate(func() error {
		return &mbp.NotLeaderError{Role: "lobe", LeaderID: "cortex-1", LeaderAddr: "10.0.0.1:8474"}
	})

	for _, tool := range []string{
		"muninn_remember",
		"muninn_remember_batch",
		"muninn_forget",
		"muninn_link",
		"muninn_evolve",
		"muninn_intend",
	} {
		t.Run(tool, func(t *testing.T) {
			w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+
				`"name":"`+tool+`","arguments":{"content":"written on a lobe","concept":"probe"}}}`)

			var resp struct {
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v (body %s)", err, w.Body.String())
			}
			if resp.Error == nil {
				t.Fatalf("%s on a Lobe returned a result, not an error: %s — the write "+
					"is durable on that one node and invisible cluster-wide (#596)", tool, w.Body.String())
			}
			if resp.Error.Code != -32002 {
				t.Errorf("error code = %d, want -32002 (not the Cortex)", resp.Error.Code)
			}
			if !strings.Contains(resp.Error.Message, "cortex-1") {
				t.Errorf("message must name the Cortex so a client can retry there: %q", resp.Error.Message)
			}
		})
	}
}

// TestMCP_ReadToolsUnaffectedOnFollower: a Lobe exists to serve reads.
func TestMCP_ReadToolsUnaffectedOnFollower(t *testing.T) {
	srv := newTestServer()
	srv.SetWriteGate(func() error {
		return &mbp.NotLeaderError{Role: "lobe", LeaderID: "cortex-1"}
	})
	for _, tool := range []string{"muninn_recall", "muninn_read", "muninn_status"} {
		t.Run(tool, func(t *testing.T) {
			w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+
				`"name":"`+tool+`","arguments":{"context":"anything","id":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}}}`)
			if strings.Contains(w.Body.String(), `"code":-32002`) {
				t.Errorf("read tool %s refused on a Lobe: %s", tool, w.Body.String())
			}
		})
	}
}

// TestMCP_StandaloneUnaffected: no gate installed, nothing changes.
func TestMCP_StandaloneUnaffected(t *testing.T) {
	srv := newTestServer()
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{`+
		`"name":"muninn_remember","arguments":{"content":"standalone"}}}`)
	if strings.Contains(w.Body.String(), `"code":-32002`) {
		t.Fatalf("standalone server refused a write: %s", w.Body.String())
	}
}
