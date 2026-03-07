package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func mcpHealthCheck(baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(baseURL + "/mcp/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func mcpCall(baseURL, toolName string, args map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      1,
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	resp, err := http.Post(baseURL+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if errObj, ok := result["error"]; ok {
		return nil, fmt.Errorf("MCP error: %v", errObj)
	}
	res, _ := result["result"].(map[string]any)
	return res, nil
}

func (r *replState) cmdShowVaults() {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", "http://127.0.0.1:8475/api/vaults", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	if r.sessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: r.sessionCookie})
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error connecting to server: %v\n", err)
		fmt.Println("Is muninn running? Try: muninn start")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Fallback: show static message
		fmt.Println("  default   (built-in)")
		fmt.Println()
		fmt.Println("  For full vault list, open: http://127.0.0.1:8476")
		return
	}

	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Fprintf(os.Stderr, "Error decoding response: %v\n", err)
		return
	}

	// The API may return []string, []vault, or {vaults: []} — handle all forms.
	var vaultNames []string
	var vaults []map[string]any
	switch v := result.(type) {
	case []any:
		for _, item := range v {
			switch val := item.(type) {
			case string:
				vaultNames = append(vaultNames, val)
			case map[string]any:
				vaults = append(vaults, val)
			}
		}
	case map[string]any:
		if list, ok := v["vaults"].([]any); ok {
			for _, item := range list {
				switch val := item.(type) {
				case string:
					vaultNames = append(vaultNames, val)
				case map[string]any:
					vaults = append(vaults, val)
				}
			}
		}
	}

	if len(vaults) == 0 && len(vaultNames) == 0 {
		fmt.Println("  No vaults found.")
		fmt.Println("  Vaults are created automatically when your AI tools store their first memory.")
		return
	}

	if len(vaults) > 0 {
		formatVaultTable(vaults)
		return
	}

	// String-only vault list — filter out test vaults and display cleanly.
	var userVaults, testVaults int
	for _, name := range vaultNames {
		if strings.HasPrefix(name, "proof-") {
			testVaults++
			continue
		}
		userVaults++
		fmt.Printf("  • %s\n", name)
	}
	if testVaults > 0 {
		fmt.Printf("\n  (%d test vaults hidden)\n", testVaults)
	}
	if userVaults == 0 && testVaults > 0 {
		fmt.Println("  No user vaults found (only test vaults).")
	}
}

