package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// mcpProxyURL is the HTTP MCP endpoint for the running daemon.
// Default is derived from defaultMCPPort so a single constant controls the port.
// Override via MUNINN_MCP_URL env var for non-default daemon configurations.
// Overridable in tests via direct assignment.
var mcpProxyURL = "http://127.0.0.1:" + defaultMCPPort + "/mcp"

// mcpStderr is the writer for proxy diagnostic messages. Package-level so
// tests can redirect it without forking a process.
var mcpStderr io.Writer = os.Stderr

// jsonRPCErrorResponse is a JSON-RPC 2.0 error response structure.
type jsonRPCErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   jsonRPCErrorObj `json:"error"`
}

// jsonRPCErrorObj holds the error code and message for a JSON-RPC error.
type jsonRPCErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// writeJSONRPCError writes a JSON-RPC 2.0 error response to out.
// rpcID is the id from the originating request; nil is treated as null.
// This ensures MCP clients receive a structured error instead of silence,
// which would cause them to hang or report "Server disconnected".
//
// Note: for JSON-RPC notifications (requests without an id), emitting an
// error response with id:null is a spec deviation — the spec says servers
// MUST NOT reply to notifications. However, silence causes MCP clients to
// disconnect, so we prefer the explicit error. A code comment explains this
// tradeoff at the call site.
func writeJSONRPCError(out io.Writer, rpcID json.RawMessage, code int, msg string) {
	if rpcID == nil {
		rpcID = json.RawMessage("null")
	}
	resp := jsonRPCErrorResponse{
		JSONRPC: "2.0",
		ID:      rpcID,
		Error:   jsonRPCErrorObj{Code: code, Message: msg},
	}
	b, _ := json.Marshal(resp) // struct contains only primitive types; Marshal cannot fail
	fmt.Fprintf(out, "%s\n", b)
}

// httpStatusToRPCError maps an HTTP status code to a JSON-RPC error code and message.
// Codes are in the implementation-defined range (-32000 to -32099) per JSON-RPC 2.0 spec.
func httpStatusToRPCError(status int) (code int, msg string) {
	switch status {
	case http.StatusUnauthorized:
		return -32001, "MuninnDB: authentication required — verify ~/.muninn/mcp.token matches the running daemon"
	case http.StatusNotFound:
		return -32004, "MuninnDB: MCP endpoint not found"
	case http.StatusTooManyRequests:
		return -32029, "MuninnDB: rate limit exceeded"
	case http.StatusServiceUnavailable:
		return -32000, "MuninnDB: daemon unavailable"
	default:
		return -32000, fmt.Sprintf("MuninnDB: HTTP %d from daemon", status)
	}
}

// runMCPStdio is the stdio→HTTP MCP proxy used by Claude Desktop and other clients
// that spawn MCP servers as local subprocesses. It bridges:
//
//	stdin  (newline-delimited JSON-RPC)  →  MuninnDB HTTP MCP endpoint
//	stdout  ←  JSON-RPC responses
//
// The Bearer token is re-read from disk on every request so the proxy works
// transparently even after a daemon restart.
//
// MUNINN_MCP_URL overrides the target endpoint for non-default port or TLS setups:
//
//	MUNINN_MCP_URL=https://127.0.0.1:8750/mcp muninn mcp
func runMCPStdio() {
	loadEnvFile()

	if u := os.Getenv("MUNINN_MCP_URL"); u != "" {
		mcpProxyURL = u
	}
	runMCPStdioWith(os.Stdin, os.Stdout)
}

// runMCPStdioWith is the testable implementation of the proxy loop.
//
// Session handling: the proxy is MCP session-aware. After forwarding an
// "initialize" request, it captures the Mcp-Session-Id response header and
// includes it in all subsequent requests. This keeps the daemon's per-session
// state consistent across the lifetime of a single client session.
func runMCPStdioWith(in io.Reader, out io.Writer) {
	client := &http.Client{Timeout: 35 * time.Second}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MB max line

	var sessionID string // MCP session ID captured from initialize response

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		// Best-effort parse to detect the "initialize" method and extract the
		// request ID for error responses. Malformed lines are still forwarded.
		var rpcEnvelope struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		json.Unmarshal([]byte(line), &rpcEnvelope) //nolint:errcheck // ignored intentionally; malformed lines still forwarded

		token := readTokenFile()

		req, err := http.NewRequest(http.MethodPost, mcpProxyURL, bytes.NewBufferString(line))
		if err != nil {
			fmt.Fprintf(mcpStderr, "muninn mcp: build request: %v\n", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		// Forward the MCP session ID on all requests after initialize.
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}

		resp, err := client.Do(req)
		if err != nil {
			// Emit a JSON-RPC error so the client receives a structured failure
			// instead of silence (which causes MCP clients to disconnect).
			// See writeJSONRPCError for the notification id:null spec note.
			fmt.Fprintf(mcpStderr, "muninn mcp: daemon unreachable — is muninn running? (%v)\n", err)
			writeJSONRPCError(out, rpcEnvelope.ID, -32000, "MuninnDB: daemon unreachable — is muninn running?")
			continue
		}

		// Capture session ID from the initialize response per MCP Streamable HTTP spec.
		if rpcEnvelope.Method == "initialize" {
			if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
				sessionID = sid
			}
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			fmt.Fprintf(mcpStderr, "muninn mcp: read response: %v\n", readErr)
			writeJSONRPCError(out, rpcEnvelope.ID, -32000, "MuninnDB: failed to read daemon response")
			continue
		}

		// HTTP 202 Accepted = MCP notification (fire-and-forget); no stdout output.
		if resp.StatusCode == http.StatusAccepted {
			continue
		}

		if resp.StatusCode >= 400 {
			code, msg := httpStatusToRPCError(resp.StatusCode)
			if resp.StatusCode == http.StatusUnauthorized {
				fmt.Fprintf(mcpStderr, "muninn mcp: 401 Unauthorized — token mismatch; verify ~/.muninn/mcp.token and restart the daemon if the token changed\n")
			}
			writeJSONRPCError(out, rpcEnvelope.ID, code, msg)
			continue
		}

		body = bytes.TrimSpace(body)
		if len(body) > 0 {
			fmt.Fprintf(out, "%s\n", body)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(mcpStderr, "muninn mcp: stdin: %v\n", err)
		os.Exit(1)
	}
}
