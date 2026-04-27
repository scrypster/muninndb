package cognitive

import (
	"testing"
	"time"
)

func TestBayesianSearcherNextConfig(t *testing.T) {
	bs := NewBayesianSearcher(42)

	for i := 0; i < 20; i++ {
		cfg := bs.NextConfig()

		if cfg.SimilarityThreshold < 0.1 || cfg.SimilarityThreshold > 0.5 {
			t.Errorf("round %d: SimilarityThreshold %f out of [0.1, 0.5]", i, cfg.SimilarityThreshold)
		}
		if cfg.SeparationAlpha < 0.0 || cfg.SeparationAlpha > 0.9 {
			t.Errorf("round %d: SeparationAlpha %f out of [0.0, 0.9]", i, cfg.SeparationAlpha)
		}
		if cfg.ReplayInterval < 1*time.Hour || cfg.ReplayInterval > 24*time.Hour {
			t.Errorf("round %d: ReplayInterval %v out of [1h, 24h]", i, cfg.ReplayInterval)
		}
	}
}

func TestBayesianSearcherConverges(t *testing.T) {
	bs := NewBayesianSearcher(99)

	// The "optimal" config: episodes on, separation on, alpha=0.45, threshold=0.3.
	// Feed 50 rounds where configs matching the target score higher.
	for i := 0; i < 50; i++ {
		cfg := bs.NextConfig()
		score := scoreForConvergence(cfg)
		bs.RecordResult(BenchmarkResult{
			Config:    cfg,
			Precision: score,
			Recall:    score,
			MRR:       score,
			Latency:   10 * time.Millisecond,
			Score:     score,
		})
	}

	bestCfg, bestResult := bs.BestConfig()
	if bestResult.Score < 0.5 {
		t.Errorf("expected best score >= 0.5, got %f", bestResult.Score)
	}

	// After 50 rounds the searcher should have found a config with episodes enabled
	// and separation enabled, since those contribute most to the score.
	_ = bestCfg // We verify via score; exact flags depend on stochastic exploration.

	// Generate 20 more configs and check the searcher is biased toward episodes=true.
	episodeCount := 0
	for i := 0; i < 20; i++ {
		cfg := bs.NextConfig()
		if cfg.EpisodesEnabled {
			episodeCount++
		}
	}
	if episodeCount < 10 {
		t.Errorf("expected episodes enabled in >50%% of late samples, got %d/20", episodeCount)
	}
}

// scoreForConvergence returns a synthetic score that rewards the "optimal" config.
func scoreForConvergence(fv FeatureVector) float64 {
	score := 0.0
	if fv.EpisodesEnabled {
		score += 0.3
	}
	if fv.SeparationEnabled {
		score += 0.3
	}
	// Reward threshold near 0.3.
	score += 0.2 * (1.0 - abs(fv.SimilarityThreshold-0.3)/0.4)
	// Reward alpha near 0.45.
	score += 0.2 * (1.0 - abs(fv.SeparationAlpha-0.45)/0.9)
	if score < 0 {
		score = 0
	}
	return score
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestToHippocampalConfig(t *testing.T) {
	fv := FeatureVector{
		EpisodesEnabled:     true,
		SimilarityThreshold: 0.3,
		ReplayEnabled:       true,
		ReplayInterval:      6 * time.Hour,
		SeparationEnabled:   true,
		SeparationAlpha:     0.45,
		LociEnabled:         true,
		CompletionEnabled:   true,
	}

	hc := fv.ToHippocampalConfig()

	if !hc.EnableEpisodes {
		t.Error("expected EnableEpisodes=true")
	}
	if hc.EpisodeConfig.SimilarityThreshold != 0.3 {
		t.Errorf("expected SimilarityThreshold=0.3, got %f", hc.EpisodeConfig.SimilarityThreshold)
	}
	if !hc.EnableReplay {
		t.Error("expected EnableReplay=true")
	}
	if hc.ReplayConfig.Interval != 6*time.Hour {
		t.Errorf("expected ReplayInterval=6h, got %v", hc.ReplayConfig.Interval)
	}
	if !hc.EnableSeparation {
		t.Error("expected EnableSeparation=true")
	}
	if hc.SeparationConfig.RepulsionAlpha != 0.45 {
		t.Errorf("expected RepulsionAlpha=0.45, got %f", hc.SeparationConfig.RepulsionAlpha)
	}
	if !hc.EnableLoci {
		t.Error("expected EnableLoci=true")
	}
	if !hc.EnableCompletion {
		t.Error("expected EnableCompletion=true")
	}

	// Verify defaults are preserved for fields not in FeatureVector.
	defaults := DefaultHippocampalConfig()
	if hc.EpisodeConfig.TimeGap != defaults.EpisodeConfig.TimeGap {
		t.Errorf("expected TimeGap=%v (default), got %v", defaults.EpisodeConfig.TimeGap, hc.EpisodeConfig.TimeGap)
	}
	if hc.ReplayConfig.LearningRate != defaults.ReplayConfig.LearningRate {
		t.Errorf("expected LearningRate=%f (default), got %f", defaults.ReplayConfig.LearningRate, hc.ReplayConfig.LearningRate)
	}
}

func TestThompsonSamplingExploration(t *testing.T) {
	bs := NewBayesianSearcher(7)

	// Generate 30 configs with no feedback (pure prior exploration).
	// Expect diverse outputs — not all identical.
	type signature struct {
		ep, rp, sep, loci, comp bool
	}
	seen := make(map[signature]bool)
	for i := 0; i < 30; i++ {
		cfg := bs.NextConfig()
		sig := signature{cfg.EpisodesEnabled, cfg.ReplayEnabled, cfg.SeparationEnabled, cfg.LociEnabled, cfg.CompletionEnabled}
		seen[sig] = true
	}

	// With 5 boolean features and uniform priors, we expect at least 4 distinct
	// boolean combinations in 30 draws (32 possible).
	if len(seen) < 4 {
		t.Errorf("expected at least 4 distinct boolean combos in 30 draws, got %d", len(seen))
	}

	// Also check that continuous parameters show variation.
	thresholds := make(map[float64]bool)
	alphas := make(map[float64]bool)
	for i := 0; i < 30; i++ {
		cfg := bs.NextConfig()
		thresholds[cfg.SimilarityThreshold] = true
		alphas[cfg.SeparationAlpha] = true
	}
	if len(thresholds) < 2 {
		t.Errorf("expected at least 2 distinct thresholds, got %d", len(thresholds))
	}
	if len(alphas) < 2 {
		t.Errorf("expected at least 2 distinct alphas, got %d", len(alphas))
	}
}
