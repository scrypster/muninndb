package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z"
var version string

// muninnVersion returns the binary version string. Falls back to "dev".
func muninnVersion() string {
	if version != "" {
		return version
	}
	return "dev"
}

// toolChoice represents an AI tool in the wizard selection list.
type toolChoice struct {
	key         string // internal key: "claude", "cursor", etc.
	displayName string // shown in wizard
	configPath  string // path detected (empty if not found or manual-only)
	detected    bool   // true if config path exists on disk
	selected    bool   // true = will be configured
}

// detectInstalledTools scans known config paths and returns toolChoices.
// Detected tools are pre-selected.
func detectInstalledTools() []toolChoice {
	tools := []toolChoice{
		{key: "claude", displayName: "Claude Desktop", configPath: claudeDesktopConfigPath()},
		{key: "claude-code", displayName: "Claude Code / CLI", configPath: claudeCodeConfigPath()},
		{key: "cursor", displayName: "Cursor", configPath: cursorConfigPath()},
		{key: "openclaw", displayName: "OpenClaw", configPath: openClawConfigPath()},
		{key: "windsurf", displayName: "Windsurf", configPath: windsurfConfigPath()},
		{key: "codex", displayName: "Codex", configPath: codexConfigPath()},
		{key: "opencode", displayName: "OpenCode", configPath: openCodeConfigPath()},
		{key: "vscode", displayName: "VS Code", configPath: ""},
		{key: "manual", displayName: "Other / manual config", configPath: ""},
	}
	for i, t := range tools {
		if t.configPath != "" {
			if _, err := os.Stat(t.configPath); err == nil {
				tools[i].detected = true
				tools[i].selected = true
			}
		}
	}
	return tools
}

// runInit runs the first-time onboarding wizard (or non-interactive setup via flags).
func runInit() {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	toolFlag := fs.String("tool", "", "AI tools to configure, comma-separated: claude,claude-code,cursor,openclaw,windsurf,codex,opencode,vscode,manual")
	tokenFlag := fs.String("token", "", "Use this specific token (skip generation)")
	noToken := fs.Bool("no-token", false, "Disable token authentication (open MCP endpoint)")
	noStart := fs.Bool("no-start", false, "Skip starting the server")
	yes := fs.Bool("yes", false, "Accept all defaults non-interactively")
	fs.Usage = func() { subcommandHelp["init"]() }

	var args []string
	if len(os.Args) > 2 {
		args = os.Args[2:]
	}
	fs.Parse(args)

	mcpURL := "http://localhost:8750/mcp"
	isInteractive := term.IsTerminal(int(os.Stdin.Fd()))

	if !isInteractive && !*yes && *toolFlag == "" {
		fmt.Fprintln(os.Stderr, `muninn init requires an interactive terminal.
For non-interactive setup, use flags:

  muninn init --tool claude --yes
  muninn init --tool cursor,claude --no-token --yes
  muninn init --yes   (manual instructions only)

  --tool <tools>   Comma-separated: claude, cursor, openclaw, windsurf, codex, vscode, manual
  --token <tok>    Use specific token
  --no-token       Open MCP (no auth)
  --no-start       Skip starting server
  --yes            Accept defaults, non-interactive`)
		os.Exit(1)
	}

	if isInteractive && *toolFlag == "" && !*yes {
		runInteractiveInit(mcpURL, tokenFlag, noToken, noStart)
		return
	}

	runNonInteractiveInit(mcpURL, *toolFlag, *tokenFlag, *noToken, *noStart, *yes)
}

