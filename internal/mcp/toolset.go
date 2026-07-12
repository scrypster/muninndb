package mcp

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// coreToolNames is the day-to-day working set: session boot, recall, the
// write/evolve path, and health. Everything else (enrichment admin, graph
// export, entity surgery, tree memory, consolidation, ...) remains fully
// callable via tools/call — the toolset only filters what tools/list
// ADVERTISES, because advertised schemas ride in every MCP client's context
// window whether or not they are used (~41KB / ~10k tokens for the full
// 39-tool surface, vs ~1/4 of that for this set).
var coreToolNames = map[string]bool{
	"muninn_recall":         true,
	"muninn_remember":       true,
	"muninn_remember_batch": true,
	"muninn_read":           true,
	"muninn_evolve":         true,
	"muninn_link":           true,
	"muninn_where_left_off": true,
	"muninn_find_by_entity": true,
	"muninn_status":         true,
	"muninn_guide":          true,
}

var toolsetWarnOnce sync.Once

// resolveToolset picks the toolset for one request. Precedence: the
// X-Muninn-Toolset request header (lets each client choose in its own MCP
// config, no daemon restart), then the MUNINN_MCP_TOOLSET env var (daemon
// default), then "full". Unknown values fall through to the next layer —
// a typo in a header or env file must never cost a seat its memory tools.
func resolveToolset(r *http.Request) string {
	if r != nil {
		switch v := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Muninn-Toolset"))); v {
		case "core", "full":
			return v
		case "":
			// no header — fall through to env
		default:
			toolsetWarnOnce.Do(func() {
				slog.Warn("mcp: unknown X-Muninn-Toolset header, falling back to daemon default", "value", v)
			})
		}
	}
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("MUNINN_MCP_TOOLSET"))); v {
	case "core", "full":
		return v
	case "":
		return "full"
	default:
		toolsetWarnOnce.Do(func() {
			slog.Warn("mcp: unknown MUNINN_MCP_TOOLSET value, serving full toolset", "value", v)
		})
		return "full"
	}
}

// exposedToolDefinitions returns the tool definitions tools/list advertises
// for this request. Filtering happens at advertisement time only; dispatch
// (tools/call) is by name and never consults this list.
func exposedToolDefinitions(r *http.Request) []ToolDefinition {
	all := allToolDefinitions()
	if resolveToolset(r) != "core" {
		return all
	}
	out := make([]ToolDefinition, 0, len(coreToolNames))
	for _, t := range all {
		if coreToolNames[t.Name] {
			out = append(out, t)
		}
	}
	return out
}
