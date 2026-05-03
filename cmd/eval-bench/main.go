package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
)

func main() {
	mode := flag.String("mode", "compare", "baseline|search|compare|interactive")
	dataset := flag.String("dataset", "all", "locomo|longmemeval|all")
	iterations := flag.Int("iterations", 20, "Bayesian search iterations")
	dataDir := flag.String("data-dir", filepath.Join(".", "testdata", "eval-bench"), "dataset directory")
	judgeURL := flag.String("judge-url", os.Getenv("EVAL_JUDGE_URL"), "local LLM judge URL (OpenAI-compat)")
	openrouterKey := flag.String("openrouter-key", os.Getenv("OPENROUTER_API_KEY"), "OpenRouter API key")
	supabaseKey := flag.String("supabase-key", os.Getenv("SUPABASE_KEY"), "Supabase anon key")
	ollamaURL := flag.String("ollama-url", os.Getenv("OLLAMA_URL"), "Ollama base URL (e.g. http://localhost:11434)")
	ollamaModel := flag.String("ollama-model", "nomic-embed-text", "Ollama embedding model name")
	localEmbed := flag.Bool("local-embed", false, "use built-in ONNX bge-small embedder (requires -tags localassets)")
	verbose := flag.Bool("verbose", false, "verbose output")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := ensureDatasets(*dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "download datasets: %v\n", err)
		os.Exit(1)
	}

	locomoPath := filepath.Join(*dataDir, "locomo10.json")
	longmemPath := filepath.Join(*dataDir, "longmemeval_s_cleaned.json")

	var judge Judge
	if *judgeURL != "" || *openrouterKey != "" {
		judge = NewJudge(*judgeURL, *openrouterKey)
	}

	var supa *SupabaseClient
	if *supabaseKey != "" {
		supa = NewSupabaseClient(*supabaseKey)
	}

	// Resolve embedder: local ONNX > Ollama > hash fallback.
	var sharedEmbedder activation.Embedder
	embedderName := "hash-384"
	if *localEmbed {
		le, err := NewLocalEmbedder(ctx, *dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "local embed init: %v\n", err)
			os.Exit(1)
		}
		defer le.Close()
		sharedEmbedder = le
		embedderName = "bge-small-en-v1.5"
		fmt.Fprintf(os.Stderr, "using embedder: %s (dim=%d)\n", embedderName, le.Dim)
	} else if *ollamaURL != "" {
		oe, err := NewOllamaEmbedder(ctx, *ollamaURL, *ollamaModel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ollama init: %v\n", err)
			os.Exit(1)
		}
		defer oe.Close()
		sharedEmbedder = oe
		embedderName = "ollama/" + *ollamaModel
		fmt.Fprintf(os.Stderr, "using embedder: %s (dim=%d)\n", embedderName, oe.Dim)
	}

	makeFactory := func(hippo *cognitive.HippocampalConfig) EngineFactory {
		return func(tmpDir string) (*evalEngine, error) {
			return NewEvalEngine(tmpDir, hippo, sharedEmbedder)
		}
	}
	switch *mode {
	case "baseline":
		hippo := cognitive.DefaultHippocampalConfig()
		factory := makeFactory(&hippo)
		results, err := runEval(ctx, locomoPath, longmemPath, *dataset, factory, judge, supa, *verbose, "baseline")
		if err != nil {
			fmt.Fprintf(os.Stderr, "baseline: %v\n", err)
			os.Exit(1)
		}
		printBaselineReport(results)

	case "search":
		result, err := RunBayesianSearch(ctx, SearchConfig{
			Iterations:      *iterations,
			DataDir:         *dataDir,
			LocomoPath:      locomoPath,
			LongMemEvalPath: longmemPath,
			Dataset:         *dataset,
			EngineFactory:   makeFactory,
			Judge:           judge,
			Supabase:        supa,
			Verbose:         *verbose,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "search: %v\n", err)
			os.Exit(1)
		}
		printSearchReport(result)

	case "compare":
		hippo := cognitive.DefaultHippocampalConfig()
		factory := makeFactory(&hippo)
		baseResults, err := runEval(ctx, locomoPath, longmemPath, *dataset, factory, judge, supa, *verbose, "baseline")
		if err != nil {
			fmt.Fprintf(os.Stderr, "baseline: %v\n", err)
			os.Exit(1)
		}

		searchResult, err := RunBayesianSearch(ctx, SearchConfig{
			Iterations:      *iterations,
			DataDir:         *dataDir,
			LocomoPath:      locomoPath,
			LongMemEvalPath: longmemPath,
			Dataset:         *dataset,
			EngineFactory:   makeFactory,
			Judge:           judge,
			Supabase:        supa,
			Verbose:         *verbose,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "search: %v\n", err)
			os.Exit(1)
		}

		printComparisonReport(baseResults, searchResult)

	case "interactive":
		results, err := RunInteractive(ctx, sharedEmbedder, *verbose)
		if err != nil {
			fmt.Fprintf(os.Stderr, "interactive: %v\n", err)
			os.Exit(1)
		}
		printInteractiveReport(results)

	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", *mode)
		os.Exit(1)
	}
}

func runEval(ctx context.Context, locomoPath, longmemPath, dataset string, factory EngineFactory, judge Judge, supa *SupabaseClient, verbose bool, label string) (*EvalResults, error) {
	results := &EvalResults{Label: label}

	if dataset == "locomo" || dataset == "all" {
		if verbose {
			fmt.Fprintf(os.Stderr, "\n[%s] Running LocOMo...\n", label)
		}
		locomoResults, err := RunLocomo(ctx, locomoPath, factory, verbose)
		if err != nil {
			return nil, fmt.Errorf("locomo: %w", err)
		}
		results.Locomo = locomoResults
		results.LocomoSummary = SummarizeLocomo(locomoResults)
	}

	if dataset == "longmemeval" || dataset == "all" {
		if judge == nil {
			fmt.Fprintf(os.Stderr, "warning: skipping LongMemEval (no judge URL or OpenRouter key)\n")
		} else {
			if verbose {
				fmt.Fprintf(os.Stderr, "\n[%s] Running LongMemEval...\n", label)
			}
			lmeResults, err := RunLongMemEval(ctx, longmemPath, factory, judge, verbose)
			if err != nil {
				return nil, fmt.Errorf("longmemeval: %w", err)
			}
			results.LongMemEval = lmeResults
			results.LongMemSummary = SummarizeLongMemEval(lmeResults)
		}
	}

	return results, nil
}
