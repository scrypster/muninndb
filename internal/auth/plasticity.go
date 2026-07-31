package auth

import "strings"

// PlasticityConfig is the per-vault cognitive pipeline configuration.
// A nil PlasticityConfig means "use defaults" (equivalent to Preset: "default").
// Non-nil pointer fields override the chosen preset value.
type PlasticityConfig struct {
	Version int    `json:"version,omitempty"` // schema version, currently 1
	Preset  string `json:"preset,omitempty"`  // "default" | "reference" | "scratchpad" | "knowledge-graph" | "working"

	// Optional overrides (nil = use preset value)
	HebbianEnabled    *bool    `json:"hebbian_enabled,omitempty"`
	TemporalEnabled   *bool    `json:"temporal_enabled,omitempty"`
	AutoLinkNeighbors *bool    `json:"auto_link_neighbors,omitempty"` // semantic neighbor auto-linking
	HopDepth          *int     `json:"hop_depth,omitempty"`           // BFS hops 0–8
	SemanticWeight    *float32 `json:"semantic_weight,omitempty"`     // 0–1
	FTSWeight         *float32 `json:"fts_weight,omitempty"`          // 0–1
	RelevanceFloor    *float32 `json:"relevance_floor,omitempty"`     // 0–1
	TemporalHalflife  *float32 `json:"temporal_halflife,omitempty"`   // days
	TraversalProfile  *string  `json:"traversal_profile,omitempty"`   // "default"|"causal"|"confirmatory"|"adversarial"|"structural"; empty = use auto-inference
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
	ArchiveThreshold *float64 `json:"archive_threshold,omitempty"`  // consolidation score threshold for archiving (default 0.05)

	// Behavior mode controls how AI agents use memory
	BehaviorMode         *string `json:"behavior_mode,omitempty"`         // "autonomous"|"prompted"|"selective"|"custom"
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

	// Long-Term Potentiation (LTP) for Hebbian associations.
	// Associations co-activated beyond LTPThreshold become potentiated,
	// enforcing a higher weight floor that resists decay.
	// All zero/nil = disabled (backward compatible).
	LTPThreshold   *int     `json:"ltp_threshold,omitempty"`    // co-activation count to trigger LTP (0 = disabled)
	LTPWeightFloor *float32 `json:"ltp_weight_floor,omitempty"` // minimum weight for potentiated edges (0–1; 0 = disabled)

	// ExcludeUntrusted controls whether untrusted engrams are filtered from ACTIVATE results.
	// nil = false (default: include all engrams regardless of trust).
	ExcludeUntrusted *bool `json:"exclude_untrusted,omitempty"`

	// MultiUser marks the vault as shared by multiple users or agents. When set,
	// guidance surfaces (muninn_guide, empty-recall hints) steer clients away from
	// vault-global recency tools (muninn_where_left_off, muninn_session) toward
	// per-user scoped recall (e.g. a per-user tag in the recall context).
	// nil = false (single-user; guidance unchanged).
	MultiUser *bool `json:"multi_user,omitempty"`

	// ReinforceOnRead controls whether Engine.Read (by-id) fires a
	// fire-and-forget TouchAccess (#682) to bump AccessCount/LastAccess for
	// the read engram. nil = true (default: reads reinforce). Recall/Activate
	// NEVER reinforces regardless of this flag (COG-12) — this only gates the
	// explicit-read-by-id channel.
	ReinforceOnRead *bool `json:"reinforce_on_read,omitempty"`

	// SemanticFloor overrides the COG-26 semantic-abstention baseline b for
	// this vault (see internal/plugin/embed/baseline.go), instead of the
	// registry value looked up from the vault's embed model. nil = use the
	// registry (or identity+WARN if the model is unregistered). An explicit
	// 0 disables the floor (identity transform) for operators who have
	// verified their embedder needs no calibration. Range [0,1); values >=1
	// would zero every match and are rejected at resolution (clamped to the
	// registry/identity fallback, never silently producing a floor that
	// abstains everything).
	SemanticFloor *float64 `json:"semantic_floor,omitempty"`

	// ExcludeTags is a per-vault list of tags excluded from recall RANKING.
	// A candidate carrying any of these tags is dropped from activation results
	// (the recall pipeline) before scoring. This is ranking-only: the engram is
	// NOT deleted, NOT hidden from explicit direct-id or as_of-by-id reads, and
	// still counts toward the vault. An explicit per-request tag include
	// (tags_all/tags_any naming an excluded tag) overrides the standing exclude
	// for that request — caller intent wins. Not preset-varying; default-empty /
	// opt-in: nil or empty means no exclusion, so a vault without this config
	// recalls byte-identically to before (#713).
	ExcludeTags []string `json:"exclude_tags,omitempty"`
}

