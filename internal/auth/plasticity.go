package auth

// PlasticityConfig is the per-vault cognitive pipeline configuration.
// A nil PlasticityConfig means "use defaults" (equivalent to Preset: "default").
// Non-nil pointer fields override the chosen preset value.
type PlasticityConfig struct {
	Version int    `json:"version,omitempty"` // schema version, currently 1
	Preset  string `json:"preset,omitempty"`  // "default" | "reference" | "scratchpad" | "knowledge-graph"

	// Optional overrides (nil = use preset value)
	HebbianEnabled    *bool    `json:"hebbian_enabled,omitempty"`
	TemporalEnabled      *bool    `json:"temporal_enabled,omitempty"`
	AutoLinkNeighbors *bool    `json:"auto_link_neighbors,omitempty"` // semantic neighbor auto-linking
	HopDepth          *int     `json:"hop_depth,omitempty"`            // BFS hops 0–8
	SemanticWeight    *float32 `json:"semantic_weight,omitempty"`      // 0–1
	FTSWeight         *float32 `json:"fts_weight,omitempty"`           // 0–1
	RelevanceFloor        *float32 `json:"relevance_floor,omitempty"`          // 0–1
	TemporalHalflife    *float32 `json:"temporal_halflife,omitempty"`      // days
	TraversalProfile  *string  `json:"traversal_profile,omitempty"`    // "default"|"causal"|"confirmatory"|"adversarial"|"structural"; empty = use auto-inference
	// ACT-R parameters (new, preferred over Ebbinghaus fields)
	ACTRDecay    *float64 `json:"actr_decay,omitempty"`     // power-law exponent d (default 0.5)
	ACTRHebScale *float64 `json:"actr_heb_scale,omitempty"` // Hebbian amplifier (default 4.0)

	// Experimental scoring models
	ExperimentalCGDN *bool `json:"experimental_cgdn,omitempty"` // enable CGDN scorer (default false)

	// Predictive Activation Signal (PAS)
	PredictiveActivation *bool `json:"predictive_activation,omitempty"`
	PASMaxInjections     *int  `json:"pas_max_injections,omitempty"` // 0-10, default 5

	// Pruning policy (both are zero/disabled by default)
	MaxEngrams    *int     `json:"max_engrams,omitempty"`    // max engrams per vault; 0 = no limit
	RetentionDays *float32 `json:"retention_days,omitempty"` // max age in days; 0 = no limit

	// Association edge decay (applied each prune pass, ~60s)
	AssocDecayFactor *float32 `json:"assoc_decay_factor,omitempty"` // multiplier per pass (e.g. 0.95 = 5% decay); 0 = disabled
	AssocMinWeight   *float32 `json:"assoc_min_weight,omitempty"`   // edges below this are deleted (e.g. 0.05)
	ArchiveThreshold *float64 `json:"archive_threshold,omitempty"` // consolidation score threshold for archiving (default 0.05)

	// Behavior mode controls how AI agents use memory
	BehaviorMode         *string `json:"behavior_mode,omitempty"`          // "autonomous"|"prompted"|"selective"|"custom"
	BehaviorInstructions *string `json:"behavior_instructions,omitempty"` // freeform text for "custom" mode

	// Inline enrichment controls how caller-provided enrichment interacts with background enrichment
	InlineEnrichment *string `json:"inline_enrichment,omitempty"` // "caller_only"|"caller_preferred"|"background_only"|"disabled"

	// EnrichmentEnabled is a vault-level kill switch for all enrichment (both inline and background).
	// Default: true. When false, skip ALL enrichment for engrams in this vault.
	EnrichmentEnabled *bool `json:"enrichment_enabled,omitempty"`

	// RecallMode is the default recall mode for this vault: "semantic"|"recent"|"balanced"|"deep".
	// nil = use "balanced" (engine defaults).
	RecallMode *string `json:"recall_mode,omitempty"`

	// ScoringFusion selects the Phase 6 scoring strategy.
	// "rrf" = use Phase 3 RRF scores directly (rank-based, scale-invariant).
	// "weighted_sum" = use legacy weighted-sum scoring (DisableACTR implied).
	// nil/empty = default (ACT-R scoring, unchanged behavior).
	ScoringFusion *string `json:"scoring_fusion,omitempty"`
}

