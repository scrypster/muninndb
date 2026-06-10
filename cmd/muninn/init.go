package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	tlsCert := fs.String("tls-cert", "", "Path to TLS certificate (PEM) — serve clients over https")
	tlsKey := fs.String("tls-key", "", "Path to TLS private key (PEM)")
	fs.Usage = func() { subcommandHelp["init"]() }

	var args []string
	if len(os.Args) > 2 {
		args = os.Args[2:]
	}
	fs.Parse(args)

	// Honor ~/.muninn/muninn.env like the daemon does (shell env still wins):
	// a persisted MUNINN_TLS_CERT/KEY or MUNINN_MCP_URL must shape the configs
	// this run generates, or a re-run on a TLS install would write http URLs.
	loadEnvFile()

	isInteractive := term.IsTerminal(int(os.Stdin.Fd()))

	if !isInteractive && !*yes && *toolFlag == "" {
		fmt.Fprintln(os.Stderr, `muninn init requires an interactive terminal.
For non-interactive setup, use flags:

  muninn init --tool claude --yes
  muninn init --tool cursor,claude --no-token --yes
  muninn init --tool claude --tls-cert cert.pem --tls-key key.pem --yes
  muninn init --yes   (manual instructions only)

  --tool <tools>   Comma-separated: claude, claude-code, cursor, openclaw, windsurf, codex, opencode, vscode, manual
  --token <tok>    Use specific token
  --no-token       Open MCP (no auth)
  --no-start       Skip starting server
  --tls-cert <p>   TLS certificate (PEM) — serve clients over https
  --tls-key <p>    TLS private key (PEM)
  --yes            Accept defaults, non-interactive`)
		os.Exit(1)
	}

	if isInteractive && *toolFlag == "" && !*yes {
		runInteractiveInit(tokenFlag, noToken, noStart, *tlsCert, *tlsKey)
		return
	}

	runNonInteractiveInit(*toolFlag, *tokenFlag, *noToken, *noStart, *yes, *tlsCert, *tlsKey)
}

// validateTLSPair reports whether a cert/key pair is usable. Both empty is valid
// (no TLS); exactly one set is an error; both set must load as a key pair.
func validateTLSPair(cert, key string) error {
	if (cert == "") != (key == "") {
		return fmt.Errorf("--tls-cert and --tls-key must both be set (or neither)")
	}
	if cert == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		return fmt.Errorf("invalid TLS cert/key: %w", err)
	}
	return nil
}

// tlsSchemeFromEnv reports "https" when both TLS env vars are set, else "http".
func tlsSchemeFromEnv() string {
	return schemeFor(os.Getenv("MUNINN_TLS_CERT"), os.Getenv("MUNINN_TLS_KEY"))
}

// daemonRunning reports whether a muninn daemon is currently running, going by
// the PID file in the data directory.
func daemonRunning() bool {
	pid, err := readPID(filepath.Join(defaultDataDir(), "muninn.pid"))
	return err == nil && isProcessRunning(pid)
}

// clientScheme is the scheme written into generated client configs and printed
// URLs: https when this process's TLS env says so (a TLS setup happening right
// now — runInit loads muninn.env, so persisted TLS counts too); otherwise the
// scheme the RUNNING daemon recorded in the muninn.addrs sidecar, so re-running
// init against an already-TLS daemon never downgrades configs to http. The
// sidecar is consulted only while the daemon is alive — a stale file left by a
// crash must not make a fresh plaintext daemon advertise https.
func clientScheme() string {
	if s := tlsSchemeFromEnv(); s == "https" {
		return s
	}
	if daemonRunning() {
		if addrs, err := readAddrsFile(defaultDataDir()); err == nil && addrs.Scheme != "" {
			return addrs.Scheme
		}
	}
	return "http"
}

