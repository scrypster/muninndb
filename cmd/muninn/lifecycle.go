package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

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

	args := []string{"--daemon", "--data", dataDir}
	if !webEnabled {
		args = append(args, "--no-web")
	}
	// Pass through --dev flag if present
	for _, arg := range os.Args {
		if arg == "--dev" {
			args = append(args, "--dev")
			break
		}
	}
	// Pass MCP token from token file if present
	if tok := readTokenFile(); tok != "" {
		args = append(args, "--mcp-token", tok)
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
	mcpHealthURL := "http://" + defaultMCPAddr + "/mcp/health"
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
			fmt.Println("  Web UI → http://localhost:8476")
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

	// Wait for the process to actually exit (up to 5s) so that restart
	// doesn't race with the old process still holding the ports.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessRunning(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

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
