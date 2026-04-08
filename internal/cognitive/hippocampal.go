package cognitive

import "time"

// HippocampalConfig holds feature toggles for hippocampal cognitive subsystems.
// All features are disabled by default; set individual Enable* flags to opt in.
type HippocampalConfig struct {
	// EnableEpisodes activates the episode segmentation worker, which detects
	// context shifts between consecutive writes via cosine similarity and
	// creates same_episode associations.
	EnableEpisodes bool `json:"enable_episodes"`

	// Episode tuning knobs — only used when EnableEpisodes is true.
	EpisodeConfig EpisodeConfig `json:"episode_config"`

	// EnableSeparation activates hippocampal pattern separation.
	// When true, cross-context ACTIVATE results are penalised using entity
	// Jaccard similarity as a context mismatch signal.
	EnableSeparation bool `json:"enable_separation"`

	SeparationConfig SeparationConfig `json:"separation_config"`

	// EnableCompletion enables pattern completion mode for ACTIVATE.
	// When true, the 'complete' recall mode is available, which returns the
	// full episode containing the top activation result.
	EnableCompletion bool `json:"enable_completion"`

	// EnableLoci is reserved for a future background worker that maintains
	// cached communities. On-demand MCP tools (muninn_loci, muninn_locus_members)
	// are always available regardless of this flag.
	EnableLoci bool `json:"enable_loci"`

	LociConfig LociConfig `json:"loci_config"`

	// EnableReplay activates the hippocampal replay worker, which periodically
	// runs synthetic ACTIVATE calls on recent engrams to strengthen Hebbian
	// associations. Biological analogue: hippocampal sleep replay / consolidation.
	EnableReplay bool `json:"enable_replay"`

	ReplayConfig ReplayConfig `json:"replay_config"`
}

// EpisodeConfig holds tuning parameters for the episode segmentation worker.
type EpisodeConfig struct {
	// SimilarityThreshold is the cosine similarity below which two consecutive
	// embeddings are considered a context boundary (new episode). Default 0.5.
	SimilarityThreshold float64 `json:"similarity_threshold"`

	// TimeGap is the maximum wall-clock gap between writes before forcing
	// an episode boundary regardless of similarity. Default 30 minutes.
	TimeGap time.Duration `json:"time_gap"`

	// AssociationWeight is the initial weight assigned to same_episode
	// associations. Default 0.3.
	AssociationWeight float32 `json:"association_weight"`
}

// SeparationConfig tunes the pattern separation scoring adjustment.
type SeparationConfig struct {
	// RepulsionAlpha is the maximum score penalty applied to cross-context
	// candidates. A candidate with zero entity overlap receives a multiplier
	// of (1.0 - RepulsionAlpha). Must be in [0, 1). Zero disables the penalty. Default: 0.3.
	RepulsionAlpha float64

	// ContextMismatchFn selects the method used to detect context mismatch.
	// Currently only "entity" is supported: Jaccard similarity over the
	// entity sets of the query and candidate engrams. Default: "entity".
	ContextMismatchFn string
}

// LociConfig holds parameters for label propagation community detection.
type LociConfig struct {
	MaxIterations int `json:"max_iterations"` // label propagation max iterations (default: 100)
}

// ReplayConfig holds tuning parameters for the hippocampal replay worker.
type ReplayConfig struct {
	// Interval is how often the replay cycle runs. Default: 6 hours.
	Interval time.Duration `json:"interval"`

	// LearningRate is the dampened Hebbian learning rate for replay-triggered
	// activations. Lower than the normal rate (0.01) to avoid runaway
	// reinforcement. Default: 0.005.
	// NOTE: currently informational — the Hebbian worker uses a fixed rate.
	// This field is logged and reserved for future per-activation rate control.
	LearningRate float64 `json:"learning_rate"`

	// MaxEngrams is the maximum number of recent engrams to replay per vault
	// per cycle. Default: 100.
	MaxEngrams int `json:"max_engrams"`
}

// DefaultReplayConfig returns a ReplayConfig with production-ready defaults.
func DefaultReplayConfig() ReplayConfig {
	return ReplayConfig{
		Interval:     6 * time.Hour,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
}

// DefaultHippocampalConfig returns a HippocampalConfig with all features
// disabled and sane defaults for the tuning knobs.
func DefaultHippocampalConfig() HippocampalConfig {
	return HippocampalConfig{
		EnableEpisodes:   false,
		EpisodeConfig:    DefaultEpisodeConfig(),
		EnableSeparation: false,
		SeparationConfig: DefaultSeparationConfig(),
		EnableLoci:       false,
		LociConfig:       DefaultLociConfig(),
		EnableReplay:     false,
		ReplayConfig:     DefaultReplayConfig(),
	}
}

// DefaultEpisodeConfig returns an EpisodeConfig with production-ready defaults.
func DefaultEpisodeConfig() EpisodeConfig {
	return EpisodeConfig{
		SimilarityThreshold: 0.5,
		TimeGap:             30 * time.Minute,
		AssociationWeight:   0.3,
	}
}

// DefaultSeparationConfig returns conservative separation defaults.
func DefaultSeparationConfig() SeparationConfig {
	return SeparationConfig{
		RepulsionAlpha:    0.3,
		ContextMismatchFn: "entity",
	}
}

// DefaultLociConfig returns LociConfig with sensible defaults.
func DefaultLociConfig() LociConfig {
	return LociConfig{
		MaxIterations: 100,
	}
}