// normalizeTLSPath expands a leading "~/" and makes the path absolute, so the
// value survives being handed to the forked daemon and persisted into
// muninn.env — a relative path would only resolve from init's cwd and would
// break every later 'muninn start' from anywhere else. Best-effort: on any
// resolution error the input is returned unchanged.
func normalizeTLSPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// clientMCPURL is the MCP endpoint written into generated AI-tool configs.
// MUNINN_MCP_URL (an operator-advertised, e.g. routable/remote, URL) wins;
// otherwise the scheme follows clientScheme so a TLS deployment gets https.
func clientMCPURL() string {
	if v := os.Getenv("MUNINN_MCP_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return clientScheme() + "://127.0.0.1:" + defaultMCPPort + "/mcp"
}

// clientUIURL is the Web UI URL shown to the operator, scheme-aware.
func clientUIURL() string {
	return clientScheme() + "://127.0.0.1:8476"
}

// clientRESTURL is the REST API base written into generated AI-tool guides
// (e.g. the OpenClaw SKILL.md curl examples), scheme-aware.
func clientRESTURL() string {
	return clientScheme() + "://127.0.0.1:8475"
}

// ambientTLSPair returns the env-configured TLS pair when it is set and valid,
// else ("",""). Used so a pair inherited from the shell or muninn.env enables
// the same persistence and messaging as explicit flags — without it, an
// ambient-TLS init produced https configs and an https daemon but never
// persisted the pair, so the next clean-shell start silently served http.
// An invalid ambient pair is left alone: the daemon validates it fatally
// itself, and init must not turn an operator's env into a hard error.
func ambientTLSPair() (cert, key string) {
	cert, key = os.Getenv("MUNINN_TLS_CERT"), os.Getenv("MUNINN_TLS_KEY")
	if cert == "" || key == "" || validateTLSPair(cert, key) != nil {
		return "", ""
	}
	return cert, key
}

// enableTLSFromFlagsOrPrompt resolves TLS for the interactive wizard. Explicit
// flags are validated strictly — a bad --tls-cert/--tls-key pair is fatal,
// matching the non-interactive path and the daemon, so an explicit TLS request
// is never silently downgraded to plaintext http. With no flags, a valid
// ambient env pair is adopted; otherwise it prompts (which may be declined).
// On success it sets the TLS env (paths normalized to absolute) so the
// upcoming runStart's forked daemon serves https. Returns the resolved
// cert/key paths, or ("","") when TLS is not enabled.
func enableTLSFromFlagsOrPrompt(certFlag, keyFlag string) (cert, key string) {
	cert, key = certFlag, keyFlag
	if cert == "" && key == "" {
		if cert, key = ambientTLSPair(); cert != "" {
			fmt.Println()
			fmt.Println("  Using the TLS certificate from MUNINN_TLS_CERT / MUNINN_TLS_KEY.")
		} else {
			cert, key = promptTLS()
		}
	} else {
		// Normalize before validating so "~/cert.pem" and relative flag paths
		// resolve the same way the prompt path does, rather than fatally failing.
		cert, key = normalizeTLSPath(cert), normalizeTLSPath(key)
		if err := validateTLSPair(cert, key); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	if cert == "" {
		return "", ""
	}
	cert, key = normalizeTLSPath(cert), normalizeTLSPath(key)
	os.Setenv("MUNINN_TLS_CERT", cert)
	os.Setenv("MUNINN_TLS_KEY", key)
	fmt.Println("  ✓ TLS enabled — clients will connect over https")
	warnIfCertNotLoopback(cert)
	return cert, key
}

// warnIfCertNotLoopback notes when the certificate covers neither 127.0.0.1
// nor localhost. This CLI skips verification on loopback, but the generated
// tool configs point external MCP/REST clients at https://127.0.0.1 — and
// those clients DO verify, so they would reject the connection.
func warnIfCertNotLoopback(certPath string) {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return
	}
	// Find the leaf CERTIFICATE block: a combined PEM may put the private key
	// first, and pem.Decode returns blocks in file order.
	var c *x509.Certificate
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if parsed, err := x509.ParseCertificate(block.Bytes); err == nil {
			c = parsed
			break
		}
	}
	if c == nil {
		return
	}
	if c.VerifyHostname("127.0.0.1") == nil || c.VerifyHostname("localhost") == nil {
		return
	}
	fmt.Println("  ⚠  The certificate covers neither 127.0.0.1 nor localhost.")
	fmt.Println("     Generated client configs use https://127.0.0.1 — clients that verify")
	fmt.Println("     certificates will reject it. Reissue the cert with an IP SAN for")
	fmt.Println("     127.0.0.1, or set MUNINN_MCP_URL to a URL the cert does cover.")
}

