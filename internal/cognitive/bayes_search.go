package cognitive

import (
	"math"
	"math/rand"
	"sort"
	"time"
)

// FeatureVector covers the primary feature toggles and key numeric knobs.
// Additional parameters (episode window size, replay dampening, loci resolution,
// separation context signals) can be added as the feature set stabilizes.
type FeatureVector struct {
	EpisodesEnabled     bool
	SimilarityThreshold float64 // 0.1-0.5
	ReplayEnabled       bool
	ReplayInterval      time.Duration
	SeparationEnabled   bool
	SeparationAlpha     float64 // 0.05-0.30
	LociEnabled         bool
	CompletionEnabled   bool
}

// BenchmarkResult captures the quality metrics for a feature configuration.
type BenchmarkResult struct {
	Config    FeatureVector
	Precision float64       // precision@K
	Recall    float64       // recall@K
	MRR       float64       // mean reciprocal rank
	Latency   time.Duration // query latency
	Score     float64       // composite score
}

// betaArm holds the alpha/beta parameters for a single Thompson sampling arm.
type betaArm struct {
	alpha float64
	beta  float64
}

// sample draws from Beta(alpha, beta) using the Joehnk method.
func (b *betaArm) sample(rng *rand.Rand) float64 {
	return betaSample(rng, b.alpha, b.beta)
}

// Number of buckets for discretized continuous parameters.
const numBuckets = 5

// Bucket boundaries for SimilarityThreshold: [0.1, 0.2, 0.3, 0.4, 0.5].
var thresholdBuckets = [numBuckets]float64{0.1, 0.2, 0.3, 0.4, 0.5}

// Bucket boundaries for SeparationAlpha: [0.05, 0.10, 0.15, 0.20, 0.30].
var alphaBuckets = [numBuckets]float64{0.05, 0.10, 0.15, 0.20, 0.30}

