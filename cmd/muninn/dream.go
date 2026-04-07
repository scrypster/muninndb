package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	muninn "github.com/scrypster/muninndb"
)

func runDream(args []string) {
	fs := flag.NewFlagSet("dream", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	force := fs.Bool("force", false, "bypass trigger gates")
	dryRun := fs.Bool("dry-run", false, "preview changes without writing")
	scope := fs.String("scope", "", "limit to a single vault")
	dataDir := fs.String("data-dir", defaultDataDir(), "data directory")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	// Check server is not running (Pebble exclusive lock).
	pidPath := filepath.Join(*dataDir, "muninn.pid")
	if pid, err := readPID(pidPath); err == nil && isProcessRunning(pid) {
		fmt.Fprintf(os.Stderr, "error: muninn is running (pid %d) — cannot run offline dream\n", pid)
		fmt.Fprintln(os.Stderr, "Stop the server first: muninn stop")
		osExit(1)
		return
	}

	// Suppress slog output — dream writes structured output to stdout.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := muninn.Open(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if isExecLockError(err) {
			fmt.Fprintf(os.Stderr, "Hint: if the daemon crashed, run 'muninn status' or remove %s/pebble/LOCK\n", *dataDir)
		}
		osExit(1)
		return
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: close: %v\n", err)
		}
	}()

	// Build LLM providers from env vars.
	ollamaP, anthropicP, openaiP := buildDreamProviders(ctx)

	report, err := db.Dream(ctx, muninn.DreamOpts{
		DryRun:            *dryRun,
		Force:             *force,
		Scope:             *scope,
		OllamaProvider:    ollamaP,
		AnthropicProvider: anthropicP,
		OpenAIProvider:    openaiP,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		osExit(1)
		return
	}

	printDreamReport(report, *dryRun)
}

func printDreamReport(report *muninn.DreamReport, dryRun bool) {
	if dryRun {
		fmt.Println("[DRY RUN] No changes were written.")
		fmt.Println()
	}

	fmt.Printf("Dream completed in %s\n", report.TotalDuration.Round(100*time.Millisecond))
	fmt.Println()

	for _, r := range report.Reports {
		if r.Orient != nil && r.Orient.IsLegal {
			fmt.Printf("  %-16s  %d engrams (protected, skipped)\n", r.Vault, r.LegalSkipped)
			continue
		}

		scanned := 0
		if r.Orient != nil {
			scanned = r.Orient.EngramCount
		}

		fmt.Printf("  %-16s  scanned %d engrams", r.Vault, scanned)
		var changes []string
		if r.MergedEngrams > 0 {
			changes = append(changes, fmt.Sprintf("merged %d", r.MergedEngrams))
		}
		if r.LLMMerged > 0 {
			changes = append(changes, fmt.Sprintf("LLM merged %d", r.LLMMerged))
		}
		if r.LLMContradictions > 0 {
			changes = append(changes, fmt.Sprintf("resolved %d contradictions", r.LLMContradictions))
		}
		if r.StabilityStrengthened > 0 {
			changes = append(changes, fmt.Sprintf("strengthened %d", r.StabilityStrengthened))
		}
		if r.StabilityWeakened > 0 {
			changes = append(changes, fmt.Sprintf("weakened %d", r.StabilityWeakened))
		}
		if r.InferredEdges > 0 {
			changes = append(changes, fmt.Sprintf("inferred %d edges", r.InferredEdges))
		}
		if len(changes) > 0 {
			fmt.Printf("  (%s)", strings.Join(changes, ", "))
		}
		fmt.Println()
	}

	if len(report.Skipped) > 0 {
		fmt.Printf("\nSkipped: %s\n", strings.Join(report.Skipped, ", "))
	}

	if report.JournalEntry != "" {
		fmt.Println()
		fmt.Println(report.JournalEntry)
	}
}

