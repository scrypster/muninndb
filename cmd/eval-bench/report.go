package main

import (
	"fmt"
	"sort"
)

// EvalResults holds results from one evaluation run.
type EvalResults struct {
	Label          string
	Locomo         []LocomoResult
	LocomoSummary  LocomoSummary
	LongMemEval    []LongMemEvalResult
	LongMemSummary LongMemEvalSummary
}

var categoryNames = map[int]string{
	1: "multi-hop",
	2: "temporal",
	3: "open-domain",
	4: "single-hop",
	5: "adversarial",
}

func printBaselineReport(r *EvalResults) {
	fmt.Println("MuninnDB Eval-Bench Report")
	fmt.Println("══════════════════════════════════════════════════")
	fmt.Printf("Config: %s\n\n", r.Label)

	if r.LocomoSummary.Total > 0 {
		printLocomoTable(r.LocomoSummary)
	}
	if r.LongMemSummary.TotalQuestions > 0 {
		fmt.Println()
		printLongMemTable(r.LongMemSummary)
	}
	fmt.Println("══════════════════════════════════════════════════")
}

func printLocomoTable(s LocomoSummary) {
	fmt.Printf("LocOMo (%d questions)\n", s.Total)
	fmt.Println("Category              Count    Avg F1    Recall")
	fmt.Println("──────────────────────────────────────────────────")

	cats := sortedCats(s.ByCategory)
	for _, cat := range cats {
		m := s.ByCategory[cat]
		fmt.Printf("%-22s %4d    %.3f     %.3f\n", fmt.Sprintf("%d (%s)", cat, categoryNames[cat]), m.Count, m.AvgF1, m.AvgRecall)
	}
	fmt.Println("──────────────────────────────────────────────────")
	fmt.Printf("%-22s %4d    %.3f     %.3f\n", "Overall", s.Total, s.OverallF1, s.OverallRecall)
}

func printLongMemTable(s LongMemEvalSummary) {
	fmt.Printf("LongMemEval (%d questions)\n", s.TotalQuestions)
	fmt.Println("Ability                       Count    Accuracy")
	fmt.Println("─────────────────────────────────────────────────")

	abilities := sortedAbilities(s.ByAbility)
	for _, ability := range abilities {
		acc := s.ByAbility[ability]
		fmt.Printf("%-30s        %.3f\n", ability, acc)
	}
	fmt.Println("─────────────────────────────────────────────────")
	fmt.Printf("%-30s        %.3f\n", "Overall", s.OverallAccuracy)
	fmt.Printf("Avg latency: %.1fms\n", s.AvgLatencyMS)
}

func printSearchReport(r BayesianResult) {
	fmt.Println("\nBayesian Search Results")
	fmt.Println("══════════════════════════════════════════════════")
	fmt.Printf("Session: %s\n", r.SessionID)
	fmt.Printf("Iterations: %d\n", len(r.AllResults))
	fmt.Printf("Best composite score: %.4f\n\n", r.BestScore.Score)

	fv := r.BestConfig
	fmt.Println("Best Configuration:")
	fmt.Printf("  Episodes:     %v (threshold=%.2f)\n", fv.EpisodesEnabled, fv.SimilarityThreshold)
	fmt.Printf("  Replay:       %v (interval=%s)\n", fv.ReplayEnabled, fv.ReplayInterval)
	fmt.Printf("  Separation:   %v (alpha=%.2f)\n", fv.SeparationEnabled, fv.SeparationAlpha)
	fmt.Printf("  Loci:         %v\n", fv.LociEnabled)
	fmt.Printf("  Completion:   %v\n", fv.CompletionEnabled)
	fmt.Println("══════════════════════════════════════════════════")
}

func printComparisonReport(baseline *EvalResults, search BayesianResult) {
	fmt.Println("\nBefore / After Comparison")
	fmt.Println("══════════════════════════════════════════════════")

	if baseline.LocomoSummary.Total > 0 {
		baseRecall := baseline.LocomoSummary.OverallRecall
		bestRecall := search.BestScore.MRR // we store bestRecall in MRR field
		delta := bestRecall - baseRecall
		sign := "+"
		if delta < 0 {
			sign = ""
		}
		fmt.Printf("LocOMo Recall: %.3f → %.3f (%s%.3f)\n", baseRecall, bestRecall, sign, delta)
		fmt.Printf("LocOMo F1:     %.3f → %.3f\n", baseline.LocomoSummary.OverallF1, search.BestScore.Precision)
	}

	if baseline.LongMemSummary.TotalQuestions > 0 {
		baseAcc := baseline.LongMemSummary.OverallAccuracy
		bestAcc := search.BestScore.Recall // we stored avgAcc in Recall
		delta := bestAcc - baseAcc
		sign := "+"
		if delta < 0 {
			sign = ""
		}
		fmt.Printf("LongMemEval:   %.3f → %.3f (%s%.3f)\n", baseAcc, bestAcc, sign, delta)
	}

	fmt.Printf("Composite:     %.4f (best of %d iterations)\n", search.BestScore.Score, len(search.AllResults))

	fmt.Println("\nBest hippocampal config:")
	fv := search.BestConfig
	fmt.Printf("  episodes=%v sep=%v(α=%.2f) replay=%v loci=%v completion=%v\n",
		fv.EpisodesEnabled, fv.SeparationEnabled, fv.SeparationAlpha,
		fv.ReplayEnabled, fv.LociEnabled, fv.CompletionEnabled)
	fmt.Println("══════════════════════════════════════════════════")
}

func sortedCats(m map[int]CategoryMetrics) []int {
	cats := make([]int, 0, len(m))
	for c := range m {
		cats = append(cats, c)
	}
	sort.Ints(cats)
	return cats
}

func sortedAbilities(m map[string]float64) []string {
	abilities := make([]string, 0, len(m))
	for a := range m {
		abilities = append(abilities, a)
	}
	sort.Strings(abilities)
	return abilities
}