func runInteractiveInit(mcpURL string, tokenFlag *string, noToken *bool, noStart *bool) {
	printWelcomeBanner()

	// Step 1: Tool detection + multi-select
	tools := detectInstalledTools()
	fmt.Println("  Scanning for AI tools...")
	fmt.Println()
	fmt.Println("  Which AI tools would you like to configure?")
	fmt.Println()

	selectedTools := runToolMultiSelect(tools)

	// Step 2: Embedder selection
	embedderOptions := []selectOption{
		{label: "Local (bundled)", hint: "offline, no setup required   (recommended)"},
		{label: "Ollama", hint: "self-hosted"},
		{label: "OpenAI", hint: "cloud, requires API key"},
		{label: "Voyage", hint: "cloud, requires API key"},
		{label: "Cohere", hint: "cloud, requires API key"},
		{label: "Google (Gemini)", hint: "cloud, requires API key"},
		{label: "Jina", hint: "cloud, requires API key"},
		{label: "Mistral", hint: "cloud, requires API key"},
	}
	fmt.Println()
	fmt.Println("  Which embedder should muninn use for memory search?")
	fmt.Println()
	embedderIdx := runSingleSelect(embedderOptions, 0)
	embedderChoice := fmt.Sprintf("%d", embedderIdx+1)
	printEmbedderNote(embedderChoice)

	// Step 3: Behavior mode selection
	behaviorOptions := []selectOption{
		{label: "Autonomous", hint: "AI remembers proactively   (recommended)"},
		{label: "Prompted", hint: "only when you ask"},
		{label: "Selective", hint: "decisions & errors auto, rest on request"},
		{label: "Custom", hint: "provide your own instructions"},
	}
	fmt.Println()
	fmt.Println("  How should your AI use memory?")
	fmt.Println()
	behaviorIdx := runSingleSelect(behaviorOptions, 0)
	behaviorChoice := fmt.Sprintf("%d", behaviorIdx+1)
	behaviorMode := parseBehaviorChoice(behaviorChoice)
	var customInstructions string
	if behaviorMode == "custom" {
		fmt.Println()
		fmt.Print("  Enter your custom instructions: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		customInstructions = strings.TrimSpace(scanner.Text())
	}
	printBehaviorNote(behaviorMode, customInstructions)

	// Auto: generate token (no prompt)
	var token string
	if !*noToken {
		if *tokenFlag != "" {
			token = *tokenFlag
		} else {
			dataDir := defaultDataDir()
			var isNew bool
			var err error
			token, isNew, err = loadOrGenerateToken(dataDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\n  warning: could not generate token: %v\n", err)
			} else if isNew {
				fmt.Println()
				fmt.Println("  Generating MCP access token...  ✓")
			}
		}
	}

	// Configure selected tools
	if len(selectedTools) > 0 {
		fmt.Println()
		toolErrs := configureNamedTools(selectedTools, mcpURL, token)
		if len(toolErrs) > 0 {
			fmt.Println()
			fmt.Printf("  ⚠  %d tool(s) failed to configure — check errors above.\n", len(toolErrs))
			fmt.Println("     You can re-run: muninn init")
		}

		if hasClaudeCode(selectedTools) {
			promptClaudeMD()
		}
	}

	// Auto: start server (no "start now?" prompt)
	if !*noStart {
		fmt.Println()
		runStart(true)
	}

	// Success message
	fmt.Println()
	fmt.Println("  ────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  You're live. Your AI tools now have memory.")
	fmt.Println()
	fmt.Println("  Try it → open Claude Code or Cursor and ask:")
	fmt.Println(`    "What do you remember about me?"`)
	fmt.Println()
	fmt.Println("  Browse memories → http://localhost:8476")
	fmt.Println()
	fmt.Println("  ────────────────────────────────────────────────────")
	fmt.Println()
}

// readKey reads a single keypress from stdin in raw mode. It handles
// fragmented escape sequences by doing a follow-up read when the first
// byte is ESC (0x1b). Returns the key bytes and any read error.
func readKey(buf []byte) (int, error) {
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return n, err
	}
	if n == 1 && buf[0] == 27 {
		extra := make([]byte, 8)
		n2, _ := os.Stdin.Read(extra)
		copy(buf[1:], extra[:n2])
		n += n2
	}
	return n, nil
}

