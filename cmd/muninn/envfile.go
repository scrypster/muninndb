package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// embedderDefaultValue is the default value to pre-populate for URL-based providers.
var embedderDefaultValue = map[string]string{
	"ollama": "ollama://localhost:11434/nomic-embed-text",
}

// buildEnvFileContent returns the text for a new muninn.env file.
// embedProvider is the selected embed provider (e.g. "ollama", "openai", "local").
// enrichURL is the enrich provider URL, empty if not configured.
func buildEnvFileContent(embedProvider, enrichURL string) string {
	var b strings.Builder

	b.WriteString("# MuninnDB Configuration\n")
	b.WriteString("# Auto-loaded by 'muninn start' and 'muninn mcp'.\n")
	b.WriteString("# Shell environment variables always take precedence over values here.\n")
	b.WriteString("# Only MUNINN* variables are read from this file.\n")
	b.WriteString("#\n")
	b.WriteString("# Edit and uncomment the variables you need.\n\n")

	// ── Embedder ──────────────────────────────────────────────────────────────
	b.WriteString("# ── Embedder ──────────────────────────────────────────────\n")

	type embedEntry struct{ provider, varName, example string }
	allEmbed := []embedEntry{
		{"ollama", "MUNINN_OLLAMA_URL", "ollama://localhost:11434/nomic-embed-text"},
		{"openai", "MUNINN_OPENAI_KEY", "sk-..."},
		{"openai", "MUNINN_OPENAI_URL", ""},  // optional, always commented
		{"voyage", "MUNINN_VOYAGE_KEY", "pa-..."},
		{"cohere", "MUNINN_COHERE_KEY", "..."},
		{"google", "MUNINN_GOOGLE_KEY", "..."},
		{"jina", "MUNINN_JINA_KEY", "jina_..."},
		{"mistral", "MUNINN_MISTRAL_KEY", "..."},
		{"local", "MUNINN_LOCAL_EMBED", "0"}, // uncomment to disable
	}
	for _, e := range allEmbed {
		active := embedProvider == e.provider
		// These vars are always written commented regardless of selection.
		if e.varName == "MUNINN_OPENAI_URL" || e.varName == "MUNINN_LOCAL_EMBED" {
			active = false
		}
		val := embedderDefaultValue[e.provider]
		if val == "" {
			val = e.example
		}
		if active {
			fmt.Fprintf(&b, "%s=%s\n", e.varName, val)
		} else if val != "" {
			fmt.Fprintf(&b, "# %s=%s\n", e.varName, val)
		} else {
			fmt.Fprintf(&b, "# %s=\n", e.varName)
		}
	}
	b.WriteString("\n")

	// ── Enrichment ────────────────────────────────────────────────────────────
	b.WriteString("# ── Enrichment (optional LLM enrichment) ─────────────────\n")
	if enrichURL != "" {
		fmt.Fprintf(&b, "MUNINN_ENRICH_URL=%s\n", enrichURL)
		b.WriteString("# MUNINN_ENRICH_API_KEY=\n")
		b.WriteString("# MUNINN_ANTHROPIC_KEY=   # alias for Anthropic providers\n")
	} else {
		b.WriteString("# MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001\n")
		b.WriteString("# MUNINN_ENRICH_API_KEY=\n")
		b.WriteString("# MUNINN_ANTHROPIC_KEY=\n")
	}
	b.WriteString("\n")

	// ── Network ───────────────────────────────────────────────────────────────
	b.WriteString("# ── Network ──────────────────────────────────────────────\n")
	b.WriteString("# MUNINN_LISTEN_HOST=127.0.0.1\n")
	b.WriteString("# MUNINN_UI_ADDR=127.0.0.1:8476\n")
	b.WriteString("# MUNINN_MCP_URL=http://127.0.0.1:8750/mcp\n")
	b.WriteString("# MUNINN_CORS_ORIGINS=\n")
	b.WriteString("\n")

	// ── TLS ───────────────────────────────────────────────────────────────────
	b.WriteString("# ── TLS (optional) ───────────────────────────────────────\n")
	b.WriteString("# MUNINN_TLS_CERT=/path/to/cert.pem\n")
	b.WriteString("# MUNINN_TLS_KEY=/path/to/key.pem\n")
	b.WriteString("\n")

	// ── Backup ────────────────────────────────────────────────────────────────
	b.WriteString("# ── Backup (optional) ────────────────────────────────────\n")
	b.WriteString("# MUNINN_BACKUP_DIR=\n")
	b.WriteString("# MUNINN_BACKUP_INTERVAL=6h\n")
	b.WriteString("# MUNINN_BACKUP_RETAIN=5\n")
	b.WriteString("\n")

	// ── Memory / Performance ─────────────────────────────────────────────────
	b.WriteString("# ── Memory / Performance ─────────────────────────────────\n")
	b.WriteString("# MUNINN_MEM_LIMIT_GB=4\n")
	b.WriteString("# MUNINN_GC_PERCENT=200\n")
	b.WriteString("# MUNINN_RATE_LIMIT_GLOBAL_RPS=1000\n")
	b.WriteString("# MUNINN_RATE_LIMIT_PER_IP_RPS=100\n")

	return b.String()
}