// ResolvedPlasticity is the fully-merged configuration after applying preset defaults
// and any field-level overrides from PlasticityConfig.
// Weight fields (SemanticWeight, FTSWeight, HebbianWeight, TemporalWeight, RecencyWeight)
// are independent multipliers and are not required to sum to 1.0.
// The engine may normalize or use them as-is depending on the activation context.
type ResolvedPlasticity struct {
	HebbianEnabled    bool    `json:"hebbian_enabled"`
	TemporalEnabled      bool    `json:"temporal_enabled"`
	AutoLinkNeighbors bool    `json:"auto_link_neighbors"`
	HopDepth          int     `json:"hop_depth"`
	SemanticWeight    float32 `json:"semantic_weight"`
	FTSWeight         float32 `json:"fts_weight"`
	RelevanceFloor        float32 `json:"relevance_floor"`
	TemporalHalflife    float32 `json:"temporal_halflife"` // days
	HebbianWeight     float32 `json:"hebbian_weight"`
	TemporalWeight       float32 `json:"temporal_weight"`
	RecencyWeight     float32 `json:"recency_weight"`
	TraversalProfile  string  `json:"traversal_profile"` // empty string = use auto-inference
	// ACT-R parameters (new, preferred over Ebbinghaus fields)
	ACTRDecay    float64 `json:"actr_decay"`
	ACTRHebScale float64 `json:"actr_heb_scale"`
	// Experimental scoring models
	ExperimentalCGDN bool `json:"experimental_cgdn"`
	// Predictive Activation Signal (PAS)
	PredictiveActivation bool `json:"predictive_activation"`
	PASMaxInjections     int  `json:"pas_max_injections"`
	// Pruning policy
	MaxEngrams    int     `json:"max_engrams"`    // 0 = no limit
	RetentionDays float32 `json:"retention_days"` // 0 = no limit
	// Association edge decay
	AssocDecayFactor float32 `json:"assoc_decay_factor"` // multiplier per prune pass; 0 = disabled
	AssocMinWeight   float32 `json:"assoc_min_weight"`   // edges below this are deleted
	ArchiveThreshold float64 `json:"archive_threshold"`  // consolidation score threshold for archiving
	// Behavior mode
	BehaviorMode         string `json:"behavior_mode"`
	BehaviorInstructions string `json:"behavior_instructions"`
	// Inline enrichment mode
	InlineEnrichment string `json:"inline_enrichment"` // "caller_only", "caller_preferred", "background_only", "disabled"
	// EnrichmentEnabled is a vault-level kill switch for all enrichment.
	EnrichmentEnabled bool `json:"enrichment_enabled"`
	// RecallMode is the default recall mode for this vault.
	RecallMode string `json:"recall_mode"`
	// ScoringFusion selects Phase 6 scoring strategy: "" (default=ACT-R), "rrf", or "weighted_sum".
	ScoringFusion string `json:"scoring_fusion"`
}

type plasticityPreset struct {
	HebbianEnabled    bool
	TemporalEnabled      bool
	AutoLinkNeighbors bool
	HopDepth          int
	SemanticWeight    float32
	FTSWeight         float32
	RelevanceFloor        float32
	TemporalHalflife    float32
	HebbianWeight     float32
	TemporalWeight       float32
	RecencyWeight     float32
	ACTRDecay            float64
	ACTRHebScale         float64
	ExperimentalCGDN     bool
	PredictiveActivation bool
	PASMaxInjections     int
	MaxEngrams           int
	RetentionDays        float32
	AssocDecayFactor     float32
	AssocMinWeight       float32
	ArchiveThreshold     float64
	BehaviorMode         string
	InlineEnrichment  string
	EnrichmentEnabled bool
	RecallMode        string
	ScoringFusion     string // "" = default (ACT-R), "rrf", "weighted_sum"
}

