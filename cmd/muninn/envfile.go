package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

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
