package consolidation

import "time"

// ConsolidationReport summarizes the results of a single consolidation pass.
type ConsolidationReport struct {
	Vault          string        // vault name
	StartedAt      time.Time     // when consolidation started
	Duration       time.Duration // wall-clock time elapsed
	DedupClusters  int           // number of deduplication clusters formed
	MergedEngrams  int           // total engrams merged via deduplication
	PromotedNodes  int           // engrams promoted to schema nodes
	DecayedEngrams int           // engrams aged and decayed
	InferredEdges  int           // new transitive associations inferred
	DryRun         bool          // true if no mutations occurred
	Errors         []string      // non-fatal errors encountered per phase

	// Dream-specific fields (populated by DreamOnce, nil/zero for RunOnce)
	Orient       *VaultSummary // Phase 0 vault summary
	LegalSkipped int           // legal engrams skipped in Phase 2b

	// Dream-specific: Phase 4 bidirectional stability
	StabilityStrengthened int // engrams with stability increased
	StabilityWeakened     int // engrams with stability decreased

	// Dream-specific: Phase 2b LLM consolidation
	LLMMerged         int    // engrams merged by LLM recommendation
	LLMContradictions int    // contradictions resolved by LLM
	LLMSuggestions    int    // cross-vault suggestions from LLM
	Journal           string // narrative from LLM (Phase 6)

	// Phase 2 near-duplicate clusters for LLM review (transient, not serialized)
	DedupClustersForLLM []DedupCluster `json:"-"`
}
