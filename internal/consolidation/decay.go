package consolidation

import (
	"context"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// runPhase4DecayAcceleration accelerates relevance decay for old, low-access engrams.
// Engrams matching ALL of these criteria are decayed:
// - AccessCount < 2
// - Age > 30 days
// - Relevance < 0.3
// Decay: relevance = relevance * 0.5
func (w *Worker) runPhase4DecayAcceleration(ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte, report *ConsolidationReport) error {
	const maxAccessCount = 2
	const maxRelevance = 0.3
	ageThreshold := 30 * 24 * time.Hour
	now := time.Now()

	allIDs, err := scanAllEngramIDs(ctx, store, wsPrefix)
	if err != nil {
		return err
	}

	if len(allIDs) == 0 {
		return nil
	}

	// Fetch all engrams
	allEngrams, err := store.GetEngrams(ctx, wsPrefix, allIDs)
	if err != nil {
		return err
	}

	var decayed int
	for _, eng := range allEngrams {
		if eng == nil {
			continue
		}

		// Check all three criteria
		if eng.AccessCount >= maxAccessCount {
			continue // has sufficient access
		}

		age := now.Sub(eng.CreatedAt)
		if age < ageThreshold {
			continue // too new
		}

		if eng.Relevance >= maxRelevance {
			continue // too relevant already
		}

		// Apply decay
		newRelevance := eng.Relevance * 0.5

		if !w.DryRun {
			if err := store.UpdateRelevance(ctx, wsPrefix, eng.ID, newRelevance, eng.Stability); err != nil {
				slog.Warn("consolidation phase 4: failed to decay engram", "id", eng.ID, "error", err)
				continue
			}
		}

		decayed++
	}

	report.DecayedEngrams = decayed
	slog.Debug("consolidation phase 4 (decay acceleration) completed", "decayed", decayed)

	return nil
}
