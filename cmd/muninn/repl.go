package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

type replState struct {
	vault         string // current vault context (empty = no vault selected)
	mcpURL        string // e.g. "http://127.0.0.1:8750"
	cmdCount      int    // total commands run this session (for tip rotation)
	firstRun      bool   // true if no config file existed at shell start
	sessionCookie string // for REST API calls requiring admin auth
}

func (r *replState) prompt() string {
	if r.vault == "" {
		return "muninn> "
	}
	return r.vault + "> "
}

// parseReplInput parses a REPL input line into (command, args).
// Two-word commands like "show vaults" are returned as the full command string.
// "quit" is normalized to "exit".
func parseReplInput(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", nil
	}
	if strings.ToLower(line) == "quit" {
		return "exit", nil
	}

	// Two-word commands checked first
	twoWord := []string{
		"show vaults", "show memories", "show stats", "show contradictions",
	}
	lower := strings.ToLower(line)
	for _, tw := range twoWord {
		if lower == tw {
			return tw, nil
		}
	}

	// Single-word or word + rest
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToLower(parts[0])
	if len(parts) == 1 {
		return cmd, nil
	}
	return cmd, []string{parts[1]}
}

func runShell() {
	mcpURL := os.Getenv("MUNINNDB_MCP_URL")
	if mcpURL == "" {
		mcpURL = "http://127.0.0.1:8750"
	}

	// Detect first run (no config file = user hasn't used 'use <vault>' before)
	_, configErr := os.Stat(configPath())
	isFirstRun := os.IsNotExist(configErr)

	r := &replState{
		mcpURL:   mcpURL,
		firstRun: isFirstRun,
	}
	r.vault = loadDefaultVault()

	// Check server is up
	if !mcpHealthCheck(r.mcpURL) {
		fmt.Fprintln(os.Stderr, "muninn is not running.")
		fmt.Fprintln(os.Stderr, "Start it with: muninn start")
		os.Exit(1)
	}

	// Auto-authenticate with default credentials; prompt only if that fails
	sessionCookie, authErr := autoAuth("http://127.0.0.1:8476")
	if authErr != nil {
		// Default creds failed — prompt once
		fmt.Print("Username: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		username := strings.TrimSpace(scanner.Text())

		fmt.Print("Password: ")
		passBytes, pwErr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if pwErr != nil {
			fmt.Fprintln(os.Stderr, "failed to read password:", pwErr)
			os.Exit(1)
		}

		if err := shellValidateAdmin(username, string(passBytes)); err != nil {
			fmt.Fprintln(os.Stderr, "Authentication failed.")
			os.Exit(1)
		}
		// Refresh cookie after explicit login
		sessionCookie, _ = autoAuth("http://127.0.0.1:8476")
	}

	r.sessionCookie = sessionCookie

	scanner := bufio.NewScanner(os.Stdin)

	if r.firstRun {
		fmt.Println("  You're in the MuninnDB interactive shell.")
		fmt.Println("  Your AI tools store and retrieve memories here automatically.")
		fmt.Println()
		fmt.Println("  Tips:")
		fmt.Println("    • show vaults       — see your memory namespaces")
		fmt.Println("    • use <vault>        — select a vault to work in")
		fmt.Println("    • show memories      — browse recent memories")
		fmt.Println("    • search <query>     — semantic search across memories")
		fmt.Println("    • help               — all available commands")
		fmt.Println()
	} else {
		fmt.Println("Type 'help' for commands, 'exit' to quit.")
		if r.vault != "" {
			fmt.Printf("Using vault: %s\n", r.vault)
		}
		fmt.Println()
	}

	for {
		fmt.Print(r.prompt())
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if line == "" {
			continue
		}
		if r.handleCommand(line) {
			break
		}
	}
}

// handleCommand executes a parsed command. Returns true if the shell should exit.
func (r *replState) handleCommand(line string) bool {
	cmd, args := parseReplInput(line)
	r.cmdCount++

	switch cmd {
	case "exit":
		fmt.Println("Bye!")
		return true

	case "help":
		r.printShellHelp()

	case "use":
		if len(args) == 0 {
			fmt.Println("  Usage:   use <vault-name>")
			fmt.Println("  Example: use personal")
			fmt.Println("  Tip:     run 'show vaults' to see available vaults")
			return false
		}
		r.vault = args[0]
		saveDefaultVault(r.vault)
		fmt.Printf("Switched to vault '%s'\n", r.vault)

	case "show vaults":
		r.cmdShowVaults()

	case "show memories":
		r.requireVault(r.cmdShowMemories)

	case "show stats":
		r.cmdShowStats()

	case "show contradictions":
		r.requireVault(r.cmdShowContradictions)

	case "search":
		if len(args) == 0 {
			fmt.Println("  Usage:   search <query> [--mode semantic|recent|balanced|deep] [--since ISO8601] [--before ISO8601] [--hops N] [--profile default|causal|confirmatory|adversarial|structural]")
			fmt.Println("  Example: search authentication decisions --since 2026-01-01 --mode semantic")
			fmt.Println("  Tip:     use natural language — semantic search, not keyword matching")
			return false
		}
		r.requireVault(func() { r.cmdSearch(args[0]) })

	case "get":
		if len(args) == 0 {
			fmt.Println("  Usage:   get <id>")
			fmt.Println("  Example: get 01JFXX4KZMB3E9QV7P")
			fmt.Println("  Tip:     run 'show memories' to see IDs")
			return false
		}
		r.requireVault(func() { r.cmdGet(args[0]) })

	case "forget":
		if len(args) == 0 {
			fmt.Println("  Usage:   forget <id>")
			fmt.Println("  Example: forget 01JFXX4KZMB3E9QV7P")
			fmt.Println("  Tip:     forget is reversible — restore from the web UI")
			return false
		}
		r.requireVault(func() { r.cmdForget(args[0]) })

	case "":
		// empty line — do nothing

	default:
		suggestion := suggestCommand(cmd)
		if suggestion != "" {
			fmt.Printf("Unknown command: %q  (did you mean '%s'?)\n", cmd, suggestion)
		} else {
			fmt.Printf("Unknown command: %q\n", cmd)
		}
		fmt.Println("Type 'help' for available commands.")
	}

	// Rotate tip every 7 commands (after first-run tips are shown)
	if !r.firstRun && r.cmdCount > 0 && r.cmdCount%7 == 0 {
		r.printRotatingTip()
	}

	return false
}

func (r *replState) requireVault(fn func()) {
	if r.vault == "" {
		fmt.Println("  No vault selected.")
		fmt.Println("  List vaults:  show vaults")
		fmt.Println("  Select one:   use <vault-name>")
		return
	}
	fn()
}

func (r *replState) printShellHelp() {
	fmt.Print(`
Available commands:

  Vault context:
    show vaults              List all vaults
    use <vault>              Switch to a vault (persists across sessions)

  Memory operations  (require an active vault):
    show memories            Browse recent memories in current vault
    show contradictions      List conflicting memories detected by MuninnDB
    search <query> [flags]   Semantic search — use natural language
                             Flags: --since ISO8601, --before ISO8601,
                                    --mode actr|cgdn|additive,
                                    --hops N,
                                    --profile default|causal|confirmatory|adversarial|structural
    get <id>                 Fetch a memory by its ID
    forget <id>              Soft-delete a memory (recoverable)

  Server:
    show stats               Server health, ports, and worker status
    exit / quit              Exit the shell

  Tips:
    • Memories are stored by your AI tools — no manual entry needed
    • 'search' uses semantic similarity, not keyword matching
    • 'forget' is a soft-delete — the memory can be restored from the web UI

`)
}

func (r *replState) printRotatingTip() {
	tips := []string{
		"Tip: 'search' uses semantic similarity — try natural language queries.",
		"Tip: 'show contradictions' surfaces memories that conflict with each other.",
		"Tip: Switch between projects with 'use <vault-name>'.",
		"Tip: Open http://127.0.0.1:8476 to browse the memory graph visually.",
		"Tip: 'forget' is reversible — restore memories from Settings in the web UI.",
		"Tip: The web UI shows memory confidence, decay, and association graphs.",
	}
	idx := (r.cmdCount / 7) % len(tips)
	fmt.Println()
	fmt.Println(" ", tips[idx])
}

// shellValidateAdmin validates admin credentials against the running UI server's
// login endpoint (POST http://127.0.0.1:8476/api/auth/login).
// Returns nil on success, non-nil on auth failure or network error.
func shellValidateAdmin(username, password string) error {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post("http://127.0.0.1:8476/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("invalid credentials (status %d)", resp.StatusCode)
	}
	return nil
}

// autoAuth attempts to log in with default credentials.
// Returns the session cookie value on success.
func autoAuth(uiBase string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"username": "root",
		"password": "password",
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(uiBase+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("connect to server: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth failed (status %d)", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "session" || c.Name == "muninn_session" {
			return c.Value, nil
		}
	}
	// No cookie found but 200 — auth succeeded without session cookie
	return "", nil
}


// knownCommands lists all valid REPL commands for typo suggestion.
var knownCommands = []string{
	"exit", "quit", "help", "use",
	"show vaults", "show memories", "show stats", "show contradictions",
	"search", "get", "forget",
}

// suggestCommand returns the closest known command to input if within edit distance 2,
// or "" if no good match is found. Uses simple Levenshtein distance.
func suggestCommand(input string) string {
	best := ""
	bestDist := 3 // only suggest if distance ≤ 2
	for _, cmd := range knownCommands {
		// Compare against just the first word of multi-word commands
		firstWord := strings.SplitN(cmd, " ", 2)[0]
		d := levenshtein(input, firstWord)
		if d < bestDist {
			bestDist = d
			best = cmd
		}
	}
	return best
}

// levenshtein computes the Levenshtein edit distance between two strings.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr := make([]int, len(rb)+1)
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				curr[j] = prev[j-1]
			} else {
				curr[j] = 1 + minOf3(prev[j], curr[j-1], prev[j-1])
			}
		}
		prev = curr
	}
	return prev[len(rb)]
}

func minOf3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
