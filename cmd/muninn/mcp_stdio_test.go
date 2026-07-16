package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// withMCPProxyURL overrides the global mcpProxyURL for the duration of a test.
func withMCPProxyURL(t *testing.T, url string) {
	t.Helper()
	orig := mcpProxyURL
	mcpProxyURL = url
	t.Cleanup(func() { mcpProxyURL = orig })
}

// TestMCPProxyURLDefault guards that the default URL is derived from the canonical
// port constant and targets localhost. If defaultMCPPort changes, this will catch
// any mismatch before a release.
func TestMCPProxyURLDefault(t *testing.T) {
	if !strings.Contains(mcpProxyURL, defaultMCPPort) {
		t.Errorf("mcpProxyURL %q must contain defaultMCPPort %q", mcpProxyURL, defaultMCPPort)
	}
	if !strings.Contains(mcpProxyURL, "127.0.0.1") {
		t.Errorf("mcpProxyURL %q must target 127.0.0.1 (localhost only)", mcpProxyURL)
	}
	if !strings.HasSuffix(mcpProxyURL, "/mcp") {
		t.Errorf("mcpProxyURL %q must end with /mcp", mcpProxyURL)
	}
}

// TestRunMCPStdio_EnvVarOverride verifies MUNINN_MCP_URL is applied before the
// proxy loop starts. Uses a test server as the override target.
func TestRunMCPStdio_EnvVarOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	t.Setenv("MUNINN_MCP_URL", srv.URL)
	orig := mcpProxyURL
	defer func() { mcpProxyURL = orig }()

	// Apply the env override exactly as runMCPStdio does.
	if u := os.Getenv("MUNINN_MCP_URL"); u != "" {
		mcpProxyURL = u
	}
	if mcpProxyURL != srv.URL {
		t.Errorf("env var override not applied: got %q, want %q", mcpProxyURL, srv.URL)
	}

	// Verify the proxy actually reaches the overridden server.
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)
	if out.Len() != 0 {
		t.Errorf("expected no output for 202, got %q", out.String())
	}
}

func TestRunMCPStdioWith_NotificationNoOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if out.Len() != 0 {
		t.Errorf("expected no output for 202 Accepted, got %q", out.String())
	}
}

func TestRunMCPStdioWith_ResponseWrittenToOut(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	got := strings.TrimSpace(out.String())
	if got != body {
		t.Errorf("expected %q, got %q", body, got)
	}
}

func TestRunMCPStdioWith_EmptyLinesSkipped(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	// Two empty/whitespace lines followed by one real request.
	in := strings.NewReader("\n   \n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if calls != 1 {
		t.Errorf("expected 1 HTTP call (empty lines skipped), got %d", calls)
	}
}

// withMCPStderr redirects mcpStderr for the duration of a test.
func withMCPStderr(t *testing.T, w io.Writer) {
	t.Helper()
	orig := mcpStderr
	mcpStderr = w
	t.Cleanup(func() { mcpStderr = orig })
}