func (r *replState) cmdShowMemories() {
	since := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	result, err := mcpCall(r.mcpURL, "muninn_session", map[string]any{
		"vault": r.vault,
		"since": since,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	// Check for empty result and provide a helpful message
	if isEmptyMCPResult(result) {
		fmt.Println("No memories in vault '" + r.vault + "' in the last 24 hours.")
		fmt.Println()
		fmt.Println("Memories are created automatically when your AI tools are connected.")
		fmt.Println("Run 'muninn init' to connect Claude Desktop, Cursor, or other tools.")
		return
	}
	prettyPrint(result)
}

type searchOptions struct {
	query   string
	since   string // RFC3339
	before  string // RFC3339
	mode    string // "semantic", "recent", "balanced", "deep", "cgdn", or "" (default)
	hops    int    // 0 = not set
	profile string
}

// parseSearchFlags parses: search <query words...> [--flag value]...
// Query is everything before the first --flag token.
// Returns (opts, errMsg); errMsg is empty on success.
func parseSearchFlags(input string) (searchOptions, string) {
	tokens := strings.Fields(input)
	opts := searchOptions{}

	// Split query/flags at first token starting with "--"
	flagStart := len(tokens)
	for i, tok := range tokens {
		if strings.HasPrefix(tok, "--") {
			flagStart = i
			break
		}
	}

	opts.query = strings.Join(tokens[:flagStart], " ")

	// Parse flags
	flagTokens := tokens[flagStart:]
	for i := 0; i < len(flagTokens); i++ {
		tok := flagTokens[i]
		if !strings.HasPrefix(tok, "--") {
			return opts, fmt.Sprintf("unexpected token after flags: %q", tok)
		}
		if i+1 >= len(flagTokens) {
			return opts, fmt.Sprintf("flag %q requires a value", tok)
		}
		val := flagTokens[i+1]
		i++ // consume value

		switch tok {
		case "--since":
			if _, err := time.Parse(time.RFC3339, val); err != nil {
				if _, err2 := time.Parse("2006-01-02", val); err2 != nil {
					return opts, fmt.Sprintf("--since: invalid ISO8601 date: %q", val)
				}
				t, _ := time.Parse("2006-01-02", val)
				val = t.UTC().Format(time.RFC3339)
			}
			opts.since = val
		case "--before":
			if _, err := time.Parse(time.RFC3339, val); err != nil {
				if _, err2 := time.Parse("2006-01-02", val); err2 != nil {
					return opts, fmt.Sprintf("--before: invalid ISO8601 date: %q", val)
				}
				t, _ := time.Parse("2006-01-02", val)
				val = t.UTC().Format(time.RFC3339)
			}
			opts.before = val
		case "--mode":
			switch val {
			case "semantic", "recent", "balanced", "deep":
				opts.mode = val
			case "actr":
				opts.mode = "balanced" // legacy alias
			case "cgdn":
				opts.mode = val // pass through for power users
			case "additive", "":
				opts.mode = "" // default
			default:
				return opts, fmt.Sprintf("--mode: unknown %q (valid: semantic, recent, balanced, deep)", val)
			}
		case "--hops":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return opts, fmt.Sprintf("--hops: must be a non-negative integer, got %q", val)
			}
			opts.hops = n
		case "--profile":
			validProfiles := map[string]bool{
				"default": true, "causal": true, "confirmatory": true,
				"adversarial": true, "structural": true,
			}
			if !validProfiles[val] {
				return opts, fmt.Sprintf("--profile: unknown value %q (valid: default, causal, confirmatory, adversarial, structural)", val)
			}
			opts.profile = val
		default:
			return opts, fmt.Sprintf("unknown flag: %s. Valid: --since, --before, --mode, --hops, --profile", tok)
		}
	}

	return opts, ""
}

func (r *replState) cmdSearch(input string) {
	opts, errMsg := parseSearchFlags(input)
	if errMsg != "" {
		fmt.Printf("Error: %s\n", errMsg)
		return
	}
	if opts.query == "" {
		fmt.Println("  Usage:   search <query> [--mode semantic|recent|balanced|deep] [--since ISO8601] [--before ISO8601] [--hops N] [--profile default|causal|confirmatory|adversarial|structural]")
		fmt.Println("  Example: search decisions about authentication --since 2026-01-01")
		return
	}

	params := map[string]any{
		"vault":   r.vault,
		"context": []string{opts.query},
		"limit":   10,
	}
	if opts.since != "" {
		params["since"] = opts.since
	}
	if opts.before != "" {
		params["before"] = opts.before
	}
	if opts.mode != "" {
		params["mode"] = opts.mode
	}
	if opts.hops > 0 {
		params["max_hops"] = opts.hops
	}
	if opts.profile != "" {
		params["profile"] = opts.profile
	}

	result, err := mcpCall(r.mcpURL, "muninn_recall", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	if isEmptyMCPResult(result) {
		fmt.Printf("No memories match '%s'.\n", opts.query)
		fmt.Println()
		fmt.Println("Tips:")
		fmt.Println("  • Try broader terms or synonyms")
		fmt.Println("  • Semantic search works best with natural language phrases")
		fmt.Println("  • Check Settings → Vault in the web UI to verify an embedder is configured")
		return
	}
	prettyPrint(result)
}

func (r *replState) cmdGet(id string) {
	result, err := mcpCall(r.mcpURL, "muninn_read", map[string]any{
		"vault": r.vault,
		"id":    id,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	prettyPrint(result)
}

func (r *replState) cmdForget(id string) {
	_, err := mcpCall(r.mcpURL, "muninn_forget", map[string]any{
		"vault": r.vault,
		"id":    id,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	fmt.Printf("  Soft-deleted memory %s\n", id)
	fmt.Printf("  Undo with: restore %s\n", id)
}

func (r *replState) cmdShowContradictions() {
	result, err := mcpCall(r.mcpURL, "muninn_contradictions", map[string]any{
		"vault": r.vault,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	prettyPrint(result)
}

func (r *replState) cmdShowStats() {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(r.mcpURL + "/mcp/health")
	if err != nil || resp.StatusCode != 200 {
		fmt.Println("Server: not running")
		fmt.Println("Start with: muninn start")
		return
	}
	resp.Body.Close()
	fmt.Println("Server: running")
	fmt.Println("  MBP  :8474   binary protocol")
	fmt.Println("  REST :8475   JSON API")
	fmt.Println("  MCP  :8750   AI tool integration")
	fmt.Println("  UI   :8476   http://127.0.0.1:8476")
	if r.vault != "" {
		fmt.Println()
		result, err := mcpCall(r.mcpURL, "muninn_status", map[string]any{"vault": r.vault})
		if err == nil {
			prettyPrint(result)
		}
	}
}

// isEmptyMCPResult returns true if the MCP result has no meaningful content.
func isEmptyMCPResult(result map[string]any) bool {
	if result == nil {
		return true
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return true
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		return true
	}
	text, _ := item["text"].(string)
	// Check for empty JSON arrays/objects or empty strings
	trimmed := strings.TrimSpace(text)
	return trimmed == "" || trimmed == "[]" || trimmed == "{}" || trimmed == "null"
}

func runShowVaults() {
	r := &replState{mcpURL: "http://127.0.0.1:8750"}
	r.cmdShowVaults()
}

// prettyPrint extracts text content from an MCP result and prints it.
func prettyPrint(result map[string]any) {
	if result == nil {
		fmt.Println("(no result)")
		return
	}
	// MCP text content format: result["content"][0]["text"]
	if content, ok := result["content"].([]any); ok && len(content) > 0 {
		if item, ok := content[0].(map[string]any); ok {
			if text, ok := item["text"].(string); ok {
				var v any
				if json.Unmarshal([]byte(text), &v) == nil {
					b, _ := json.MarshalIndent(v, "", "  ")
					fmt.Println(string(b))
					return
				}
				fmt.Println(text)
				return
			}
		}
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
}

// Config persistence

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muninn", "config")
}

func loadDefaultVault() string {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return ""
	}
	var cfg map[string]string
	if json.Unmarshal(b, &cfg) == nil {
		return cfg["default_vault"]
	}
	return ""
}

func saveDefaultVault(vault string) {
	path := configPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	cfg := map[string]string{"default_vault": vault}
	b, _ := json.Marshal(cfg)
	os.WriteFile(path, b, 0644)
}

// formatVaultTable prints vaults in a table format.
func formatVaultTable(vaults []map[string]any) {
	fmt.Printf("\n  %-20s  %-10s  %s\n", "NAME", "MEMORIES", "LAST ACTIVE")
	fmt.Printf("  %-20s  %-10s  %s\n", "────────────────────", "──────────", "───────────")
	for _, v := range vaults {
		name, _ := v["name"].(string)
		count := 0
		if c, ok := v["memory_count"].(float64); ok {
			count = int(c)
		}
		lastActive := "—"
		if la, ok := v["last_active"].(string); ok && la != "" {
			lastActive = humanizeTime(la)
		}
		fmt.Printf("  %-20s  %-10d  %s\n", name, count, lastActive)
	}
	fmt.Println()
}

// humanizeTime converts an RFC3339 timestamp to a human-friendly relative string.
func humanizeTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
