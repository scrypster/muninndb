package main

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// wantsHelp returns true if args contain -h, --help, or help.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

// printSubcommandUsage prints a consistently-formatted subcommand help block.
func printSubcommandUsage(name, summary, usage string, flags [][2]string, examples []string) {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))
	bold := func(s string) string {
		if isTTY {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if isTTY {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}

	fmt.Println()
	fmt.Printf("%s — %s\n", bold("muninn "+name), summary)
	fmt.Println()
	fmt.Printf("  Usage: %s\n", usage)

	if len(flags) > 0 {
		fmt.Println()
		fmt.Println(bold("  Flags:"))
		for _, f := range flags {
			fmt.Printf("    %-24s %s\n", f[0], f[1])
		}
	}

	if len(examples) > 0 {
		fmt.Println()
		fmt.Println(bold("  Examples:"))
		for _, e := range examples {
			fmt.Printf("    %s\n", dim(e))
		}
	}
	fmt.Println()
}

// subcommandHelp maps subcommand names to their help printers.
var subcommandHelp = map[string]func(){
	"init": func() {
		printSubcommandUsage("init", "first-time setup wizard", "muninn init [flags]",
			[][2]string{
				{"--tool <tools>", "Comma-separated: claude,claude-code,cursor,openclaw,windsurf,codex,vscode,manual"},
				{"--token <tok>", "Use a specific MCP token (skip generation)"},
				{"--no-token", "Disable token auth (open MCP endpoint)"},
				{"--no-start", "Configure tools but don't start the server"},
				{"--yes", "Accept all defaults (non-interactive)"},
			},
			[]string{
				"muninn init",
				"muninn init --tool claude,cursor --yes",
				"muninn init --tool manual --no-token",
			})
	},
	"start": func() {
		printSubcommandUsage("start", "start all services in background", "muninn start [flags]",
			[][2]string{
				{"--dev", "Serve web assets from ./web (hot reload)"},
			},
			[]string{
				"muninn start",
				"muninn start --dev",
			})
	},
	"stop": func() {
		printSubcommandUsage("stop", "stop the running server", "muninn stop", nil,
			[]string{"muninn stop"})
	},
	"restart": func() {
		printSubcommandUsage("restart", "stop and restart the server", "muninn restart", nil,
			[]string{"muninn restart"})
	},
	"status": func() {
		printSubcommandUsage("status", "show service health and ports", "muninn status", nil,
			[]string{"muninn status"})
	},
	"shell": func() {
		printSubcommandUsage("shell", "interactive memory shell", "muninn shell", nil,
			[]string{
				"muninn shell",
				"muninn          # drops into shell when server is running",
			})
	},
	"logs": func() {
		printSubcommandUsage("logs", "show recent logs and tail", "muninn logs [N] [flags]",
			[][2]string{
				{"N", "Number of recent lines to show (positional, default: 25)"},
				{"--last N", "Number of recent lines to show (default: 25)"},
				{"--level <level>", "Filter by log level: debug, info, warn, error"},
				{"--no-follow", "Print recent lines and exit (don't tail)"},
			},
			[]string{
				"muninn logs",
				"muninn logs 100",
				"muninn logs --level error",
				"muninn logs 50 --level warn --no-follow",
			})
	},
	"backup": func() {
		printSubcommandUsage("backup", "offline point-in-time backup", "muninn backup --output <dir> [flags]",
			[][2]string{
				{"--output <dir>", "Destination directory for the backup (required)"},
				{"--data-dir <dir>", "Source data directory (default: ~/.muninn/data)"},
			},
			[]string{
				"muninn backup --output /backups/muninn-2026-02-25",
			})
	},
	"upgrade": func() {
		printSubcommandUsage("upgrade", "check for and install updates", "muninn upgrade [flags]",
			[][2]string{
				{"--check", "Check only, don't install"},
			},
			[]string{
				"muninn upgrade",
				"muninn upgrade --check",
			})
	},
	"cluster": func() {
		printSubcommandUsage("cluster", "cluster management", "muninn cluster <subcommand> [flags]",
			[][2]string{
				{"info", "Display cluster topology and node status"},
				{"status", "Show cluster health and replication lag"},
				{"enable", "Enable cluster mode on this node"},
				{"disable", "Disable cluster mode on this node"},
				{"failover", "Trigger manual leader failover"},
				{"add-node", "Show instructions for adding a node"},
				{"remove-node", "Remove a node from the cluster"},
			},
			[]string{
				"muninn cluster info",
				"muninn cluster status --json",
				"muninn cluster enable --role replica",
				"muninn cluster failover --yes",
			})
	},
	"vault": func() {
		printSubcommandUsage("vault", "vault management", "muninn vault <command> [flags]",
			[][2]string{
				{"create <name> [--public]", "Create and register a new vault"},
				{"list [--pattern <glob>]", "List all vaults (with optional glob filter)"},
				{"delete <name> [--yes] [--force]", "Delete a vault and all its memories"},
				{"clear <name> [--yes] [--force]", "Remove all memories from a vault"},
				{"clone <source> <new-name>", "Clone a vault into a new vault"},
				{"merge <source> <target>", "Merge source vault into target"},
				{"export --vault <name>", "Export vault to .muninn archive"},
				{"export-markdown --vault <name>", "Export vault notes as markdown .tgz"},
				{"import <file> --vault <name>", "Import .muninn archive"},
				{"reindex-fts <name>", "Rebuild FTS index"},
				{"", ""},
				{"-u <user>", "Admin username (default: root)"},
				{"-p", "Prompt for password"},
				{"-p<password>", "Inline password (no space)"},
				{"-h <host:port>", "Server host:port (default: localhost:8475)"},
			},
			[]string{
				"muninn vault create myproject",
				"muninn vault create public-notes --public",
				"muninn vault list",
				"muninn vault list --pattern 'proof-*'",
				"muninn vault delete old-project --yes",
				"muninn vault clone production staging",
				"muninn vault export --vault mydata -o backup.muninn",
				"muninn vault export-markdown --vault mydata -o notes.tgz",
				"muninn vault delete prod-vault -u admin -p",
			})
	},
	"api-key": func() {
		printSubcommandUsage("api-key", "API key management", "muninn api-key <command> [flags]",
			[][2]string{
				{"create --vault <vault>", "Create a new API key (token shown once)"},
				{"  [--label <label>]", "Human-readable label for the key"},
				{"  [--mode full|observe]", "Access mode: full (default) or observe"},
				{"  [--expires 90d]", "Expiry duration (e.g. 90d, 365d)"},
				{"list [--vault <vault>]", "List keys (no token values returned)"},
				{"revoke <key-id>", "Revoke a key immediately"},
				{"  [--vault <vault>]", "Vault the key belongs to"},
				{"", ""},
				{"-u <user>", "Admin username (default: root)"},
				{"-p", "Prompt for password"},
				{"-p<password>", "Inline password (no space)"},
				{"-h <host:port>", "Server host:port (default: localhost:8475)"},
			},
			[]string{
				"muninn api-key create --vault default --label my-agent",
				"muninn api-key create --vault default --mode observe --expires 90d",
				"muninn api-key list",
				"muninn api-key list --vault default",
				"muninn api-key revoke A1B2C3D4",
			})
	},
	"admin": func() {
		printSubcommandUsage("admin", "admin user management", "muninn admin <command> [flags]",
			[][2]string{
				{"change-password", "Interactively change the admin password"},
				{"", ""},
				{"-u <user>", "Admin username (default: root)"},
				{"-p", "Prompt for current password"},
				{"-p<password>", "Inline current password (no space)"},
				{"-h <host:port>", "Server host:port (default: localhost:8475)"},
			},
			[]string{
				"muninn admin change-password",
				"muninn admin change-password -u root -p",
			})
	},
	"mcp": func() {
		printSubcommandUsage("mcp", "stdio→HTTP MCP proxy for OpenClaw", "muninn mcp",
			[][2]string{
				{"MUNINN_MCP_URL", "Override MCP endpoint (default: http://127.0.0.1:8750/mcp)"},
			},
			[]string{
				"muninn mcp",
				"MUNINN_MCP_URL=https://remote:8750/mcp muninn mcp",
			})
	},
	"show vaults": func() {
		printSubcommandUsage("show vaults", "list all vaults", "muninn show vaults", nil,
			[]string{"muninn show vaults"})
	},
	"completion": func() {
		printSubcommandUsage("completion", "generate shell completions", "muninn completion <bash|zsh|fish>", nil,
			[]string{
				"muninn completion bash >> ~/.bashrc",
				"muninn completion zsh >> ~/.zshrc",
				"muninn completion fish > ~/.config/fish/completions/muninn.fish",
			})
	},
	"version": func() {
		printSubcommandUsage("version", "print version", "muninn version", nil, nil)
	},
}

func printHelp() {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	bold := func(s string) string {
		if isTTY {
			return "\033[1m" + s + "\033[0m"
		}
		return s
	}
	dim := func(s string) string {
		if isTTY {
			return "\033[2m" + s + "\033[0m"
		}
		return s
	}
	cyan := func(s string) string {
		if isTTY {
			return "\033[36m" + s + "\033[0m"
		}
		return s
	}

	fmt.Println()
	fmt.Println(bold("muninn") + " — a cognitive memory database")
	fmt.Println()

	fmt.Println(bold("QUICK START"))
	fmt.Println()
	fmt.Printf("  %s       %s\n", cyan("muninn"), dim("# status check / drop into shell"))
	fmt.Printf("  %s  %s\n", cyan("muninn init"), dim("# guided setup (or re-run to reconfigure)"))
	fmt.Printf("  %s %s\n", cyan("muninn start"), dim("# start all services in background"))
	fmt.Println()

	fmt.Println(bold("COMMANDS"))
	fmt.Println()
	fmt.Printf("  %-32s %s\n", cyan("muninn init"), "First-time setup wizard — connects your AI tools")
	fmt.Printf("  %-32s %s\n", cyan("muninn start"), "Start all services in background")
	fmt.Printf("  %-32s %s\n", cyan("muninn stop"), "Stop the running server")
	fmt.Printf("  %-32s %s\n", cyan("muninn restart"), "Stop and restart")
	fmt.Printf("  %-32s %s\n", cyan("muninn status"), "Show which services are running")
	fmt.Printf("  %-32s %s\n", cyan("muninn"), "Status check / drop into interactive shell")
	fmt.Printf("  %-32s %s\n", cyan("muninn shell"), "Interactive shell (alias: bare muninn when running)")
	fmt.Printf("  %-32s %s\n", cyan("muninn logs"), "Show last 25 lines + tail")
	fmt.Printf("  %-32s %s\n", cyan("muninn logs 50"), "Show last 50 lines + tail")
	fmt.Printf("  %-32s %s\n", cyan("muninn logs --no-follow"), "Print recent lines and exit")
	fmt.Printf("  %-32s %s\n", cyan("muninn show vaults"), "List all vaults (requires server running)")
	fmt.Printf("  %-32s %s\n", cyan("muninn vault <command>"), "Vault management (create, list, delete, clear, clone, merge, export, import)")
	fmt.Printf("  %-32s %s\n", cyan("muninn api-key <command>"), "API key management (create, list, revoke)")
	fmt.Printf("  %-32s %s\n", cyan("muninn admin change-password"), "Change the admin password")
	fmt.Printf("  %-32s %s\n", cyan("muninn backup --output <dir>"), "Offline point-in-time backup (server must be stopped)")
	fmt.Printf("  %-32s %s\n", cyan("muninn cluster"), "Cluster management (info, status, failover, add-node, remove-node)")
	fmt.Printf("  %-32s %s\n", cyan("muninn mcp"), "stdio→HTTP MCP proxy (for OpenClaw)")
	fmt.Printf("  %-32s %s\n", cyan("muninn completion <shell>"), "Shell completion (bash/zsh/fish)")
	fmt.Printf("  %-32s %s\n", cyan("muninn upgrade"), "Check for and install updates")
	fmt.Printf("  %-32s %s\n", cyan("muninn help"), "Show this message")
	fmt.Println()

	fmt.Println(bold("SERVER FLAGS") + dim("  (used with: muninn start)"))
	fmt.Println()
	fmt.Printf("  %-28s %s\n", "--data <dir>", "Data directory (default: ~/.muninn/data)")
	fmt.Printf("  %-28s %s\n", "--mcp-addr <addr>", "MCP listen address (default: :8750)")
	fmt.Printf("  %-28s %s\n", "--mcp-token <tok>", "MCP bearer token for AI tool auth")
	fmt.Printf("  %-28s %s\n", "--dev", "Serve web assets from ./web (hot-reload, dev only)")
	fmt.Println()

	fmt.Println(bold("AI TOOL INTEGRATION") + dim("  (MCP — Model Context Protocol)"))
	fmt.Println()
	fmt.Println("  MuninnDB exposes an MCP server that AI tools connect to for memory.")
	fmt.Println("  Run " + cyan("muninn init") + " to configure Claude Desktop, Cursor, or Windsurf automatically.")
	fmt.Println()
	fmt.Println("  MCP endpoint: http://localhost:8750/mcp")
	fmt.Printf("  %-28s %s\n", "MUNINN_MCP_URL", "Override MCP server URL (also used by 'muninn mcp' proxy)")
	fmt.Printf("  %-28s %s\n", "MUNINNDB_DATA", "Override default data directory")
	fmt.Println()

	fmt.Println(bold("PORTS"))
	fmt.Println()
	fmt.Printf("  %-8s %s\n", ":8474", "MBP  — binary protocol")
	fmt.Printf("  %-8s %s\n", ":8475", "REST — JSON API")
	fmt.Printf("  %-8s %s\n", ":8476", "UI   — web dashboard (http://localhost:8476)")
	fmt.Printf("  %-8s %s\n", ":8750", "MCP  — AI tool integration")
	fmt.Println()

	fmt.Println(bold("EMBEDDERS") + dim("  (optional — enable semantic similarity search)"))
	fmt.Println()
	fmt.Printf("  %-28s %s\n", "MUNINN_OLLAMA_URL", "Local Ollama embed model (e.g. ollama://localhost:11434/nomic-embed-text)")
	fmt.Printf("  %-28s %s\n", "MUNINN_OPENAI_KEY", "OpenAI embeddings API key (text-embedding-3-small, 1536d)")
	fmt.Printf("  %-28s %s\n", "MUNINN_OPENAI_URL", "Optional OpenAI base URL or provider URL override")
	fmt.Printf("  %-28s %s\n", "MUNINN_VOYAGE_KEY", "Voyage AI embeddings API key (voyage-3, 1024d)")
	fmt.Printf("  %-28s %s\n", "MUNINN_COHERE_KEY", "Cohere embeddings API key (embed-v4, 1024d)")
	fmt.Printf("  %-28s %s\n", "MUNINN_GOOGLE_KEY", "Google Gemini embeddings API key (text-embedding-004, 768d)")
	fmt.Printf("  %-28s %s\n", "MUNINN_JINA_KEY", "Jina embeddings API key (jina-embeddings-v3, 1024d)")
	fmt.Printf("  %-28s %s\n", "MUNINN_MISTRAL_KEY", "Mistral embeddings API key (mistral-embed, 1024d)")
	fmt.Println()

	fmt.Println(bold("LLM ENRICHMENT") + dim("  (optional — auto-extract entities, relationships, summaries)"))
	fmt.Println()
	fmt.Println("  Set MUNINN_ENRICH_URL to enable background LLM enrichment on every new memory.")
	fmt.Println("  One provider at a time. URL scheme selects the provider:")
	fmt.Println()
	fmt.Printf("  %-28s %s\n", "Ollama (local, no key):", "MUNINN_ENRICH_URL=ollama://localhost:11434/llama3.2")
	fmt.Printf("  %-28s %s\n", "OpenAI:", "MUNINN_ENRICH_URL=openai://gpt-4o-mini")
	fmt.Printf("  %-28s %s\n", "", "MUNINN_ENRICH_API_KEY=sk-...")
	fmt.Printf("  %-28s %s\n", "Anthropic:", "MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001")
	fmt.Printf("  %-28s %s\n", "", "MUNINN_ANTHROPIC_KEY=sk-ant-...  (or MUNINN_ENRICH_API_KEY)")
	fmt.Println()
	fmt.Println("  Enrichment runs asynchronously and does not block memory writes.")
	fmt.Println()
}
