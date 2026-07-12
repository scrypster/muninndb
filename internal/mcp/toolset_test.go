package mcp

import (
	"net/http"
	"testing"
)

func reqWithToolsetHeader(t *testing.T, v string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		r.Header.Set("X-Muninn-Toolset", v)
	}
	return r
}

func TestExposedToolDefinitions(t *testing.T) {
	all := allToolDefinitions()

	t.Run("core names all exist in the full list", func(t *testing.T) {
		names := map[string]bool{}
		for _, td := range all {
			names[td.Name] = true
		}
		for n := range coreToolNames {
			if !names[n] {
				t.Errorf("core tool %q not present in allToolDefinitions — rename drift", n)
			}
		}
	})

	t.Run("unset env, no header serves full", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "")
		if got := len(exposedToolDefinitions(nil)); got != len(all) {
			t.Fatalf("got %d tools, want %d", got, len(all))
		}
	})

	t.Run("env full serves full", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "full")
		if got := len(exposedToolDefinitions(nil)); got != len(all) {
			t.Fatalf("got %d tools, want %d", got, len(all))
		}
	})

	t.Run("env core serves exactly the core set", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "core")
		got := exposedToolDefinitions(nil)
		if len(got) != len(coreToolNames) {
			t.Fatalf("got %d tools, want %d", len(got), len(coreToolNames))
		}
		for _, td := range got {
			if !coreToolNames[td.Name] {
				t.Errorf("non-core tool %q leaked into core toolset", td.Name)
			}
		}
	})

	t.Run("env case and whitespace tolerated", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "  CORE  ")
		if got := len(exposedToolDefinitions(nil)); got != len(coreToolNames) {
			t.Fatalf("got %d tools, want %d", got, len(coreToolNames))
		}
	})

	t.Run("unknown env fails open to full", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "cor")
		if got := len(exposedToolDefinitions(nil)); got != len(all) {
			t.Fatalf("got %d tools, want %d (fail-open)", got, len(all))
		}
	})

	t.Run("header core overrides env full", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "full")
		r := reqWithToolsetHeader(t, "core")
		if got := len(exposedToolDefinitions(r)); got != len(coreToolNames) {
			t.Fatalf("got %d tools, want %d", got, len(coreToolNames))
		}
	})

	t.Run("header full overrides env core", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "core")
		r := reqWithToolsetHeader(t, "FULL")
		if got := len(exposedToolDefinitions(r)); got != len(all) {
			t.Fatalf("got %d tools, want %d", got, len(all))
		}
	})

	t.Run("unknown header falls back to env", func(t *testing.T) {
		t.Setenv("MUNINN_MCP_TOOLSET", "core")
		r := reqWithToolsetHeader(t, "corre")
		if got := len(exposedToolDefinitions(r)); got != len(coreToolNames) {
			t.Fatalf("got %d tools, want %d (fall back to env)", got, len(coreToolNames))
		}
	})
}