// promptTLS asks whether to serve TLS and, if so, collects and validates a
// cert/key pair (one retry; "~/" is expanded). Returns ("","") when TLS is
// declined or the pair can't be validated — always saying so, since the user
// answered yes and would otherwise believe TLS is on.
func promptTLS() (cert, key string) {
	r := bufio.NewReader(os.Stdin)
	fmt.Println()
	fmt.Print("  Serve clients over TLS (https)? [y/N]: ")
	ans, _ := r.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
		return "", ""
	}
	for attempt := 0; attempt < 2; attempt++ {
		fmt.Print("    TLS certificate path (PEM): ")
		c, _ := r.ReadString('\n')
		fmt.Print("    TLS private key path (PEM): ")
		k, _ := r.ReadString('\n')
		c, k = strings.TrimSpace(c), strings.TrimSpace(k)
		if c == "" && k == "" {
			break // fall through to the explicit not-enabled warning
		}
		c, k = normalizeTLSPath(c), normalizeTLSPath(k)
		if err := validateTLSPair(c, k); err != nil {
			fmt.Printf("    ⚠  %v\n", err)
			continue
		}
		return c, k
	}
	fmt.Println("    ⚠  TLS not enabled — the server will use plaintext http.")
	fmt.Println("       Configure later: muninn init --tls-cert <cert> --tls-key <key>")
	return "", ""
}

// persistTLSEnv writes the TLS cert/key paths into muninn.env as active lines
// so future restarts keep https. Both vars go in one atomic write — the pair
// can never be persisted half, which would make every later daemon start fail
// its exactly-one-of check. Best-effort: a failure leaves the current daemon's
// TLS intact (it came from the inherited env), but is reported loudly because
// it means TLS will not survive a restart. A no-op on empty input, so an empty
// pair can never be written as active.
func persistTLSEnv(cert, key string) {
	if cert == "" || key == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("init: could not resolve home for muninn.env TLS persistence", "error", err)
		return
	}
	path := filepath.Join(home, envFileName)
	err = upsertEnvFileVars(path, [][2]string{
		{"MUNINN_TLS_CERT", cert},
		{"MUNINN_TLS_KEY", key},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠  could not persist TLS settings to %s: %v\n", path, err)
		fmt.Fprintln(os.Stderr, "     TLS will NOT survive a restart — add MUNINN_TLS_CERT and MUNINN_TLS_KEY there manually.")
	}
}

