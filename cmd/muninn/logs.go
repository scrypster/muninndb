package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultTailHistory = 25

func logFilePath() string {
	return filepath.Join(defaultDataDir(), "muninn.log")
}

func runLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	last := fs.Int("last", defaultTailHistory, "Number of recent lines to show")
	level := fs.String("level", "", "Filter by log level: debug, info, warn, error")
	noFollow := fs.Bool("no-follow", false, "Print recent lines and exit (don't tail)")
	fs.Usage = func() { subcommandHelp["logs"]() }
	fs.Parse(args)

	// Support positional arg: muninn logs 50
	if fs.NArg() > 0 {
		if n, err := strconv.Atoi(fs.Arg(0)); err == nil && n > 0 {
			*last = n
		}
	}

	// #852: <dataDir>/muninn.log is only ever written by the CLI's own
	// fork-a-daemon path. A daemon started any other way — a systemd unit
	// execing --daemon directly, launchd with StandardErrorPath, `docker
	// run` — never writes it, and reading it unconditionally shows fossil
	// content with no signal anything is wrong. resolveLogPath defers to
	// the muninn.logdest sidecar the daemon itself records at every
	// startup (#850), which is authoritative regardless of how the daemon
	// was started.
	path, inherited := resolveLogPath(defaultDataDir())
	if inherited {
		printInheritedLogGuidance(defaultDataDir())
		return
	}

	if *noFollow {
		printLastN(path, *last, *level)
		return
	}

	tailLog(path, *level, *last, os.Stdout, os.Stderr)
}

// printInheritedLogGuidance is what `muninn logs` prints instead of tailing
// a file, when the running (or most recently started) daemon's log
// destination is inherited stderr with no path this command can resolve.
// Showing nothing, or showing an explanation, both beat showing stale or
// wrong content as if it were current (docs/internals/claim-discipline.md).
func printInheritedLogGuidance(dataDir string) {
	fmt.Println("  This daemon's log destination is inherited stderr, not a file muninn")
	fmt.Println("  itself opened — `muninn logs` has no path it can safely read.")
	fmt.Println()
	if unit, managed := serviceManagerOwnsDaemon(); managed {
		fmt.Printf("  Managed by systemd (unit: %s). View its logs with:\n", unit)
		fmt.Printf("    journalctl -u %s -n 100 --no-pager\n", unit)
	} else {
		fmt.Println("  Check your process supervisor for where it captures stdout/stderr:")
		fmt.Println("    macOS (launchd):  StandardErrorPath / StandardOutPath in the .plist")
		fmt.Println("    Linux (systemd):  journalctl -u <unit> -n 100 --no-pager")
		fmt.Println("    Docker:           docker logs <container>")
	}
	fmt.Println()
	fmt.Println("  To make `muninn logs` work again, point the daemon at an explicit file:")
	fmt.Printf("    --log-file %s\n", filepath.Join(dataDir, "muninn.log"))
	fmt.Println("  or set MUNINN_LOG_FILE to the same path your supervisor captures.")
}

// printLastN reads the last N lines from the log file (filtered by level if set).
func printLastN(path string, n int, levelFilter string) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("  No log file found at", path)
			fmt.Println("  Start muninn to begin logging: muninn start")
			return
		}
		fmt.Fprintf(os.Stderr, "Error opening log: %v\n", err)
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if levelFilter == "" || matchesLevel(line, levelFilter) {
			lines = append(lines, line)
		}
	}

	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	for _, l := range lines[start:] {
		fmt.Println(l)
	}
}

// tailLog shows the last N lines of history then continuously tails until Ctrl+C.
// out and errOut are passed in by the caller (never read from os.Stdout/os.Stderr
// directly) so that concurrent tests that redirect those globals don't race.
func tailLog(path string, levelFilter string, lastN int, out, errOut io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "  No log file found at", path)
			fmt.Fprintln(out, "  Start muninn to begin logging: muninn start")
			return
		}
		fmt.Fprintf(errOut, "Error opening log: %v\n", err)
		return
	}
	defer f.Close()

	// Show recent history before tailing
	if lastN > 0 {
		var lines []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if levelFilter == "" || matchesLevel(line, levelFilter) {
				lines = append(lines, line)
			}
		}
		start := 0
		if len(lines) > lastN {
			start = len(lines) - lastN
		}
		for _, l := range lines[start:] {
			fmt.Fprintln(out, l)
		}
	}

	// Seek to current end so we only tail new content
	f.Seek(0, io.SeekEnd)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  tailing %s  (Ctrl+C to stop)\n", path)
	if levelFilter != "" {
		fmt.Fprintf(out, "  filter: %s\n", levelFilter)
	}
	fmt.Fprintln(out, "  "+strings.Repeat("─", 60))

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		line = strings.TrimRight(line, "\n")
		if levelFilter == "" || matchesLevel(line, levelFilter) {
			fmt.Fprintln(out, line)
		}
	}
}

func matchesLevel(line, level string) bool {
	return strings.Contains(strings.ToUpper(line), strings.ToUpper(level))
}
