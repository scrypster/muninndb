package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// parseExplicitFlag scans osArgs for an explicit --name value or --name=value
// occurrence and returns the value. Returns "" if the flag is not present.
// Used to detect user-supplied values that must be forwarded to the daemon.
func parseExplicitFlag(name string, osArgs []string) string {
	long := "--" + name
	short := "-" + name
	for i, arg := range osArgs {
		if (arg == long || arg == short) && i+1 < len(osArgs) {
			return osArgs[i+1]
		}
		if after, ok := strings.CutPrefix(arg, long+"="); ok {
			return after
		}
		if after, ok := strings.CutPrefix(arg, short+"="); ok {
			return after
		}
	}
	return ""
}

// buildDaemonArgs constructs the argument list for the forked daemon process.
// It forwards --listen-host when non-default, --cors-origins when non-empty,
// and any explicitly provided per-service address flags (--rest-addr, --mbp-addr,
// --grpc-addr, --mcp-addr, --ui-addr) so they take effect in the daemon.
func buildDaemonArgs(dataDir string, dev bool, mcpToken string, osArgs []string, listenHostEnv, corsOriginsEnv string) []string {
	args := []string{"--daemon", "--data", dataDir}
	if dev {
		args = append(args, "--dev")
	}
	if mcpToken != "" {
		args = append(args, "--mcp-token", mcpToken)
	}
	// --listen-host: forward when non-default
	listenHost := parseListenHost(osArgs, listenHostEnv)
	if listenHost != "127.0.0.1" {
		args = append(args, "--listen-host", listenHost)
	}
	// --cors-origins: forward from flag or env (flag wins)
	corsOrigins := corsOriginsEnv
	if v := parseExplicitFlag("cors-origins", osArgs); v != "" {
		corsOrigins = v
	}
	if corsOrigins != "" {
		args = append(args, "--cors-origins", corsOrigins)
	}
	// Per-service address overrides: forward any that the user explicitly set.
	// These take priority over --listen-host defaults inside the daemon.
	for _, name := range []string{"rest-addr", "mbp-addr", "grpc-addr", "mcp-addr", "ui-addr", "metrics-addr"} {
		if v := parseExplicitFlag(name, osArgs); v != "" {
			args = append(args, "--"+name, v)
		}
	}
	return args
}

// runStart forks muninn as a background daemon and waits for health check.
func runStart(webEnabled bool) {
	dataDir := defaultDataDir()
	pidPath := filepath.Join(dataDir, "muninn.pid")

	// First-run hint: if data dir doesn't exist, suggest init
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		fmt.Println("Tip: First time? Run 'muninn init' for guided setup and AI tool configuration.")
		fmt.Println()
	}

	// Check already running
	if pid, err := readPID(pidPath); err == nil {
		if isProcessRunning(pid) {
			fmt.Printf("muninn already running (pid %d)\n", pid)
			return
		}
		os.Remove(pidPath)
	}

	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create data dir: %v\n", err)
		os.Exit(1)
	}

	// Determine dev mode from os.Args
	dev := false
	for _, arg := range os.Args {
		if arg == "--dev" {
			dev = true
			break
		}
	}

	args := buildDaemonArgs(dataDir, dev, readTokenFile(), os.Args[1:], os.Getenv("MUNINN_LISTEN_HOST"), os.Getenv("MUNINN_CORS_ORIGINS"))
	if !webEnabled {
		args = append(args, "--no-web")
	}

	cmd := exec.Command(os.Args[0], args...)
	cmd.SysProcAttr = daemonSysProcAttr()
	daemonExtraSetup(cmd)
	cmd.Stdout = nil
	logPath := logFilePath()
	lf, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if logErr == nil {
		cmd.Stderr = lf
	} else {
		cmd.Stderr = nil
	}
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		if lf != nil {
			lf.Close()
		}
		fmt.Fprintf(os.Stderr, "failed to start: %v\n", err)
		os.Exit(1)
	}

	// Close parent's copy — child has inherited the fd
	if lf != nil {
		lf.Close()
	}

	// Write PID file immediately so stop works even if health check is slow
	if err := writePID(pidPath, cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write PID file: %v\n", err)
	}

	// Wait for health check (up to 5s)
	mcpHealthURL := "http://127.0.0.1:" + defaultMCPPort + "/mcp/health"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		resp, err := http.Get(mcpHealthURL)
		if err != nil {
			continue
		}
		// Always drain and close body to allow connection reuse and prevent leaks.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("muninn started (pid %d)\n", cmd.Process.Pid)
			fmt.Println()
			printStatusDisplay(true)
			fmt.Println("  Web UI → http://127.0.0.1:8476")
			fmt.Println()
			return
		}
	}
	fmt.Fprintln(os.Stderr, "muninn started but health check timed out")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  Last log entries:")
	printLastN(logFilePath(), 20, "")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  For more detail: muninn logs")
}

// runStop signals the running daemon to shut down.
func runStop() {
	pidPath := filepath.Join(defaultDataDir(), "muninn.pid")
	pid, err := readPID(pidPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "process not found: %v\n", err)
		os.Exit(1)
	}
	if err := stopProcess(proc); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop: %v\n", err)
		os.Exit(1)
	}

	// Wait for the process to actually exit before returning. The timeout is
	// 35s — 5s beyond the daemon's own 30s internal shutdown deadline — so the
	// CLI always outlasts the daemon's worst-case graceful shutdown, including
	// Pebble WAL flush and fsync on slow disks.
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessRunning(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// If the process is still alive after 35s, do not silently proceed.
	// Starting a new instance while the old one holds the Pebble lock will
	// fail with "resource temporarily unavailable". Bail out explicitly so
	// the user knows to investigate rather than seeing a confusing start failure.
	if isProcessRunning(pid) {
		fmt.Fprintf(os.Stderr, "muninn (pid %d) did not stop within 35s — aborting\n", pid)
		fmt.Fprintf(os.Stderr, "Check 'muninn logs' for details. You can force-kill with: kill -9 %d\n", pid)
		os.Exit(1)
	}

	// 300ms buffer after process exit before returning. kill(pid,0) returning
	// ESRCH means the process is gone from the kernel table, but flock(2) lock
	// release is not guaranteed to be visible to other processes at exactly
	// that instant. This covers the brief kernel cleanup window.
	time.Sleep(300 * time.Millisecond)

	fmt.Printf("muninn stopped (pid %d)\n", pid)
	os.Remove(pidPath)
}

// runStatus prints service health and exits. Uses shared printStatusDisplay.
func runStatus() {
	state := printStatusDisplay(false)
	if state == stateStopped {
		osExit(1)
	}
}

func runStartService(service string) {
	switch service {
	case "web":
		fmt.Println("Web UI is not yet implemented (planned for Epic 16)")
	default:
		fmt.Fprintf(os.Stderr, "unknown service: %s\n", service)
		osExit(1)
	}
}

func runStopService(service string) {
	switch service {
	case "web":
		fmt.Println("Web UI is not yet implemented (planned for Epic 16)")
	default:
		fmt.Fprintf(os.Stderr, "unknown service: %s\n", service)
		osExit(1)
	}
}

func defaultDataDir() string {
	if d := os.Getenv("MUNINNDB_DATA"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muninn", "data")
}