// ResolvedPlasticity is the fully-merged configuration after applying preset defaults
// and any field-level overrides from PlasticityConfig.
// Weight fields (SemanticWeight, FTSWeight, HebbianWeight, TemporalWeight, RecencyWeight)
// are independent multipliers and are not required to sum to 1.0.
// The engine may normalize or use them as-is depending on the activation context.
type ResolvedPlasticity struct {
	HebbianEnabled    bool    `json:"hebbian_enabled"`
	TemporalEnabled   bool    `json:"temporal_enabled"`
	AutoLinkNeighbors bool    `json:"auto_link_neighbors"`
	HopDepth          int     `json:"hop_depth"`
	SemanticWeight    float32 `json:"semantic_weight"`
	FTSWeight         float32 `json:"fts_weight"`
	RelevanceFloor    float32 `json:"relevance_floor"`
	TemporalHalflife  float32 `json:"temporal_halflife"` // days
	HebbianWeight     float32 `json:"hebbian_weight"`
	TemporalWeight    float32 `json:"temporal_weight"`
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
	// MultiUser: vault is shared by multiple users/agents; guidance surfaces
	// steer clients from vault-global recency tools to per-user scoped recall.
	MultiUser bool `json:"multi_user"`
	// Inline enrichment mode
	InlineEnrichment string `json:"inline_enrichment"` // "caller_only", "caller_preferred", "background_only", "disabled"
	// EnrichmentEnabled is a vault-level kill switch for all enrichment.
	EnrichmentEnabled bool `json:"enrichment_enabled"`
	// RecallMode is the default recall mode for this vault.
	RecallMode string `json:"recall_mode"`
	// ScoringFusion selects Phase 6 scoring strategy: "" (default=ACT-R), "rrf", or "weighted_sum".
	ScoringFusion string `json:"scoring_fusion"`
	// LTP (Long-Term Potentiation) resolved values. Zero = disabled.
	LTPThreshold   int     `json:"ltp_threshold"`
	LTPWeightFloor float32 `json:"ltp_weight_floor"`
	// ExcludeUntrusted: when true, ACTIVATE silently skips engrams with TrustUntrusted.
	ExcludeUntrusted bool `json:"exclude_untrusted"`
	// ReinforceOnRead: when true, Engine.Read (by-id) fires TouchAccess (#682)
	// to bump AccessCount/LastAccess. Default true. Never applies to
	// Recall/Activate (COG-12) — that path never reinforces.
	ReinforceOnRead bool `json:"reinforce_on_read"`

	// SemanticFloorOverride, when non-nil, replaces the COG-26 registry
	// lookup for this vault's semantic-abstention baseline b. nil = no
	// override (use the per-embedder registry). See PlasticityConfig.SemanticFloor.
	SemanticFloorOverride *float64 `json:"semantic_floor_override,omitempty"`

	// ExcludeTags is the resolved per-vault recall-ranking tag exclusion list
	// (see PlasticityConfig.ExcludeTags). nil/empty = no exclusion. Normalized
	// at resolution: blank entries dropped, duplicates collapsed. Not
	// preset-varying.
	ExcludeTags []string `json:"exclude_tags,omitempty"`
}

type plasticityPreset struct {
	HebbianEnabled       bool
	TemporalEnabled      bool
	AutoLinkNeighbors    bool
	HopDepth             int
	SemanticWeight       float32
	FTSWeight            float32
	RelevanceFloor       float32
	TemporalHalflife     float32
	HebbianWeight        float32
	TemporalWeight       float32
	RecencyWeight        float32
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
	InlineEnrichment     string
	EnrichmentEnabled    bool
	RecallMode           string
	ScoringFusion        string // "" = default (ACT-R), "rrf", "weighted_sum"
	LTPThreshold         int
	LTPWeightFloor       float32
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
	"working": {
		// default-derived cognition (Hebbian + PAS on) for a shared workflow
		// vault, with two changes: scratch auto-evaporates, and agents get
		// "selective" persistence guidance. See RFC #597.
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
		RetentionDays:        7,
		AssocDecayFactor:     0.95,
		AssocMinWeight:       0.05,
		ArchiveThreshold:     0.05,
		BehaviorMode:         "selective",
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
		LTPThreshold:         p.LTPThreshold,
		LTPWeightFloor:       p.LTPWeightFloor,
		// ReinforceOnRead is not preset-varying: default true across every
		// preset, overridable per-vault via cfg.ReinforceOnRead below.
		ReinforceOnRead: true,
	}

	if cfg == nil {
		return r
	}

	// SemanticFloor is not preset-varying (like ReinforceOnRead above):
	// nil = no override, use the COG-26 registry. Validated at the resolution
	// site (internal/engine/engine.go), not here — clamping a bad value
	// silently here would hide a misconfiguration the caller should see.
	r.SemanticFloorOverride = cfg.SemanticFloor

	// ExcludeTags is not preset-varying (like SemanticFloor/ReinforceOnRead):
	// nil/empty = no exclusion. Normalized here so a nil, empty, or
	// duplicate-laden config all resolve to the same minimal set — and an empty
	// or all-blank config resolves to nil, byte-identical to "no config" (the
	// default-empty / opt-in invariant, #713).
	r.ExcludeTags = resolveExcludeTags(cfg.ExcludeTags)

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
	if cfg.ExcludeUntrusted != nil {
		r.ExcludeUntrusted = *cfg.ExcludeUntrusted
	}
	if cfg.MultiUser != nil {
		r.MultiUser = *cfg.MultiUser
	}
	if cfg.ReinforceOnRead != nil {
		r.ReinforceOnRead = *cfg.ReinforceOnRead
	}
	// LTP overrides
	if cfg.LTPThreshold != nil {
		v := *cfg.LTPThreshold
		if v < 0 {
			v = 0
		}
		r.LTPThreshold = v
	}
	if cfg.LTPWeightFloor != nil {
		v := float32(*cfg.LTPWeightFloor)
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		r.LTPWeightFloor = v
	}
	return r
}

// resolveExcludeTags normalizes a per-vault exclude-tags list: blank entries
// are dropped and duplicates collapsed, preserving first-seen order. Returns
// nil for a nil, empty, or all-blank input so "no config" and "empty config"
// resolve identically (the default-empty / opt-in invariant, #713).
func resolveExcludeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	var out []string
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
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