// Bucket boundaries for ReplayInterval: 1h, 3h, 6h, 12h, 24h.
var intervalBuckets = [numBuckets]time.Duration{
	1 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

// BayesianSearcher uses Thompson sampling to explore the feature space.
type BayesianSearcher struct {
	results []BenchmarkResult
	rng     *rand.Rand

	// Boolean feature arms.
	episodes   betaArm
	replay     betaArm
	separation betaArm
	loci       betaArm
	completion betaArm

	// Continuous parameter arms (one per bucket).
	thresholdArms [numBuckets]betaArm
	alphaArms     [numBuckets]betaArm
	intervalArms  [numBuckets]betaArm
}

// NewBayesianSearcher creates a searcher seeded with the given value.
// All arms start with Beta(1,1) — the uniform prior.
func NewBayesianSearcher(seed int64) *BayesianSearcher {
	bs := &BayesianSearcher{
		rng: rand.New(rand.NewSource(seed)),
	}
	// Initialise boolean arms with uniform prior.
	uniform := betaArm{alpha: 1, beta: 1}
	bs.episodes = uniform
	bs.replay = uniform
	bs.separation = uniform
	bs.loci = uniform
	bs.completion = uniform

	// Initialise continuous arms.
	for i := 0; i < numBuckets; i++ {
		bs.thresholdArms[i] = uniform
		bs.alphaArms[i] = uniform
		bs.intervalArms[i] = uniform
	}
	return bs
}

// NextConfig returns the next feature configuration to try using Thompson sampling.
func (bs *BayesianSearcher) NextConfig() FeatureVector {
	fv := FeatureVector{
		EpisodesEnabled:   bs.episodes.sample(bs.rng) > 0.5,
		ReplayEnabled:     bs.replay.sample(bs.rng) > 0.5,
		SeparationEnabled: bs.separation.sample(bs.rng) > 0.5,
		LociEnabled:       bs.loci.sample(bs.rng) > 0.5,
		CompletionEnabled: bs.completion.sample(bs.rng) > 0.5,
	}

	// Pick the bucket with the highest Thompson sample for each continuous param.
	fv.SimilarityThreshold = thresholdBuckets[bs.bestBucket(bs.thresholdArms[:])]
	fv.SeparationAlpha = alphaBuckets[bs.bestBucket(bs.alphaArms[:])]
	fv.ReplayInterval = intervalBuckets[bs.bestBucket(bs.intervalArms[:])]

	return fv
}

// bestBucket samples each bucket arm and returns the index with the highest draw.
func (bs *BayesianSearcher) bestBucket(arms []betaArm) int {
	best := 0
	bestVal := -1.0
	for i := range arms {
		v := arms[i].sample(bs.rng)
		if v > bestVal {
			bestVal = v
			best = i
		}
	}
	return best
}

// RecordResult records a benchmark result and updates the Thompson sampling arms.
func (bs *BayesianSearcher) RecordResult(result BenchmarkResult) {
	// Compute median before appending to avoid self-inclusion bias.
	med := bs.medianScore()
	bs.results = append(bs.results, result)
	good := result.Score > med

	// Update boolean arms.
	bs.updateBoolArm(&bs.episodes, result.Config.EpisodesEnabled, good)
	bs.updateBoolArm(&bs.replay, result.Config.ReplayEnabled, good)
	bs.updateBoolArm(&bs.separation, result.Config.SeparationEnabled, good)
	bs.updateBoolArm(&bs.loci, result.Config.LociEnabled, good)
	bs.updateBoolArm(&bs.completion, result.Config.CompletionEnabled, good)

	// Update continuous arms only when their parent feature is enabled.
	if result.Config.EpisodesEnabled {
		bs.updateContinuousArm(bs.thresholdArms[:], thresholdBuckets[:], result.Config.SimilarityThreshold, good)
	}
	if result.Config.SeparationEnabled {
		bs.updateContinuousArm(bs.alphaArms[:], alphaBuckets[:], result.Config.SeparationAlpha, good)
	}

	// For ReplayInterval, only update when replay is enabled.
	if result.Config.ReplayEnabled {
		for i, iv := range intervalBuckets {
			if result.Config.ReplayInterval == iv {
				if good {
					bs.intervalArms[i].alpha++
				} else {
					bs.intervalArms[i].beta++
				}
				break
			}
		}
	}
}

// updateBoolArm adjusts a boolean feature arm based on whether it was enabled and result quality.
func (bs *BayesianSearcher) updateBoolArm(arm *betaArm, enabled, good bool) {
	if enabled {
		if good {
			arm.alpha++
		} else {
			arm.beta++
		}
	}
}

// updateContinuousArm finds the matching bucket for val and updates it.
func (bs *BayesianSearcher) updateContinuousArm(arms []betaArm, buckets []float64, val float64, good bool) {
	closest := 0
	minDist := math.Abs(val - buckets[0])
	for i := 1; i < len(buckets); i++ {
		d := math.Abs(val - buckets[i])
		if d < minDist {
			minDist = d
			closest = i
		}
	}
	if good {
		arms[closest].alpha++
	} else {
		arms[closest].beta++
	}
}

// medianScore returns the median composite score across all recorded results.
// Returns 0 if no results exist yet (making the first result always "good").
func (bs *BayesianSearcher) medianScore() float64 {
	n := len(bs.results)
	if n == 0 {
		return 0
	}
	scores := make([]float64, n)
	for i, r := range bs.results {
		scores[i] = r.Score
	}
	sort.Float64s(scores)
	if n%2 == 0 {
		return (scores[n/2-1] + scores[n/2]) / 2
	}
	return scores[n/2]
}

// BestConfig returns the configuration with the highest composite score.
// Panics if no results have been recorded.
func (bs *BayesianSearcher) BestConfig() (FeatureVector, BenchmarkResult) {
	if len(bs.results) == 0 {
		panic("BayesianSearcher.BestConfig: no results recorded")
	}
	best := bs.results[0]
	for _, r := range bs.results[1:] {
		if r.Score > best.Score {
			best = r
		}
	}
	return best.Config, best
}

// ToHippocampalConfig converts a FeatureVector to a HippocampalConfig.
func (fv FeatureVector) ToHippocampalConfig() HippocampalConfig {
	cfg := DefaultHippocampalConfig()

	cfg.EnableEpisodes = fv.EpisodesEnabled
	cfg.EpisodeConfig.SimilarityThreshold = fv.SimilarityThreshold

	cfg.EnableReplay = fv.ReplayEnabled
	cfg.ReplayConfig.Interval = fv.ReplayInterval

	cfg.EnableSeparation = fv.SeparationEnabled
	cfg.SeparationConfig.RepulsionAlpha = fv.SeparationAlpha

	cfg.EnableLoci = fv.LociEnabled
	cfg.EnableCompletion = fv.CompletionEnabled

	return cfg
}

// betaSample draws a sample from Beta(alpha, beta) using the gamma function approach.
// For alpha,beta >= 1 this uses Gamma variates: if X ~ Gamma(a,1) and Y ~ Gamma(b,1)
// then X/(X+Y) ~ Beta(a,b).
func betaSample(rng *rand.Rand, alpha, beta float64) float64 {
	x := gammaSample(rng, alpha)
	y := gammaSample(rng, beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaSample draws from Gamma(shape, 1) using Marsaglia and Tsang's method.
// Handles shape >= 1 directly; for 0 < shape < 1, uses the boost trick.
func gammaSample(rng *rand.Rand, shape float64) float64 {
	if shape < 1 {
		// Boost: Gamma(a,1) = Gamma(a+1,1) * U^(1/a)
		return gammaSample(rng, shape+1) * math.Pow(rng.Float64(), 1.0/shape)
	}

	d := shape - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)

	for {
		var x, v float64
		for {
			x = rng.NormFloat64()
			v = 1.0 + c*x
			if v > 0 {
				break
			}
		}
		v = v * v * v
		u := rng.Float64()

		// Squeeze step.
		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v
		}
		if math.Log(u) < 0.5*x*x+d*(1.0-v+math.Log(v)) {
			return d * v
		}
	}
}
