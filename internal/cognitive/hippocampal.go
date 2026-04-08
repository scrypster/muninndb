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

// DefaultHippocampalConfig returns a HippocampalConfig with all features
// disabled and sane defaults for the tuning knobs.
func DefaultHippocampalConfig() HippocampalConfig {
	return HippocampalConfig{
		EnableEpisodes: false,
		EpisodeConfig:  DefaultEpisodeConfig(),
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