// parseArrow returns +1 (down), -1 (up), or 0 (not an arrow) from raw key bytes.
func parseArrow(buf []byte, n int) int {
	if n == 1 {
		switch buf[0] {
		case 'k', 'K':
			return -1
		case 'j', 'J':
			return +1
		}
	}
	if n >= 3 && buf[0] == 27 && buf[1] == 91 {
		switch buf[2] {
		case 65:
			return -1
		case 66:
			return +1
		}
	}
	return 0
}

// runToolMultiSelect renders an interactive checkbox list with arrow-key
// navigation and spacebar toggling. Falls back to text input for non-TTY.
func runToolMultiSelect(tools []toolChoice) []string {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return runToolMultiSelectFallback(tools)
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return runToolMultiSelectFallback(tools)
	}

	cursor := 0
	totalLines := len(tools) + 2 // tools + blank + hint

	render := func(first bool) {
		if !first {
			// Move to column 0, then up totalLines-1 lines (hint line has no trailing newline,
			// so cursor is ON the last line, not below it).
			fmt.Printf("\033[%dA\r", totalLines-1)
		}
		for i, t := range tools {
			arrow := "  "
			if i == cursor {
				arrow = "▸ "
			}
			check := "○"
			if t.selected {
				check = "●"
			}
			suffix := ""
			if t.detected && t.configPath != "" {
				suffix = "  \033[2mdetected\033[0m"
			}
			fmt.Printf("\033[K    %s%s  %s%s\r\n", arrow, check, t.displayName, suffix)
		}
		fmt.Print("\033[K\r\n")
		fmt.Print("\033[K  \033[2m↑/↓ navigate  ·  space select  ·  enter confirm\033[0m")
	}

	render(true)

	buf := make([]byte, 16)
	for {
		n, readErr := readKey(buf)
		if readErr != nil {
			break
		}

		changed := true
		switch {
		case n == 1 && buf[0] == ' ':
			tools[cursor].selected = !tools[cursor].selected
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			fmt.Printf("\033[%dA\r", totalLines-1)
			for i, t := range tools {
				check := "○"
				if t.selected {
					check = "●"
				}
				suffix := ""
				if t.detected && t.configPath != "" {
					suffix = "  \033[2mdetected\033[0m"
				}
				sel := "  "
				if i == cursor {
					sel = "▸ "
				}
				fmt.Printf("\033[K    %s%s  %s%s\r\n", sel, check, t.displayName, suffix)
			}
			fmt.Print("\033[K\r\n")
			fmt.Print("\033[K")
			term.Restore(fd, oldState)

			var keys []string
			for _, t := range tools {
				if t.selected {
					keys = append(keys, t.key)
				}
			}
			return keys
		case n == 1 && buf[0] == 3: // Ctrl+C
			fmt.Print("\r\n")
			term.Restore(fd, oldState)
			os.Exit(0)
		default:
			if dir := parseArrow(buf, n); dir != 0 {
				next := cursor + dir
				if next >= 0 && next < len(tools) {
					cursor = next
				}
			} else {
				changed = false
			}
		}

		if changed {
			render(false)
		}
	}

	term.Restore(fd, oldState)
	var keys []string
	for _, t := range tools {
		if t.selected {
			keys = append(keys, t.key)
		}
	}
	return keys
}

