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

	path := logFilePath()

	if *noFollow {
		printLastN(path, *last, *level)
		return
	}

	tailLog(path, *level, *last, os.Stdout, os.Stderr)
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