func runInteractiveInit(tokenFlag *string, noToken *bool, noStart *bool, tlsCert, tlsKey string) {
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

	// Step 4: TLS. Flags pre-fill (and skip) the prompt; otherwise ask. The env
	// must be set here — before configuring tool URLs and before runStart, whose
	// forked daemon inherits it.
	tlsCertPath, tlsKeyPath := enableTLSFromFlagsOrPrompt(tlsCert, tlsKey)
	tlsEnabled := tlsCertPath != ""

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

	// Configure selected tools. Derive the URL now that TLS env (if any) is set.
	mcpURL := clientMCPURL()
	if len(selectedTools) > 0 {
		fmt.Println()
		toolErrs := configureNamedTools(selectedTools, mcpURL, token, behaviorMode)
		if len(toolErrs) > 0 {
			fmt.Println()
			fmt.Printf("  ⚠  %d tool(s) failed to configure — check errors above.\n", len(toolErrs))
			fmt.Println("     You can re-run: muninn init")
		}

		if hasClaudeCode(selectedTools) {
			promptClaudeMD(behaviorMode)
		}
	}

	// Auto: start server (no "start now?" prompt)
	if !*noStart {
		fmt.Println()
		if err := runStart(true); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon did not start cleanly: %v\n", err)
		}
		// runStart is a no-op on an already-running daemon — if that daemon's
		// scheme doesn't match what we just configured, say so instead of
		// printing a success summary for a TLS setup that isn't serving yet.
		warnIfDaemonSchemeMismatch()
		// Point our admin/UI base URLs at the daemon we just started (scheme + port
		// from muninn.addrs) so the loopback login + plasticity calls below reach it
		// under TLS or a non-default port instead of failing against http://:8475.
		alignLocalAdminBasesToDaemon()
		// Persist the behavior choice to the default vault now that the server is up.
		// Retries once on failure; falls back to printing the manual command.
		applyBehaviorToVault(behaviorMode, customInstructions)
	}

	// Write ~/.muninn/muninn.env template (no-op if file already exists).
	embedProviders := []string{"local", "ollama", "openai", "voyage", "cohere", "google", "jina", "mistral"}
	embedProvider := "local"
	if embedderIdx >= 0 && embedderIdx < len(embedProviders) {
		embedProvider = embedProviders[embedderIdx]
	}
	if created, envErr := writeEnvFile(embedProvider, ""); envErr != nil {
		slog.Warn("init: could not write muninn.env", "error", envErr)
	} else if created {
		home, _ := os.UserHomeDir()
		fmt.Printf("  ✓ Config template written to %s\n", filepath.Join(home, ".muninn", "muninn.env"))
		fmt.Println("  Edit this file to configure MuninnDB without shell exports.")
	}
	// Persist TLS into muninn.env (active) so future restarts keep https. After
	// writeEnvFile so the template exists with the user's embedder, not clobbered.
	if tlsEnabled {
		persistTLSEnv(tlsCertPath, tlsKeyPath)
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
	fmt.Printf("  Browse memories → %s\n", clientUIURL())
	if tlsEnabled {
		fmt.Printf("  MCP endpoint    → %s\n", mcpURL)
		fmt.Println("  Remote clients  → set MUNINN_MCP_URL to this server's routable https URL before 'muninn init'")
		fmt.Println("  Client trust    → MCP/REST clients verify this certificate; distribute your CA")
		fmt.Println("                    file to them (curl: --cacert <ca.crt>; GUI tools: OS trust store)")
	}
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

func runNonInteractiveInit(toolStr, tokenStr string, noToken, noStart, yes bool, tlsCert, tlsKey string) {
	printWelcomeBanner()

	// TLS must be decided before runStart — the forked daemon inherits the env.
	// Normalize before validating so "~/cert.pem" / relative flag paths resolve
	// instead of fatally failing (and so the persisted path is absolute).
	if tlsCert != "" || tlsKey != "" {
		tlsCert, tlsKey = normalizeTLSPath(tlsCert), normalizeTLSPath(tlsKey)
	}
	if err := validateTLSPair(tlsCert, tlsKey); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if tlsCert == "" {
		// No flags: adopt a valid ambient env pair (shell or muninn.env) so it
		// gets the same persistence and messaging as an explicit one.
		tlsCert, tlsKey = ambientTLSPair()
		tlsCert, tlsKey = normalizeTLSPath(tlsCert), normalizeTLSPath(tlsKey)
	}
	tlsEnabled := tlsCert != ""
	if tlsEnabled {
		os.Setenv("MUNINN_TLS_CERT", tlsCert)
		os.Setenv("MUNINN_TLS_KEY", tlsKey)
		warnIfCertNotLoopback(tlsCert)
	}

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
		if err := runStart(true); err != nil {
			fmt.Fprintf(os.Stderr, "warning: daemon did not start cleanly: %v\n", err)
		}
		// runStart is a no-op on an already-running daemon — flag a scheme
		// mismatch instead of printing a success summary it doesn't serve yet.
		warnIfDaemonSchemeMismatch()
		fmt.Println()
	}

	var tools []string
	if toolStr != "" {
		for _, t := range strings.FieldsFunc(toolStr, func(r rune) bool { return r == ',' || r == ' ' }) {
			tools = append(tools, strings.ToLower(strings.TrimSpace(t)))
		}
	}

	mcpURL := clientMCPURL()
	if len(tools) > 0 {
		fmt.Println("Configuring AI tools:")
		toolErrs := configureNamedTools(tools, mcpURL, token, "")
		if len(toolErrs) > 0 {
			fmt.Printf("\n  ⚠  %d tool(s) failed to configure:\n", len(toolErrs))
			for _, e := range toolErrs {
				fmt.Printf("     • %s\n", e)
			}
			fmt.Println("  Re-run: muninn init --tool <toolname>")
		}

		if hasClaudeCode(tools) {
			if err := configureClaudeMD(""); err != nil {
				fmt.Fprintf(os.Stderr, "  ⚠  CLAUDE.md: %v\n", err)
			}
		}
	}

	// Write ~/.muninn/muninn.env template with all vars commented (no-op if exists).
	if _, envErr := writeEnvFile("local", ""); envErr != nil {
		slog.Warn("init: could not write muninn.env", "error", envErr)
	}
	if tlsEnabled {
		persistTLSEnv(tlsCert, tlsKey)
	}

	fmt.Println()
	fmt.Println("muninn is running.")
	fmt.Printf("  MCP endpoint:   %s\n", mcpURL)
	if token != "" {
		fmt.Println("  Token:          ~/.muninn/mcp.token")
	}
	fmt.Printf("  Web UI:         %s\n", clientUIURL())
	if tlsEnabled {
		fmt.Println("  Remote clients: set MUNINN_MCP_URL to this server's routable https URL before 'muninn init'")
		fmt.Println("  Client trust:   MCP/REST clients verify this certificate; distribute your CA")
		fmt.Println("                  file to them (curl: --cacert <ca.crt>; GUI tools: OS trust store)")
	}
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
// behaviorMode controls the ## Usage pattern section in SKILL.md / CLAUDE.md.
// Pass "" to use the default (autonomous) behavior.
func configureNamedTools(tools []string, mcpURL, token, behaviorMode string) []string {
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
			// OpenClaw has no native MCP support — do not touch openclaw.json.
			// Install only the SKILL.md so OpenClaw recognizes and loads the skill.
			// Also remove any provider.mcpServers.muninn entry written by v0.3.13-alpha,
			// which caused a fatal "Unrecognized key: provider" startup error.
			cleanupOpenClawBadConfig()
			if err := configureOpenClawSkill(behaviorMode); err != nil {
				errs = append(errs, fmt.Sprintf("OpenClaw skill: %v", err))
				fmt.Fprintf(os.Stderr, "  ✗ OpenClaw skill: %v\n", err)
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
		if customInstructions != "" {
			fmt.Printf("  behavior-instructions: %s\n", customInstructions)
		}
	default:
		fmt.Println("  Behavior: autonomous — AI will proactively remember.")
	}
}

// warnIfDaemonSchemeMismatch tells the operator to restart when this run just
// enabled TLS but the already-running daemon is still serving plaintext http.
// runStart returns early on a live daemon, so enabling TLS on a running http
// daemon (a flow promptTLS's own fallback hint recommends) would otherwise
// print a success summary while the daemon keeps serving http.
//
// Only the https-wanted / http-served direction warrants a warning: the
// reverse (a running https daemon we didn't reconfigure) is exactly what
// clientScheme adopts from the sidecar, so the generated configs already match
// the daemon — telling the operator to "restart" there would push them toward
// the http the configs are NOT using.
func warnIfDaemonSchemeMismatch() {
	if tlsSchemeFromEnv() != "https" || !daemonRunning() {
		return
	}
	addrs, err := readAddrsFile(defaultDataDir())
	if err != nil {
		return
	}
	have := addrs.Scheme
	if have == "" {
		have = "http"
	}
	if have != "https" {
		fmt.Println()
		fmt.Printf("  ⚠  The daemon is already running and serves %s, but TLS was just enabled.\n", have)
		fmt.Println("     Restart it to serve https: muninn restart")
	}
}

// alignLocalAdminBasesToDaemon points the package-level admin/UI base URLs at the
// running daemon's actual scheme + port (from the muninn.addrs sidecar) so the
// wizard's own loopback admin calls (loginAdmin, plasticity) reach a daemon it
// just started with TLS or on a non-default port. Operator overrides
// (MUNINNDB_ADMIN_URL / MUNINNDB_UI_URL) are respected and left untouched.
func alignLocalAdminBasesToDaemon() {
	addrs, err := readAddrsFile(defaultDataDir())
	if err != nil {
		return // sidecar unreadable — keep the compiled-in http defaults
	}
	scheme := addrs.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if os.Getenv("MUNINNDB_ADMIN_URL") == "" {
		if _, port, err := net.SplitHostPort(addrs.RestAddr); err == nil && port != "" {
			vaultAdminBase = scheme + "://127.0.0.1:" + port
		}
	}
	if os.Getenv("MUNINNDB_UI_URL") == "" {
		if _, port, err := net.SplitHostPort(addrs.UIAddr); err == nil && port != "" {
			vaultUIBase = scheme + "://127.0.0.1:" + port
		}
	}
}

// applyBehaviorToVault persists the chosen behavior mode to the default vault's
// plasticity config via the admin API. Called after runStart so the server is up.
//
// Tries once immediately, retries after 2 s if the first attempt fails (the daemon
// may still be initializing). On final failure it prints the manual command and
// continues — a behavior-set failure must never abort the init wizard.
//
// The PUT is idempotent: calling with the same mode twice is safe.
func applyBehaviorToVault(mode, customInstructions string) {
	doApply := func() error {
		// Attempt default-credential auto-login (root/password) — works on fresh installs.
		// Do not prompt interactively; this is a background step inside init.
		if err := loginAdmin("root", "password"); err != nil {
			return err
		}

		plasticityURL := fmt.Sprintf("%s/api/admin/vault/default/plasticity", vaultAdminBase)
		client := &http.Client{Timeout: 5 * time.Second}
		// ToLower: the scheme is case-insensitive; keep this consistent with
		// isLoopbackURL (which lowercases via url.Parse).
		if strings.HasPrefix(strings.ToLower(plasticityURL), "https://") && isLoopbackURL(plasticityURL) {
			// Loopback https: skip cert verification — the connection never leaves
			// this machine. Off-host stays verified. Unifies to httpClientForURL
			// once #468 lands.
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			}
		}

		// GET current config so we merge rather than overwrite.
		getReq, err := http.NewRequest("GET", plasticityURL, nil)
		if err != nil {
			return err
		}
		addSessionCookie(getReq)
		getResp, err := client.Do(getReq)
		if err != nil {
			return err
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET plasticity: HTTP %d", getResp.StatusCode)
		}

		var data struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(getResp.Body).Decode(&data); err != nil {
			return err
		}
		var cfgMap map[string]any
		if data.Config != nil && string(data.Config) != "null" {
			if err := json.Unmarshal(data.Config, &cfgMap); err != nil {
				cfgMap = map[string]any{}
			}
		} else {
			cfgMap = map[string]any{}
		}
		cfgMap["behavior_mode"] = mode
		if customInstructions != "" {
			cfgMap["behavior_instructions"] = customInstructions
		}

		bodyBytes, err := json.Marshal(cfgMap)
		if err != nil {
			return err
		}
		putReq, err := http.NewRequest("PUT", plasticityURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return err
		}
		putReq.Header.Set("Content-Type", "application/json")
		addSessionCookie(putReq)
		putResp, err := client.Do(putReq)
		if err != nil {
			return err
		}
		defer putResp.Body.Close()
		if putResp.StatusCode != http.StatusOK {
			return fmt.Errorf("PUT plasticity: HTTP %d", putResp.StatusCode)
		}
		return nil
	}

	err := doApply()
	if err != nil {
		// One retry after a short backoff — daemon may still be warming up.
		time.Sleep(2 * time.Second)
		err = doApply()
	}

	if err != nil {
		// Non-fatal fallback: print the manual command so the user isn't left stranded.
		fmt.Printf("  ⚠  Could not apply behavior setting automatically (%v)\n", err)
		fmt.Println("  Apply it manually after the server starts:")
		fmt.Printf("    muninn vault behavior default --mode %s\n", mode)
		if customInstructions != "" {
			fmt.Printf("    muninn vault behavior default --instructions %q\n", customInstructions)
		}
		return
	}
	fmt.Printf("  ✓ Vault behavior set to: %s\n", mode)
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
// behaviorMode is passed through to configureClaudeMD so the proactivity guidance matches
// the user's stated preference.
func promptClaudeMD(behaviorMode string) {
	fmt.Println()
	fmt.Print("  Configure CLAUDE.md to prefer MuninnDB for memory? [Y/n]: ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))

	if answer == "" || answer == "y" || answer == "yes" {
		if err := configureClaudeMD(behaviorMode); err != nil {
			fmt.Fprintf(os.Stderr, "  ⚠  CLAUDE.md: %v\n", err)
		}
	} else {
		printClaudeMDInstructions()
	}
}