// writeEnvFile writes ~/.muninn/muninn.env. If the file already exists it is
// left untouched (user may have customized it). Uses atomic write (temp+rename)
// with 0600 permissions, matching the pattern used for mcp.token.
// Returns (true, nil) if created, (false, nil) if already existed.
func writeEnvFile(embedProvider, enrichURL string) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	return writeEnvFileTo(filepath.Join(home, envFileName), embedProvider, enrichURL)
}

// writeEnvFileTo is the testable inner implementation.
// Returns (true, nil) if the file was created, (false, nil) if it already existed.
func writeEnvFileTo(path, embedProvider, enrichURL string) (bool, error) {
	// Do not overwrite an existing file — user may have customized it.
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}

	content := buildEnvFileContent(embedProvider, enrichURL)

	// Atomic write: temp file → chmod 0600 → rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".muninn_env_*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // clean up on failure

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

const envFileName = ".muninn/muninn.env"
const envFileMaxBytes = 64 * 1024 // 64 KB guard

// loadEnvFile loads ~/.muninn/muninn.env into the process environment.
// It is called at the top of runServer() and runMCPStdio() so the daemon
// process picks up config before reading any MUNINN_* vars.
// Shell environment variables always take precedence over file values.
func loadEnvFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	loadEnvFileFrom(filepath.Join(home, envFileName))
}

// loadEnvFileFrom is the testable inner implementation.
func loadEnvFileFrom(path string) {
	// Lstat first to reject symlinks before opening.
	info, err := os.Lstat(path)
	if err != nil {
		return // missing or unreadable — silent no-op
	}
	if info.Mode()&os.ModeSymlink != 0 {
		slog.Warn("muninn.env is a symlink, skipping", "path", path)
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if info.Size() > envFileMaxBytes {
		slog.Warn("muninn.env exceeds size limit, skipping",
			"path", path, "size", info.Size(), "limit", envFileMaxBytes)
		return
	}
	// Warn if group- or world-readable (may contain API keys).
	if info.Mode().Perm()&0o044 != 0 {
		fmt.Fprintf(os.Stderr, "  warning: %s is group/world-readable — run: chmod 600 %s\n", path, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	loaded := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Strip optional "export " prefix for shell compatibility.
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			slog.Warn("muninn.env: malformed line (no '='), skipping",
				"path", path, "line", lineNum)
			continue
		}

		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			slog.Warn("muninn.env: invalid key, skipping",
				"path", path, "line", lineNum)
			continue
		}

		// Restrict to MUNINN* keys — prevents hijacking PATH, LD_PRELOAD, etc.
		if !strings.HasPrefix(key, "MUNINN") {
			slog.Debug("muninn.env: non-MUNINN key ignored",
				"path", path, "line", lineNum, "key", key)
			continue
		}

		value = strings.TrimSpace(value)
		// Strip matching surrounding quotes.
		if len(value) >= 2 {
			if q := value[0]; (q == '"' || q == '\'') && value[len(value)-1] == q {
				value = value[1 : len(value)-1]
			}
		}

		// Shell env wins — only set if not already present.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if setErr := os.Setenv(key, value); setErr != nil {
			slog.Warn("muninn.env: failed to set env var",
				"key", key, "error", setErr)
			continue
		}
		loaded++
	}

	if loaded > 0 {
		slog.Info("loaded config from muninn.env", "path", path, "vars", loaded)
	}
}
