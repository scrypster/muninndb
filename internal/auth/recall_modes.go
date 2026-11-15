package auth

import (
	"fmt"
	"sort"
)

// RecallModePreset is a bundle of recall parameters for common retrieval patterns.
// Zero-value fields mean "do not override caller defaults".
type RecallModePreset struct {
	MaxHops            int
	Threshold          float32
	SemanticSimilarity float32
	FullTextRelevance  float32
	Recency            float32
	DisableACTR        bool
}

// recallModePresets are the canonical recall mode definitions, shared by MCP and REST handlers.
var recallModePresets = map[string]RecallModePreset{
	"semantic": {
		SemanticSimilarity: 0.8,
		FullTextRelevance:  0.2,
		MaxHops:            0,
		Threshold:          0.3,
		DisableACTR:        true,
	},
	// "recent" is an ALIAS of "balanced": no fields, so it scores exactly
	// like the engine defaults on every fusion mode (pinned by the engine's
	// TestRecallModeCensus R5/R5b). Sent on the wire it replaces the vault's
	// default recall mode, like every mode does — so it is NOT "the vault
	// default" on a vault whose default is semantic or deep. It declares session-orientation INTENT only —
	// on MCP it attaches the COG-29 unresolved-contradiction readout, keyed
	// on the wire string. It never listed recent memories: its old {Recency
	// .7, Sem .3, MaxHops 1, Threshold .2} built a vector whose Recency the
	// ACT-R formula never reads, raised the bar, and flipped weighted_sum
	// vaults to ACT-R, so topical recalls came back empty. See COG-6.
	"recent":   {},
	"balanced": {}, // zero value = engine defaults
	"deep": {
		MaxHops:   4,
		Threshold: 0.1,
	},
}

// LookupRecallMode returns the RecallModePreset for the given name.
// Returns an error for unknown mode names.
func LookupRecallMode(name string) (RecallModePreset, error) {
	m, ok := recallModePresets[name]
	if !ok {
		return RecallModePreset{}, fmt.Errorf("unknown recall mode %q: valid modes are semantic, recent, balanced, deep", name)
	}
	return m, nil
}

// RecallModeNames returns every defined recall-mode name, sorted. It exists
// so invariant tests (the engine's recall-mode census) enumerate the presets
// from the map itself rather than from a hand-maintained list that a new
// preset could silently escape.
func RecallModeNames() []string {
	names := make([]string, 0, len(recallModePresets))
	for n := range recallModePresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
