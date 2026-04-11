package muninn

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/consolidation"
)

// DreamOpts configures a dream consolidation pass.
type DreamOpts struct {
	DryRun bool
	Force  bool   // bypass trigger gates
	Scope  string // limit to single vault ("" = all)

	// Providers holds LLM providers for Phase 2b, ordered by preference (optional).
	Providers []consolidation.LLMProvider
}

// DreamReport is the result of a dream consolidation pass.
// NOTE: This is a type alias that exposes internal consolidation types.
// Consider wrapping before API stabilization.
type DreamReport = consolidation.DreamReport

// Dream runs a dream consolidation pass across vaults.
// It uses a lowered dedup threshold (0.85) to surface near-duplicates.
func (db *DB) Dream(ctx context.Context, opts DreamOpts) (*DreamReport, error) {
	w := consolidation.NewWorker(db.eng)

	w.Providers = opts.Providers

	report, err := w.DreamOnce(ctx, consolidation.DreamOpts{
		DryRun: opts.DryRun,
		Force:  opts.Force,
		Scope:  opts.Scope,
	})
	if err != nil {
		return nil, fmt.Errorf("muninndb dream: %w", err)
	}
	return report, nil
}
