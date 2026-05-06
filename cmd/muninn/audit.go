package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/audit"
)

func auditLogFilePath() string {
	return auditLogFilePathIn(defaultDataDir())
}

func auditLogFilePathIn(dataDir string) string {
	cfg := audit.ConfigFromEnv(dataDir)
	if cfg.FilePath == "" {
		return dataDir + "/audit.log"
	}
	return cfg.FilePath
}

func printAuditUsage() {
	fmt.Println("Usage: muninn audit <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  tail     Follow the audit log in real time (default: last 25 lines)")
	fmt.Println("  export   Dump events to stdout as JSON or NDJSON")
	fmt.Println("  stats    Show action counts from the audit log")
	fmt.Println()
	fmt.Println("Global flags:")
	fmt.Println("  --data <dir>   Data directory (default: ./muninn-data)")
}

func runAudit(args []string) {
	if len(args) == 0 {
		printAuditUsage()
		return
	}

	// Allow --data flag anywhere in args.
	dataDir := defaultDataDir()
	var remaining []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if (a == "--data" || a == "-data") && i+1 < len(args) {
			dataDir = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--data=") {
			dataDir = strings.TrimPrefix(a, "--data=")
			continue
		}
		remaining = append(remaining, a)
	}
	if len(remaining) == 0 {
		printAuditUsage()
		return
	}

	sub := remaining[0]
	subArgs := remaining[1:]
	path := auditLogFilePathIn(dataDir)

	switch sub {
	case "tail":
		runAuditTail(path, subArgs)
	case "export":
		runAuditExportCmd(path, subArgs)
	case "stats":
		runAuditStatsCmd(path, subArgs)
	default:
		fmt.Printf("Unknown audit command: %q\n", sub)
		printAuditUsage()
		osExit(1)
	}
}

// ── tail ─────────────────────────────────────────────────────────────────────

type auditFilter struct {
	Actor  string
	Action string // prefix match
}

func (f auditFilter) matches(e audit.AuditEvent) bool {
	if f.Actor != "" && e.ActorID != f.Actor {
		return false
	}
	if f.Action != "" && !strings.HasPrefix(e.Action, f.Action) {
		return false
	}
	return true
}

func runAuditTail(path string, args []string) {
	fs := flag.NewFlagSet("audit tail", flag.ExitOnError)
	last := fs.Int("last", 25, "Number of recent lines to show before following")
	actor := fs.String("actor", "", "Filter by actor_id")
	action := fs.String("action", "", "Filter by action (prefix match, e.g. vault.)")
	jsonOut := fs.Bool("json", false, "Output raw JSON lines")
	noFollow := fs.Bool("no-follow", false, "Print recent lines and exit")
	_ = fs.Parse(args)

	filter := auditFilter{Actor: *actor, Action: *action}

	if *noFollow {
		printLastNAudit(path, *last, filter, *jsonOut, os.Stdout)
		return
	}
	tailAuditLog(path, *last, filter, *jsonOut, os.Stdout, os.Stderr)
}

func printLastNAudit(path string, n int, f auditFilter, jsonOut bool, out io.Writer) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "  No audit log found. Start muninn to begin auditing.")
			return
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var e audit.AuditEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if !f.matches(e) {
			continue
		}
		lines = append(lines, line)
	}

	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	for _, l := range lines[start:] {
		if jsonOut {
			fmt.Fprintln(out, l)
		} else {
			prettyPrintAuditLine(l, out)
		}
	}
}

func tailAuditLog(path string, lastN int, f auditFilter, jsonOut bool, out, errOut io.Writer) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "  No audit log found. Start muninn to begin auditing.")
			return
		}
		fmt.Fprintf(errOut, "Error: %v\n", err)
		return
	}
	defer file.Close()

	if lastN > 0 {
		printLastNAudit(path, lastN, f, jsonOut, out)
	}

	_, _ = file.Seek(0, io.SeekEnd)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  tailing %s  (Ctrl+C to stop)\n", path)
	fmt.Fprintln(out, "  "+strings.Repeat("─", 60))

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			continue
		}
		var e audit.AuditEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if !f.matches(e) {
			continue
		}
		if jsonOut {
			fmt.Fprintln(out, line)
		} else {
			prettyPrintAuditLine(line, out)
		}
	}
}