// runToolMultiSelectFallback handles non-interactive (non-TTY) environments
// with simple number-based input.
func runToolMultiSelectFallback(tools []toolChoice) []string {
	for i, t := range tools {
		check := "○"
		suffix := ""
		if t.selected {
			check = "●"
		}
		if t.detected && t.configPath != "" {
			suffix = "   detected  ·  " + t.configPath
		}
		fmt.Printf("    %s  %d)  %-18s%s\n", check, i+1, t.displayName, suffix)
	}
	fmt.Println()
	fmt.Print("  Enter numbers to change selection, or Enter to confirm: ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	input := strings.TrimSpace(scanner.Text())

	if input == "" {
		var keys []string
		for _, t := range tools {
			if t.selected {
				keys = append(keys, t.key)
			}
		}
		return keys
	}

	selected := map[int]bool{}
	for _, part := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ' ' }) {
		for _, c := range part {
			if c >= '1' && c <= '9' {
				n := int(c-'0') - 1
				if n < len(tools) {
					selected[n] = true
				}
			}
		}
	}
	var keys []string
	for i, t := range tools {
		if selected[i] {
			keys = append(keys, t.key)
		}
	}
	return keys
}

// selectOption describes one entry in a single-select menu.
type selectOption struct {
	label string
	hint  string
}

// runSingleSelect renders an interactive single-select menu with arrow-key
// navigation. Returns the selected index (0-based). Falls back to a numbered
// text prompt when stdin is not a terminal.
func runSingleSelect(options []selectOption, defaultIdx int) int {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return runSingleSelectFallback(options, defaultIdx)
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return runSingleSelectFallback(options, defaultIdx)
	}

	cursor := defaultIdx
	totalLines := len(options) + 2

	render := func(first bool) {
		if !first {
			fmt.Printf("\033[%dA\r", totalLines-1)
		}
		for i, o := range options {
			arrow := "     "
			if i == cursor {
				arrow = "  ▸  "
			}
			fmt.Printf("\033[K  %s%d)  %-18s·  %s\r\n", arrow, i+1, o.label, o.hint)
		}
		fmt.Print("\033[K\r\n")
		fmt.Print("\033[K  \033[2m↑/↓ navigate  ·  enter confirm\033[0m")
	}

	render(true)

	buf := make([]byte, 16)
	for {
		n, readErr := readKey(buf)
		if readErr != nil {
			break
		}

		switch {
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			fmt.Printf("\033[%dA\r", totalLines-1)
			for i, o := range options {
				arrow := "     "
				if i == cursor {
					arrow = "  ▸  "
				}
				fmt.Printf("\033[K  %s%d)  %-18s·  %s\r\n", arrow, i+1, o.label, o.hint)
			}
			fmt.Print("\033[K\r\n")
			fmt.Print("\033[K")
			term.Restore(fd, oldState)
			return cursor
		case n == 1 && buf[0] == 3: // Ctrl+C
			fmt.Print("\r\n")
			term.Restore(fd, oldState)
			os.Exit(0)
		case n == 1 && buf[0] >= '1' && buf[0] <= '9':
			idx := int(buf[0]-'0') - 1
			if idx < len(options) {
				cursor = idx
			}
		default:
			if dir := parseArrow(buf, n); dir != 0 {
				next := cursor + dir
				if next >= 0 && next < len(options) {
					cursor = next
				}
			} else {
				continue
			}
		}

		render(false)
	}

	term.Restore(fd, oldState)
	return cursor
}

// runSingleSelectFallback handles non-TTY environments with simple numbered input.
func runSingleSelectFallback(options []selectOption, defaultIdx int) int {
	for i, o := range options {
		arrow := "     "
		if i == defaultIdx {
			arrow = "  ▸  "
		}
		fmt.Printf("  %s%d)  %-18s·  %s\n", arrow, i+1, o.label, o.hint)
	}
	fmt.Println()
	fmt.Printf("  Choice [%d]: ", defaultIdx+1)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		return defaultIdx
	}
	for _, c := range input {
		if c >= '1' && c <= '9' {
			idx := int(c-'0') - 1
			if idx < len(options) {
				return idx
			}
		}
	}
	return defaultIdx
}

