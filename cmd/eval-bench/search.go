package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/scrypster/muninndb/internal/cognitive"
)

// SearchConfig controls the Bayesian search loop.
type SearchConfig struct {
	Iterations      int
	DataDir         string
	LocomoPath      string
	LongMemEvalPath string
	Dataset         string
	EngineFactory   func(*cognitive.HippocampalConfig) EngineFactory
	Judge           Judge
	Supabase        *SupabaseClient
	Verbose         bool
}

// BayesianResult is the output of RunBayesianSearch.
type BayesianResult struct {
	BestConfig cognitive.FeatureVector
	BestScore  cognitive.BenchmarkResult
	AllResults []cognitive.BenchmarkResult
	SessionID  string
}

// RunBayesianSearch runs iterative Thompson sampling over hippocampal feature configs.
func RunBayesianSearch(ctx context.Context, cfg SearchConfig) (BayesianResult, error) {
	sessionID := uuid.New().String()
	searcher := cognitive.NewBayesianSearcher(time.Now().UnixNano())
	gitCommit, gitBranch := gitInfo()

	var allResults []cognitive.BenchmarkResult
	var bestResult cognitive.BenchmarkResult
	bestResult.Score = -1

	for iter := 0; iter < cfg.Iterations; iter++ {
		select {
		case <-ctx.Done():
			return BayesianResult{}, ctx.Err()
		default:
		}

		fv := searcher.NextConfig()

		// Enforce dependency constraints.
		if !fv.EpisodesEnabled {
			fv.ReplayEnabled = false
			fv.CompletionEnabled = false
		}

		hippo := fv.ToHippocampalConfig()
		factory := cfg.EngineFactory(&hippo)

		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "\n=== Iteration %d/%d ===\n", iter+1, cfg.Iterations)
			fmt.Fprintf(os.Stderr, "Config: episodes=%v sim=%.2f replay=%v separation=%v(α=%.2f) loci=%v completion=%v\n",
				fv.EpisodesEnabled, fv.SimilarityThreshold, fv.ReplayEnabled,
				fv.SeparationEnabled, fv.SeparationAlpha, fv.LociEnabled, fv.CompletionEnabled)
		}

		// Run evaluations.
			var avgF1, avgRecall, avgAcc float64
		var locomoCount, lmeCount int

		if cfg.Dataset == "locomo" || cfg.Dataset == "all" {
			locomoResults, err := RunLocomo(ctx, cfg.LocomoPath, factory, false)
			if err != nil {
				return BayesianResult{}, fmt.Errorf("locomo iter %d: %w", iter, err)
			}
			summary := SummarizeLocomo(locomoResults)
			avgF1 = summary.OverallF1
			avgRecall = summary.OverallRecall
			locomoCount = summary.Total
		}

		if (cfg.Dataset == "longmemeval" || cfg.Dataset == "all") && cfg.Judge != nil {
			lmeResults, err := RunLongMemEval(ctx, cfg.LongMemEvalPath, factory, cfg.Judge, false)
			if err != nil {
				return BayesianResult{}, fmt.Errorf("longmemeval iter %d: %w", iter, err)
			}
			lmeSummary := SummarizeLongMemEval(lmeResults)
			avgAcc = lmeSummary.OverallAccuracy
			lmeCount = lmeSummary.TotalQuestions
		}

		// Composite score: use answer recall (more sensitive than F1 for retrieval).
		composite := avgRecall
		if lmeCount > 0 && locomoCount > 0 {
			composite = 0.5*avgRecall + 0.5*avgAcc
		} else if lmeCount > 0 {
			composite = avgAcc
		}

		result := cognitive.BenchmarkResult{
			Config:    fv,
			Precision: avgF1,
			Recall:    avgAcc,
			MRR:       avgRecall, // store LocOMo answer recall here
			Score:     composite,
		}
		searcher.RecordResult(result)
		allResults = append(allResults, result)

		if result.Score > bestResult.Score {
			bestResult = result
		}

		if cfg.Verbose {
			fmt.Fprintf(os.Stderr, "  Recall=%.3f F1=%.3f Acc=%.3f Composite=%.3f (best: %.3f)\n",
				avgRecall, avgF1, avgAcc, composite, bestResult.Score)
		}

		// Upload to Supabase.
		if cfg.Supabase != nil {
			iterN := iter + 1
			run := EvalRun{
				GitCommit:           gitCommit,
				GitBranch:           gitBranch,
				Dataset:             cfg.Dataset,
				Embedder:            "hash-384",
				EpisodesEnabled:     fv.EpisodesEnabled,
				SimilarityThreshold: &fv.SimilarityThreshold,
				ReplayEnabled:       fv.ReplayEnabled,
				SeparationEnabled:   fv.SeparationEnabled,
				SeparationAlpha:     &fv.SeparationAlpha,
				LociEnabled:         fv.LociEnabled,
				CompletionEnabled:   fv.CompletionEnabled,
				CompositeScore:      &composite,
				AvgF1:               &avgF1,
				AvgAccuracy:         &avgAcc,
				TotalQuestions:      locomoCount + lmeCount,
				SearchIteration:     &iterN,
				SearchSession:       &sessionID,
			}
			if _, err := cfg.Supabase.InsertRun(ctx, run); err != nil {
				fmt.Fprintf(os.Stderr, "warning: supabase: %v\n", err)
			}
		}
	}

	return BayesianResult{
		BestConfig: bestResult.Config,
		BestScore:  bestResult,
		AllResults: allResults,
		SessionID:  sessionID,
	}, nil
}

func gitInfo() (commit, branch string) {
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "branch", "--show-current").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	return
}
