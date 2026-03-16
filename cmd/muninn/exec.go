package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	muninn "github.com/scrypster/muninndb"
)

// Exit codes for muninn exec.
const (
	execExitSuccess  = 0
	execExitUsage    = 1
	execExitError    = 2
	execExitNotFound = 3
)

func runExec(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printExecHelp()
		return
	}

	// Find the operation name: first non-flag argument.
	// This lets --data-dir / --vault appear before or after the operation.
	op, flagArgs := extractOperation(args)
	if op == "" {
		fmt.Fprintln(os.Stderr, "muninn exec: no operation specified")
		fmt.Fprintln(os.Stderr, "Available operations: remember, recall, read, forget")
		fmt.Fprintln(os.Stderr, "Run 'muninn exec --help' for usage.")
		os.Exit(execExitUsage)
	}

	// Single FlagSet for common + operation-specific flags.
	fs := flag.NewFlagSet("exec "+op, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory")
	vault := fs.String("vault", "default", "vault name")

	var concept, content, query, id string
	var limit int

	switch op {
	case "remember":
		fs.StringVar(&concept, "concept", "", "short label for the memory (required)")
		fs.StringVar(&content, "content", "", "full text of the memory (required)")
	case "recall":
		fs.StringVar(&query, "query", "", "search query (required)")
		fs.IntVar(&limit, "limit", 10, "maximum number of results")
	case "read":
		fs.StringVar(&id, "id", "", "engram ID to retrieve (required)")
	case "forget":
		fs.StringVar(&id, "id", "", "engram ID to delete (required)")
	default:
		fmt.Fprintf(os.Stderr, "muninn exec: unknown operation %q\n\n", op)
		fmt.Fprintln(os.Stderr, "Available operations: remember, recall, read, forget")
		fmt.Fprintln(os.Stderr, "Run 'muninn exec --help' for usage.")
		os.Exit(execExitUsage)
	}

	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(execExitUsage)
	}

	// Validate required flags per operation.
	switch op {
	case "remember":
		if concept == "" || content == "" {
			fmt.Fprintln(os.Stderr, "muninn exec remember: --concept and --content are required")
			os.Exit(execExitUsage)
		}
	case "recall":
		if query == "" {
			fmt.Fprintln(os.Stderr, "muninn exec recall: --query is required")
			os.Exit(execExitUsage)
		}
	case "read", "forget":
		if id == "" {
			fmt.Fprintf(os.Stderr, "muninn exec %s: --id is required\n", op)
			os.Exit(execExitUsage)
		}
	}

	// Suppress slog output — exec writes only JSON to stdout and errors to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Open the database. Uses signal-aware context so Close() always runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := muninn.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if isExecLockError(err) {
			fmt.Fprintf(os.Stderr, "Hint: if the daemon crashed, run 'muninn status' or remove %s/pebble/LOCK\n", *dataDir)
		}
		os.Exit(execExitError)
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: close: %v\n", err)
		}
	}()

	switch op {
	case "remember":
		execRemember(ctx, db, *vault, concept, content)
	case "recall":
		execRecall(ctx, db, *vault, query, limit)
	case "read":
		execRead(ctx, db, *vault, id)
	case "forget":
		execForget(ctx, db, *vault, id)
	}
}

func execRemember(ctx context.Context, db *muninn.DB, vault, concept, content string) {
	id, err := db.Remember(ctx, vault, concept, content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(execExitError)
	}
	writeJSON(map[string]string{"id": id, "concept": concept})
}

func execRecall(ctx context.Context, db *muninn.DB, vault, query string, limit int) {
	results, err := db.Recall(ctx, vault, query, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(execExitError)
	}
	writeJSON(results)
}

func execRead(ctx context.Context, db *muninn.DB, vault, id string) {
	e, err := db.Read(ctx, vault, id)
	if err != nil {
		if errors.Is(err, muninn.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "Error: engram %q not found\n", id)
			os.Exit(execExitNotFound)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(execExitError)
	}
	writeJSON(e)
}

func execForget(ctx context.Context, db *muninn.DB, vault, id string) {
	if err := db.Forget(ctx, vault, id); err != nil {
		if errors.Is(err, muninn.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "Error: engram %q not found\n", id)
			os.Exit(execExitNotFound)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(execExitError)
	}
	writeJSON(map[string]bool{"ok": true})
}

// writeJSON marshals v as JSON to stdout and exits on error.
func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "Error: encode output: %v\n", err)
		os.Exit(execExitError)
	}
}

// extractOperation scans args for a known operation name (remember, recall,
// read, forget) and returns it along with the remaining arguments (op removed).
// Scanning for known tokens rather than using a flag-skipping heuristic means
// flag values that start with "-" (e.g. negative numbers) and unrecognized
// flags never interfere with operation detection.
func extractOperation(args []string) (op string, rest []string) {
	for i, arg := range args {
		switch arg {
		case "remember", "recall", "read", "forget":
			rest = append(args[:i:i], args[i+1:]...)
			return arg, rest
		}
	}
	return "", args
}

// isExecLockError reports whether err is a Pebble lock error, to decide
// whether to print the daemon hint.
func isExecLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "held by another process") ||
		strings.Contains(msg, "lock") || strings.Contains(msg, "LOCK") ||
		strings.Contains(msg, "already in use") ||
		strings.Contains(msg, "resource temporarily unavailable") // Linux EAGAIN
}

func printExecHelp() {
	printSubcommandUsage("exec", "one-shot operations without a running daemon",
		"muninn exec <operation> [flags]",
		[][2]string{
			{"--data-dir <dir>", "Data directory (default: ~/.muninn/data or $MUNINNDB_DATA)"},
			{"--vault <name>", "Vault name (default: default)"},
		},
		[]string{
			`muninn exec remember --concept "standup" --content "Fixed the auth bug"`,
			`muninn exec recall --query "auth bug" --limit 5`,
			`muninn exec read --id 01ARZ3NDEKTSV4RRFFQ69G5FAV`,
			`muninn exec forget --id 01ARZ3NDEKTSV4RRFFQ69G5FAV`,
		},
	)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Operations:")
	fmt.Fprintln(os.Stderr, "  remember  --concept <label> --content <text>   store a new memory")
	fmt.Fprintln(os.Stderr, "  recall    --query <text> [--limit N]            search memories (FTS)")
	fmt.Fprintln(os.Stderr, "  read      --id <ulid>                           retrieve by ID")
	fmt.Fprintln(os.Stderr, "  forget    --id <ulid>                           permanently delete")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Exit codes: 0=success  1=usage error  2=runtime error  3=not found")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Note: exec requires exclusive access to the data directory.")
	fmt.Fprintln(os.Stderr, "      It will fail if the daemon is running on the same --data-dir.")
}
