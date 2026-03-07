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

// runMCPStdio is the stdio→HTTP MCP proxy used by OpenClaw and other clients
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
// state consistent across the lifetime of a single OpenClaw session.
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

		// Best-effort parse to detect the "initialize" method so we can
		// capture the Mcp-Session-Id from its response.
		var rpcEnvelope struct {
			Method string `json:"method"`
		}
		json.Unmarshal([]byte(line), &rpcEnvelope) //nolint:errcheck // ignored intentionally; malformed lines still forwarded

		token := readTokenFile()

		req, err := http.NewRequest(http.MethodPost, mcpProxyURL, bytes.NewBufferString(line))
		if err != nil {
			fmt.Fprintf(os.Stderr, "muninn mcp: build request: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "muninn mcp: daemon unreachable — is muninn running? (%v)\n", err)
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
			fmt.Fprintf(os.Stderr, "muninn mcp: read response: %v\n", readErr)
			continue
		}

		// HTTP 202 Accepted = MCP notification (fire-and-forget); no stdout output.
		if resp.StatusCode == http.StatusAccepted {
			continue
		}

		body = bytes.TrimSpace(body)
		if len(body) > 0 {
			fmt.Fprintf(out, "%s\n", body)
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "muninn mcp: stdin: %v\n", err)
		os.Exit(1)
	}
}
