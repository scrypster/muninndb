package consolidation

import (
	"context"
	"log/slog"
	"math"

	"github.com/scrypster/muninndb/internal/storage"
)

// runPhase5TransitiveInference infers new transitive associations A→C where no direct
// association exists, but both A→B and B→C exist with high weights.
// An inference is created if:
// - max(weight(A→B), peakWeight(A→B)) >= 0.7 AND max(weight(B→C), peakWeight(B→C)) >= 0.7
// - No direct A→C association exists
// PeakWeight is used as a fallback threshold: a decayed-but-once-strong edge still
// participates in transitive inference. Inferred weight uses current Weight (not Peak)
// to avoid inflating newly-created edges.
// Inferred weight = weight(A→B) * weight(B→C) * 0.8 (confidence discount)
func (w *Worker) runPhase5TransitiveInference(ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte, report *ConsolidationReport) error {
	const minWeight = 0.7
	const confidenceDiscount = 0.8

	allIDs, err := scanAllEngramIDs(ctx, store, wsPrefix)
	if err != nil {
		return err
	}

	if len(allIDs) == 0 {
		return nil
	}

	// Get all associations
	allAssocs, err := store.GetAssociations(ctx, wsPrefix, allIDs, 100)
	if err != nil {
		return err
	}

	inferred := 0

	// For each A, check all A→B edges
	for a, aAssocs := range allAssocs {
		for _, ab := range aAssocs {
			// Use PeakWeight as fallback: a decayed-but-once-strong edge still participates
			// in transitive inference. Inferred weight uses current Weight, not Peak,
			// to avoid inflating newly-created edges.
			effectiveAB := ab.Weight
			if ab.PeakWeight > effectiveAB {
				effectiveAB = ab.PeakWeight
			}
			if effectiveAB < minWeight {
				continue // A→B never reached threshold, even at peak
			}
			// Skip NaN/Inf input weights to prevent graph corruption
			if math.IsNaN(float64(ab.Weight)) || math.IsInf(float64(ab.Weight), 0) {
				continue
			}

			b := ab.TargetID

			// For each B, check all B→C edges
			bAssocs, hasB := allAssocs[b]
			if !hasB {
				continue // B has no outgoing associations
			}

			for _, bc := range bAssocs {
				effectiveBC := bc.Weight
				if bc.PeakWeight > effectiveBC {
					effectiveBC = bc.PeakWeight
				}
				if effectiveBC < minWeight {
					continue // B→C never reached threshold, even at peak
				}
				// Skip NaN/Inf input weights to prevent graph corruption
				if math.IsNaN(float64(bc.Weight)) || math.IsInf(float64(bc.Weight), 0) {
					continue
				}

				c := bc.TargetID

				// Check if A→C already exists
				existingWeight, err := store.GetAssocWeight(ctx, wsPrefix, a, c)
				if err != nil {
					slog.Warn("consolidation phase 5: failed to check existing weight", "a", a, "c", c, "error", err)
					continue
				}

				if existingWeight > 0 {
					continue // A→C already exists
				}

				// Cap at MaxTransitive per run
				if inferred >= w.MaxTransitive {
					slog.Debug("consolidation phase 5: reached max transitive limit", "limit", w.MaxTransitive)
					goto phase5Done
				}

				// Infer new A→C edge
				inferredWeight := ab.Weight * bc.Weight * confidenceDiscount
				if math.IsNaN(float64(inferredWeight)) || math.IsInf(float64(inferredWeight), 0) {
					continue
				}

				if !w.DryRun {
					if err := store.UpdateAssocWeight(ctx, wsPrefix, a, c, inferredWeight); err != nil {
						slog.Warn("consolidation phase 5: failed to infer association", "a", a, "c", c, "error", err)
						continue
					}
				}

				inferred++
			}
		}
	}

phase5Done:
	report.InferredEdges = inferred
	slog.Debug("consolidation phase 5 (transitive inference) completed", "inferred", inferred)

	return nil
}