func printEmbedderNote(choice string) {
	switch choice {
	case "2":
		fmt.Println()
		fmt.Println("  Ollama selected. Set MUNINN_OLLAMA_URL to configure.")
		fmt.Println("  Example: MUNINN_OLLAMA_URL=ollama://localhost:11434/nomic-embed-text")
	case "3":
		fmt.Println()
		fmt.Println("  OpenAI selected. Set MUNINN_OPENAI_KEY to configure.")
		fmt.Println("  Optional: set MUNINN_OPENAI_URL for custom base URL (e.g. LocalAI).")
	case "4":
		fmt.Println()
		fmt.Println("  Voyage selected. Set MUNINN_VOYAGE_KEY to configure.")
	case "5":
		fmt.Println()
		fmt.Println("  Cohere selected. Set MUNINN_COHERE_KEY to configure.")
	case "6":
		fmt.Println()
		fmt.Println("  Google (Gemini) selected. Set MUNINN_GOOGLE_KEY to configure.")
	case "7":
		fmt.Println()
		fmt.Println("  Jina selected. Set MUNINN_JINA_KEY to configure.")
	case "8":
		fmt.Println()
		fmt.Println("  Mistral selected. Set MUNINN_MISTRAL_KEY to configure.")
	default:
		// Local bundled — works out of the box, no message needed
	}
}

func runNonInteractiveInit(mcpURL, toolStr, tokenStr string, noToken, noStart, yes bool) {
	printWelcomeBanner()

	var token string
	if !noToken {
		if tokenStr != "" {
			token = tokenStr
		} else {
			dataDir := defaultDataDir()
			var err error
			token, _, err = loadOrGenerateToken(dataDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not generate token: %v\nContinuing without token.\n", err)
			}
		}
	}

	if !noStart {
		runStart(true)
		fmt.Println()
	}

	var tools []string
	if toolStr != "" {
		for _, t := range strings.FieldsFunc(toolStr, func(r rune) bool { return r == ',' || r == ' ' }) {
			tools = append(tools, strings.ToLower(strings.TrimSpace(t)))
		}
	}

	if len(tools) > 0 {
		fmt.Println("Configuring AI tools:")
		toolErrs := configureNamedTools(tools, mcpURL, token)
		if len(toolErrs) > 0 {
			fmt.Printf("\n  ⚠  %d tool(s) failed to configure:\n", len(toolErrs))
			for _, e := range toolErrs {
				fmt.Printf("     • %s\n", e)
			}
			fmt.Println("  Re-run: muninn init --tool <toolname>")
		}

		if hasClaudeCode(tools) {
			if err := configureClaudeMD(); err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠  CLAUDE.md: %v\n", err)
			}
		}
	}

	fmt.Println()
	fmt.Println("muninn is running.")
	fmt.Println("  MCP endpoint:   http://localhost:8750/mcp")
	if token != "" {
		fmt.Println("  Token:          ~/.muninn/mcp.token")
	}
	fmt.Println("  Web UI:         http://localhost:8476")
	fmt.Println()
}

func printWelcomeBanner() {
	fmt.Println()
	fmt.Println("  ┌────────────────────────────────────────────────────┐")
	fmt.Println("  │                                                    │")
	fmt.Printf("  │   muninn  ·  cognitive memory database  %-9s│\n", muninnVersion())
	fmt.Println("  │                                                    │")
	fmt.Println("  └────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  First time here — let's get you set up.")
	fmt.Println()
}

// configureTools maps numbered selections to tool configuration.
func configureTools(selected []int, mcpURL, token string) []string {
	var errs []string
	for _, n := range selected {
		switch n {
		case 1:
			if err := configureClaudeDesktop(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Claude Desktop: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Claude Desktop: %v\n", err)
			}
		case 2:
			if err := configureCursor(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Cursor: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Cursor: %v\n", err)
			}
		case 3:
			printVSCodeInstructions(mcpURL, token)
		case 4:
			if err := configureWindsurf(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Windsurf: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Windsurf: %v\n", err)
			}
		case 5:
			printManualInstructions(mcpURL, token)
		}
	}
	return errs
}

