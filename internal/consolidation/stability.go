package consolidation

import (
	"context"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

const defaultStabilityFloor float32 = 14.0 // days — minimum stability

// runPhase4BidirectionalStability adjusts engram stability based on access signal:
//   - High-signal: AccessCount >= 5 AND Relevance >= 0.7 → Stability *= 1.2
//   - Low-signal:  AccessCount < 2 AND age > 30d AND Relevance < 0.3 → Stability = max(Stability * 0.8, floor)
//
// Archived engrams and legal vaults are exempt. Respects w.DryRun.
func (w *Worker) runPhase4BidirectionalStability(ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte, report *ConsolidationReport, vault string) error {
	// Legal vaults are exempt.
	if isLegalVault(vault) {
		return nil
	}

	allIDs, err := scanAllEngramIDs(ctx, store, wsPrefix)
	if err != nil {
		return err
	}

	if len(allIDs) == 0 {
		return nil
	}

	allEngrams, err := store.GetEngrams(ctx, wsPrefix, allIDs)
	if err != nil {
		return err
	}

	const (
		highAccessThreshold = 5
		highRelevanceMin    = float32(0.7)
		lowAccessThreshold  = 2
		lowRelevanceMax     = float32(0.3)
	)
	ageThreshold := 30 * 24 * time.Hour
	now := time.Now()

	var strengthened, weakened int

	for _, eng := range allEngrams {
		if eng == nil {
			continue
		}

		// Skip archived engrams.
		if eng.State == storage.StateArchived {
			continue
		}

		var newStability float32
		var changed bool

		if eng.AccessCount >= highAccessThreshold && eng.Relevance >= highRelevanceMin {
			// High-signal: strengthen stability.
			newStability = eng.Stability * 1.2
			changed = true
			strengthened++
		} else if eng.AccessCount < lowAccessThreshold && now.Sub(eng.CreatedAt) > ageThreshold && eng.Relevance < lowRelevanceMax {
			// Low-signal: weaken stability, respecting the floor.
			candidate := eng.Stability * 0.8
			if candidate < defaultStabilityFloor {
				candidate = defaultStabilityFloor
			}
			newStability = candidate
			changed = true
			weakened++
		}

		if !changed {
			continue
		}

		if !w.DryRun {
			if err := store.UpdateRelevance(ctx, wsPrefix, eng.ID, eng.Relevance, newStability); err != nil {
				slog.Warn("dream phase4 (stability): failed to update engram", "id", eng.ID, "error", err)
				// Undo count increment on write failure.
				if eng.AccessCount >= highAccessThreshold && eng.Relevance >= highRelevanceMin {
					strengthened--
				} else {
					weakened--
				}
				continue
			}
		}
	}

	report.StabilityStrengthened += strengthened
	report.StabilityWeakened += weakened

	slog.Debug("dream phase4 (bidirectional stability) completed",
		"vault", vault,
		"strengthened", strengthened,
		"weakened", weakened,
	)

	return nil
}