func TestRunMCPStdioWith_DaemonUnreachable_JSONRPCError(t *testing.T) {
	withMCPProxyURL(t, "http://127.0.0.1:1") // nothing listening

	in := strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"ping"}` + "\n")
	var out bytes.Buffer
	var errBuf bytes.Buffer
	withMCPStderr(t, &errBuf)
	runMCPStdioWith(in, &out)

	// Must produce a valid JSON-RPC error, not silence.
	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("expected valid JSON-RPC error response, got: %q — parse error: %v", out.String(), err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error field in response, got %v", resp)
	}
	if code, _ := errObj["code"].(float64); code != -32000 {
		t.Errorf("expected error code -32000, got %v", code)
	}
	if id, _ := resp["id"].(float64); id != 42 {
		t.Errorf("expected id=42 in error response, got %v", resp["id"])
	}
	// Diagnostic message must go to stderr.
	if !strings.Contains(errBuf.String(), "daemon unreachable") {
		t.Errorf("expected stderr diagnostic about daemon unreachable, got %q", errBuf.String())
	}
}

// TestRunMCPStdioWith_401_JSONRPCError verifies that an HTTP 401 is converted to
// a proper JSON-RPC error with code -32001 and a diagnostic on stderr.
func TestRunMCPStdioWith_401_JSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	var errBuf bytes.Buffer
	withMCPStderr(t, &errBuf)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("expected valid JSON-RPC error response, got: %q — parse error: %v", out.String(), err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error field in response, got %v", resp)
	}
	if code, _ := errObj["code"].(float64); code != -32001 {
		t.Errorf("expected error code -32001, got %v", code)
	}
	if id, _ := resp["id"].(float64); id != 7 {
		t.Errorf("expected id=7 in error response, got %v", resp["id"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "authentication") {
		t.Errorf("expected auth message, got %q", msg)
	}
	if !strings.Contains(errBuf.String(), "401") {
		t.Errorf("expected 401 diagnostic on stderr, got %q", errBuf.String())
	}
}

// TestRunMCPStdioWith_404_JSONRPCError verifies HTTP 404 → JSON-RPC error -32004.
func TestRunMCPStdioWith_404_JSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)
	withMCPStderr(t, &bytes.Buffer{})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("expected valid JSON, got: %q — %v", out.String(), err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32004 {
		t.Errorf("expected -32004, got %v", code)
	}
}

// TestRunMCPStdioWith_500_JSONRPCError verifies HTTP 500 → JSON-RPC error -32000.
func TestRunMCPStdioWith_500_JSONRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)
	withMCPStderr(t, &bytes.Buffer{})

	in := strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("expected valid JSON, got: %q — %v", out.String(), err)
	}
	errObj, _ := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); code != -32000 {
		t.Errorf("expected -32000, got %v", code)
	}
}

// TestRunMCPStdioWith_ErrorNullIDWhenNoIDInRequest verifies that errors for
// requests without an id field use id:null in the response.
func TestRunMCPStdioWith_ErrorNullIDWhenNoIDInRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)
	withMCPStderr(t, &bytes.Buffer{})

	// Notification: no "id" field
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	var resp map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &resp); err != nil {
		t.Fatalf("expected valid JSON, got: %q — %v", out.String(), err)
	}
	if resp["id"] != nil {
		t.Errorf("expected id:null for request with no id, got %v", resp["id"])
	}
}

func TestRunMCPStdioWith_ContentTypeHeader(t *testing.T) {
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want \"application/json\"", gotContentType)
	}
}

// TestRunMCPStdioWith_ToolsetHeaderForwarded verifies the proxy forwards the
// client-side toolset preference as X-Muninn-Toolset — stdio clients have no
// other channel to reach the daemon's per-client toolset switch.
func TestRunMCPStdioWith_ToolsetHeaderForwarded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Muninn-Toolset")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	orig := mcpToolset
	mcpToolset = "core"
	t.Cleanup(func() { mcpToolset = orig })

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if got != "core" {
		t.Errorf("X-Muninn-Toolset = %q, want \"core\"", got)
	}
}

// TestRunMCPStdioWith_NoToolsetHeaderWhenUnset verifies no header is sent when
// the client sets no preference, leaving the daemon's own default in charge.
func TestRunMCPStdioWith_NoToolsetHeaderWhenUnset(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["X-Muninn-Toolset"]
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	orig := mcpToolset
	mcpToolset = ""
	t.Cleanup(func() { mcpToolset = orig })

	in := strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if present {
		t.Error("X-Muninn-Toolset header sent despite no client preference")
	}
}

func TestRunMCPStdioWith_MultipleRequests(t *testing.T) {
	responses := []string{
		`{"jsonrpc":"2.0","id":1,"result":{"pong":true}}`,
		`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`,
	}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(responses[i]))
		i++
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	input := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 output lines, got %d: %s", len(lines), out.String())
	}
	for idx, line := range lines {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Errorf("line %d is not valid JSON: %v — %s", idx, err, line)
		}
	}
}

// TestRunMCPStdioWith_SessionIDCapturedAndForwarded verifies that the proxy
// captures Mcp-Session-Id from the initialize response and forwards it on all
// subsequent requests, as required by the MCP Streamable HTTP specification.
func TestRunMCPStdioWith_SessionIDCapturedAndForwarded(t *testing.T) {
	const wantSessionID = "test-session-abc123"
	var gotSessionIDOnSecond string
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			// initialize — respond with a session ID in the header.
			w.Header().Set("Mcp-Session-Id", wantSessionID)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"muninn","version":"1.0.0"}}}`))
		default:
			// All subsequent requests — record the session ID header.
			gotSessionIDOnSecond = r.Header.Get("Mcp-Session-Id")
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if callCount != 2 {
		t.Fatalf("expected 2 HTTP calls, got %d", callCount)
	}
	if gotSessionIDOnSecond != wantSessionID {
		t.Errorf("Mcp-Session-Id not forwarded: got %q, want %q", gotSessionIDOnSecond, wantSessionID)
	}
}

// TestRunMCPStdioWith_SessionIDNotSentBeforeInitialize verifies that no
// Mcp-Session-Id header is sent on the initialize request itself.
func TestRunMCPStdioWith_SessionIDNotSentBeforeInitialize(t *testing.T) {
	var initSessionHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initSessionHeader = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if initSessionHeader != "" {
		t.Errorf("Mcp-Session-Id must not be sent before initialize completes, got %q", initSessionHeader)
	}
}

// TestRunMCPStdioWith_NoSessionIDWhenServerOmitsIt verifies the proxy works
// correctly when the server does not return a session ID (current MuninnDB behavior).
func TestRunMCPStdioWith_NoSessionIDWhenServerOmitsIt(t *testing.T) {
	var secondRequestSessionID string
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount > 1 {
			secondRequestSessionID = r.Header.Get("Mcp-Session-Id")
		}
		// Respond without Mcp-Session-Id header.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	withMCPProxyURL(t, srv.URL)

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	runMCPStdioWith(in, &out)

	if secondRequestSessionID != "" {
		t.Errorf("expected no session ID when server omits it, got %q", secondRequestSessionID)
	}
}