// configureNamedTools configures AI tools by name.
func configureNamedTools(tools []string, mcpURL, token string) []string {
	var errs []string
	for _, t := range tools {
		switch t {
		case "claude", "claude-desktop":
			if err := configureClaudeDesktop(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Claude Desktop: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Claude Desktop: %v\n", err)
			}
		case "claude-code", "claudecode":
			if err := configureClaudeCode(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Claude Code: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Claude Code: %v\n", err)
			}
		case "cursor":
			if err := configureCursor(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Cursor: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Cursor: %v\n", err)
			}
		case "vscode", "vs-code":
			printVSCodeInstructions(mcpURL, token)
		case "windsurf":
			if err := configureWindsurf(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Windsurf: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Windsurf: %v\n", err)
			}
		case "openclaw":
			if err := configureOpenClaw(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("OpenClaw: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ OpenClaw: %v\n", err)
			}
			// Also install the SKILL.md so OpenClaw knows how to use MuninnDB tools.
			if err := configureOpenClawSkill(); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: OpenClaw skill: %v\n", err)
			}
		case "codex":
			if err := configureCodex(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("Codex: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ Codex: %v\n", err)
			}
		case "opencode":
			if err := configureOpenCode(mcpURL, token); err != nil {
				errs = append(errs, fmt.Sprintf("OpenCode: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ OpenCode: %v\n", err)
			}
		case "manual", "other":
			printManualInstructions(mcpURL, token)
		default:
			fmt.Fprintf(os.Stderr, "  unknown tool: %q (use: claude, claude-code, cursor, vscode, windsurf, openclaw, opencode, codex, manual)\n", t)
		}
	}
	return errs
}

// parseToolNumbers parses "1 2 3" or "1,2,3" into deduplicated ints 1-9.
func parseToolNumbers(input string) []int {
	seen := map[int]bool{}
	var result []int
	for _, part := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ' ' }) {
		n := 0
		for _, c := range part {
			if c >= '1' && c <= '9' {
				n = int(c - '0')
				break
			}
		}
		if n > 0 && !seen[n] {
			seen[n] = true
			result = append(result, n)
		}
	}
	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseBehaviorChoice(choice string) string {
	switch choice {
	case "2":
		return "prompted"
	case "3":
		return "selective"
	case "4":
		return "custom"
	default:
		return "autonomous"
	}
}

func printBehaviorNote(mode, customInstructions string) {
	fmt.Println()
	switch mode {
	case "prompted":
		fmt.Println("  Behavior: prompted — AI will only remember when asked.")
	case "selective":
		fmt.Println("  Behavior: selective — decisions & errors auto-remembered.")
	case "custom":
		fmt.Println("  Behavior: custom — using your provided instructions.")
	default:
		fmt.Println("  Behavior: autonomous — AI will proactively remember.")
	}
	fmt.Println()
	fmt.Println("  To apply this setting, run:")
	cmd := fmt.Sprintf("    muninn vault config default --behavior %s", mode)
	fmt.Println(cmd)
	if mode == "custom" && customInstructions != "" {
		fmt.Printf("    muninn vault config default --behavior-instructions %q\n", customInstructions)
	}
}

// hasClaudeCode returns true if "claude-code" or "claudecode" is in the tool list.
func hasClaudeCode(tools []string) bool {
	for _, t := range tools {
		if t == "claude-code" || t == "claudecode" {
			return true
		}
	}
	return false
}

// promptClaudeMD asks interactively whether to configure CLAUDE.md for MuninnDB memory.
func promptClaudeMD() {
	fmt.Println()
	fmt.Print("  Configure CLAUDE.md to prefer MuninnDB for memory? [Y/n]: ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))

	if answer == "" || answer == "y" || answer == "yes" {
		if err := configureClaudeMD(); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠  CLAUDE.md: %v\n", err)
		}
	} else {
		printClaudeMDInstructions()
	}
}