func prettyPrintAuditLine(raw string, out io.Writer) {
	var e audit.AuditEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		fmt.Fprintln(out, raw)
		return
	}
	ts := e.Timestamp.Format("2006-01-02 15:04:05")
	result := e.Result
	if e.Error != "" {
		result += " (" + e.Error + ")"
	}
	target := e.TargetID
	if e.TargetType != "" && e.TargetID != "" {
		target = e.TargetType + ":" + e.TargetID
	}
	fmt.Fprintf(out, "%s  %-18s  %-28s  %-12s  %s\n",
		ts, e.ActorID, e.Action, result, target)
}

// ── export ────────────────────────────────────────────────────────────────────

func runAuditExportCmd(path string, args []string) {
	fs := flag.NewFlagSet("audit export", flag.ExitOnError)
	since := fs.String("since", "", "Start time (RFC3339, e.g. 2026-01-01T00:00:00Z)")
	until := fs.String("until", "", "End time (RFC3339)")
	format := fs.String("format", "json", "Output format: json or ndjson")
	output := fs.String("output", "", "Output file path (default: stdout)")
	_ = fs.Parse(args)

	var out io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
			osExit(1)
			return
		}
		defer f.Close()
		out = f
	}

	if err := runAuditExport(path, *since, *until, *format, out); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		osExit(1)
	}
}

func runAuditExport(path, since, until, format string, out io.Writer) error {
	var sinceT, untilT time.Time
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return fmt.Errorf("invalid --since: %w", err)
		}
		sinceT = t
	}
	if until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return fmt.Errorf("invalid --until: %w", err)
		}
		untilT = t
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no audit log found at %s", path)
		}
		return err
	}
	defer file.Close()

	var events []audit.AuditEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e audit.AuditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if !sinceT.IsZero() && e.Timestamp.Before(sinceT) {
			continue
		}
		if !untilT.IsZero() && e.Timestamp.After(untilT) {
			continue
		}
		events = append(events, e)
	}

	switch format {
	case "ndjson":
		enc := json.NewEncoder(out)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	default: // "json"
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(events)
	}
	return nil
}

// ── stats ─────────────────────────────────────────────────────────────────────

func runAuditStatsCmd(path string, args []string) {
	var out strings.Builder
	runAuditStats(path, &out)
	fmt.Print(out.String())
}

func runAuditStats(path string, out io.Writer) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "  No audit log found.")
			return
		}
		fmt.Fprintf(out, "Error: %v\n", err)
		return
	}
	defer file.Close()

	counts := map[string]int{}
	resultCounts := map[string]int{}
	total := 0
	var first, last time.Time

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var e audit.AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		counts[e.Action]++
		resultCounts[e.Result]++
		total++
		if first.IsZero() || e.Timestamp.Before(first) {
			first = e.Timestamp
		}
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
	}

	fmt.Fprintf(out, "Audit log: %s\n", path)
	fmt.Fprintf(out, "Total events: %d\n", total)
	if !first.IsZero() {
		fmt.Fprintf(out, "Oldest:       %s\n", first.Format(time.RFC3339))
		fmt.Fprintf(out, "Newest:       %s\n", last.Format(time.RFC3339))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Results:")
	for _, r := range []string{"ok", "error", "denied"} {
		if n := resultCounts[r]; n > 0 {
			fmt.Fprintf(out, "  %-10s %d\n", r, n)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Actions:")

	type kv struct {
		k string
		v int
	}
	var sorted []kv
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].v != sorted[j].v {
			return sorted[i].v > sorted[j].v
		}
		return sorted[i].k < sorted[j].k
	})
	for _, kv := range sorted {
		fmt.Fprintf(out, "  %-32s %d\n", kv.k, kv.v)
	}
}