var plasticityPresets = map[string]plasticityPreset{
	"default": {
		HebbianEnabled:       true,
		TemporalEnabled:      true,
		AutoLinkNeighbors:    true,
		HopDepth:             2,
		SemanticWeight:       0.6,
		FTSWeight:            0.3,
		RelevanceFloor:       0.05,
		TemporalHalflife:     30,
		HebbianWeight:        0.5,
		TemporalWeight:       0.4,
		RecencyWeight:        0.3,
		ACTRDecay:            0.5,
		ACTRHebScale:         4.0,
		PredictiveActivation: true,
		PASMaxInjections:     5,
		MaxEngrams:           0,
		RetentionDays:        0,
		AssocDecayFactor:     0.95,
		AssocMinWeight:       0.05,
		ArchiveThreshold:     0.05,
		BehaviorMode:         "autonomous",
		InlineEnrichment:     "caller_preferred",
		EnrichmentEnabled:    true,
		RecallMode:           "balanced",

	},
	"reference": {
		HebbianEnabled:       true,
		TemporalEnabled:      false,
		AutoLinkNeighbors:    true,
		HopDepth:             3,
		SemanticWeight:       0.7,
		FTSWeight:            0.5,
		RelevanceFloor:       1.0,
		TemporalHalflife:     365,
		HebbianWeight:        0.6,
		TemporalWeight:       0.0,
		RecencyWeight:        0.1,
		ACTRDecay:            0.2,
		ACTRHebScale:         4.0,
		PredictiveActivation: true,
		PASMaxInjections:     5,
		MaxEngrams:           0,
		RetentionDays:        0,
		AssocDecayFactor:     0.95,
		AssocMinWeight:       0.05,
		ArchiveThreshold:     0.05,
		BehaviorMode:         "autonomous",
		InlineEnrichment:     "caller_preferred",
		EnrichmentEnabled:    true,
		RecallMode:           "balanced",

	},
	"scratchpad": {
		HebbianEnabled:       false,
		TemporalEnabled:      true,
		AutoLinkNeighbors:    true,
		HopDepth:             0,
		SemanticWeight:       0.5,
		FTSWeight:            0.4,
		RelevanceFloor:       0.01,
		TemporalHalflife:     7,
		HebbianWeight:        0.0,
		TemporalWeight:       0.8,
		RecencyWeight:        0.5,
		ACTRDecay:            0.8,
		ACTRHebScale:         2.0,
		PredictiveActivation: false,
		PASMaxInjections:     0,
		MaxEngrams:           0,
		RetentionDays:        0,
		AssocDecayFactor:     0,
		AssocMinWeight:       0,
		ArchiveThreshold:     0.05,
		BehaviorMode:         "selective",
		InlineEnrichment:     "caller_preferred",
		EnrichmentEnabled:    true,
		RecallMode:           "balanced",

	},
	"knowledge-graph": {
		HebbianEnabled:       true,
		TemporalEnabled:      true,
		AutoLinkNeighbors:    true,
		HopDepth:             4,
		SemanticWeight:       0.5,
		FTSWeight:            0.2,
		RelevanceFloor:       0.1,
		TemporalHalflife:     60,
		HebbianWeight:        0.8,
		TemporalWeight:       0.2,
		RecencyWeight:        0.2,
		ACTRDecay:            0.3,
		ACTRHebScale:         8.0,
		PredictiveActivation: true,
		PASMaxInjections:     5,
		MaxEngrams:           0,
		RetentionDays:        0,
		AssocDecayFactor:     0.98,
		AssocMinWeight:       0.03,
		ArchiveThreshold:     0.05,
		BehaviorMode:         "autonomous",
		InlineEnrichment:     "caller_preferred",
		EnrichmentEnabled:    true,
		RecallMode:           "balanced",

	},
}

// ResolvePlasticity merges cfg (which may be nil) atop its chosen preset,
// returning a fully-populated ResolvedPlasticity.
func ResolvePlasticity(cfg *PlasticityConfig) ResolvedPlasticity {
	presetName := "default"
	if cfg != nil && cfg.Preset != "" {
		presetName = cfg.Preset
	}
	p, ok := plasticityPresets[presetName]
	if !ok {
		p = plasticityPresets["default"]
	}

	r := ResolvedPlasticity{
		HebbianEnabled:       p.HebbianEnabled,
		TemporalEnabled:      p.TemporalEnabled,
		AutoLinkNeighbors:    p.AutoLinkNeighbors,
		HopDepth:             p.HopDepth,
		SemanticWeight:       p.SemanticWeight,
		FTSWeight:            p.FTSWeight,
		RelevanceFloor:       p.RelevanceFloor,
		TemporalHalflife:     p.TemporalHalflife,
		HebbianWeight:        p.HebbianWeight,
		TemporalWeight:       p.TemporalWeight,
		RecencyWeight:        p.RecencyWeight,
		ACTRDecay:            p.ACTRDecay,
		ACTRHebScale:         p.ACTRHebScale,
		ExperimentalCGDN:     p.ExperimentalCGDN,
		PredictiveActivation: p.PredictiveActivation,
		PASMaxInjections:     p.PASMaxInjections,
		MaxEngrams:           p.MaxEngrams,
		RetentionDays:        p.RetentionDays,
		AssocDecayFactor:     p.AssocDecayFactor,
		AssocMinWeight:       p.AssocMinWeight,
		ArchiveThreshold:     p.ArchiveThreshold,
		BehaviorMode:         p.BehaviorMode,
		InlineEnrichment:     p.InlineEnrichment,
		EnrichmentEnabled:    p.EnrichmentEnabled,
		RecallMode:           p.RecallMode,
		ScoringFusion:        p.ScoringFusion,
	}

	if cfg == nil {
		return r
	}

	// Apply pointer-field overrides
	if cfg.HebbianEnabled != nil {
		r.HebbianEnabled = *cfg.HebbianEnabled
	}
	if cfg.TemporalEnabled != nil {
		r.TemporalEnabled = *cfg.TemporalEnabled
	}
	if cfg.AutoLinkNeighbors != nil {
		r.AutoLinkNeighbors = *cfg.AutoLinkNeighbors
	}
	if cfg.HopDepth != nil {
		r.HopDepth = *cfg.HopDepth
		if r.HopDepth < 0 {
			r.HopDepth = 0
		}
		if r.HopDepth > 8 {
			r.HopDepth = 8
		}
	}
	if cfg.SemanticWeight != nil {
		r.SemanticWeight = *cfg.SemanticWeight
		if r.SemanticWeight < 0 {
			r.SemanticWeight = 0
		}
		if r.SemanticWeight > 1 {
			r.SemanticWeight = 1
		}
	}
	if cfg.FTSWeight != nil {
		r.FTSWeight = *cfg.FTSWeight
		if r.FTSWeight < 0 {
			r.FTSWeight = 0
		}
		if r.FTSWeight > 1 {
			r.FTSWeight = 1
		}
	}
	if cfg.RelevanceFloor != nil {
		r.RelevanceFloor = *cfg.RelevanceFloor
		if r.RelevanceFloor < 0 {
			r.RelevanceFloor = 0
		}
		if r.RelevanceFloor > 1 {
			r.RelevanceFloor = 1
		}
	}
	if cfg.TemporalHalflife != nil {
		stability := *cfg.TemporalHalflife
		if stability > 0 {
			r.TemporalHalflife = stability
		}
	}
	if cfg.TraversalProfile != nil {
		r.TraversalProfile = *cfg.TraversalProfile
	}
	if cfg.ACTRDecay != nil {
		d := *cfg.ACTRDecay
		if d < 0.01 {
			d = 0.01
		}
		if d > 2.0 {
			d = 2.0
		}
		r.ACTRDecay = d
	}
	if cfg.ACTRHebScale != nil {
		s := *cfg.ACTRHebScale
		if s < 0.0 {
			s = 0.0
		}
		if s > 50.0 {
			s = 50.0
		}
		r.ACTRHebScale = s
	}
	if cfg.ExperimentalCGDN != nil {
		r.ExperimentalCGDN = *cfg.ExperimentalCGDN
	}
	if cfg.PredictiveActivation != nil {
		r.PredictiveActivation = *cfg.PredictiveActivation
	}
	if cfg.PASMaxInjections != nil {
		v := *cfg.PASMaxInjections
		if v < 0 {
			v = 0
		}
		if v > 10 {
			v = 10
		}
		r.PASMaxInjections = v
	}
	if cfg.MaxEngrams != nil && *cfg.MaxEngrams >= 0 {
		r.MaxEngrams = *cfg.MaxEngrams
	}
	if cfg.RetentionDays != nil && *cfg.RetentionDays >= 0 {
		r.RetentionDays = *cfg.RetentionDays
	}
	if cfg.AssocDecayFactor != nil {
		f := *cfg.AssocDecayFactor
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		r.AssocDecayFactor = f
	}
	if cfg.AssocMinWeight != nil {
		w := *cfg.AssocMinWeight
		if w < 0 {
			w = 0
		}
		if w > 1 {
			w = 1
		}
		r.AssocMinWeight = w
	}
	if cfg.ArchiveThreshold != nil {
		v := *cfg.ArchiveThreshold
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		r.ArchiveThreshold = v
	}
	if cfg.BehaviorMode != nil {
		if validBehaviorMode(*cfg.BehaviorMode) {
			r.BehaviorMode = *cfg.BehaviorMode
		} else {
			r.BehaviorMode = "autonomous"
		}
	}
	if cfg.BehaviorInstructions != nil {
		r.BehaviorInstructions = *cfg.BehaviorInstructions
	}
	if cfg.InlineEnrichment != nil {
		if validInlineEnrichment(*cfg.InlineEnrichment) {
			r.InlineEnrichment = *cfg.InlineEnrichment
		} else {
			r.InlineEnrichment = "caller_preferred"
		}
	}
	if cfg.EnrichmentEnabled != nil {
		r.EnrichmentEnabled = *cfg.EnrichmentEnabled
	}
	if cfg.RecallMode != nil && ValidRecallMode(*cfg.RecallMode) {
		r.RecallMode = *cfg.RecallMode
	}
	if cfg.ScoringFusion != nil {
		if ValidScoringFusion(*cfg.ScoringFusion) {
			r.ScoringFusion = *cfg.ScoringFusion
		} else {
			r.ScoringFusion = "" // invalid → default (ACT-R)
		}
	}
	return r
}

var validBehaviorModes = map[string]bool{
	"autonomous": true,
	"prompted":   true,
	"selective":  true,
	"custom":     true,
}

func validBehaviorMode(s string) bool {
	return validBehaviorModes[s]
}

var validInlineEnrichments = map[string]bool{
	"caller_only":      true,
	"caller_preferred": true,
	"background_only":  true,
	"disabled":         true,
}

func validInlineEnrichment(s string) bool {
	return validInlineEnrichments[s]
}

// ValidRecallMode returns true if s is a known recall mode name.
func ValidRecallMode(s string) bool {
	switch s {
	case "semantic", "recent", "balanced", "deep":
		return true
	}
	return false
}

// ValidPlasticityPreset returns true if s is a known preset name.
func ValidPlasticityPreset(s string) bool {
	_, ok := plasticityPresets[s]
	return ok
}

// ValidScoringFusion returns true if s is a known scoring fusion mode.
// Empty string is valid (means "use default ACT-R scoring").
func ValidScoringFusion(s string) bool {
	switch s {
	case "", "rrf", "weighted_sum":
		return true
	}
	return false
}
