package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/storage"
)

// ReplayEnrichmentResult holds the outcome of a replay enrichment run.
type ReplayEnrichmentResult struct {
	Processed int
	Skipped   int
	Failed    int
	Remaining int
	StagesRun []string
	DryRun    bool
}

// stageToFlag maps a stage name to its DigestFlag bit.
var stageToFlag = map[string]uint8{
	"entities":       plugin.DigestEntities,
	"relationships":  plugin.DigestRelationships,
	"classification": plugin.DigestClassified,
	"summary":        plugin.DigestSummarized,
}

// defaultReplayStages are used when no stages param is provided.
var defaultReplayStages = []string{"entities", "relationships", "classification", "summary"}

// ReplayEnrichment re-runs the enrichment pipeline for active engrams in a
// vault that are missing one or more requested digest stage flags.
//
// Parameters:
//   - vault:  vault name
//   - stages: subset of ["entities","relationships","classification","summary"]
//   - limit:  max engrams to process in this call (1-200)
//   - dryRun: if true, scan only — count what would be processed, no writes
//
// The method requires an EnrichPlugin to be registered via SetEnrichPlugin.
// If no plugin is configured and dryRun is false, an error is returned.
func (e *Engine) ReplayEnrichment(ctx context.Context, vault string, stages []string, limit int, dryRun bool) (*ReplayEnrichmentResult, error) {
	if len(stages) == 0 {
		stages = defaultReplayStages
	}

	// Validate and deduplicate stages; build the flag mask for "needs enrichment".
	stageMask := uint8(0)
	validStages := make([]string, 0, len(stages))
	seen := make(map[string]bool)
	for _, s := range stages {
		if _, ok := stageToFlag[s]; !ok {
			return nil, fmt.Errorf("unknown enrichment stage %q: valid stages are entities, relationships, classification, summary", s)
		}
		if !seen[s] {
			stageMask |= stageToFlag[s]
			validStages = append(validStages, s)
			seen[s] = true
		}
	}

	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	ws := e.store.ResolveVaultPrefix(vault)

	// Collect active engram IDs.
	ids, err := e.store.ListByState(ctx, ws, storage.StateActive, limit)
	if err != nil {
		return nil, fmt.Errorf("replay enrichment: list active engrams: %w", err)
	}

	if len(ids) == 0 {
		return &ReplayEnrichmentResult{
			Processed: 0,
			Skipped:   0,
			Failed:    0,
			Remaining: 0,
			StagesRun: validStages,
			DryRun:    dryRun,
		}, nil
	}

	// Fetch full engram records.
	engrams, err := e.store.GetEngrams(ctx, ws, ids)
	if err != nil {
		return nil, fmt.Errorf("replay enrichment: get engrams: %w", err)
	}

	// On dry run, count how many engrams are missing at least one requested stage.
	if dryRun {
		needed := 0
		skipped := 0
		for _, eng := range engrams {
			if eng == nil {
				skipped++
				continue
			}
			// "not found" means no flags set yet — treat as 0.
			flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(eng.ID))
			if flags&stageMask != stageMask {
				needed++
			} else {
				skipped++
			}
		}
		return &ReplayEnrichmentResult{
			Processed: needed,
			Skipped:   skipped,
			Failed:    0,
			Remaining: 0,
			StagesRun: validStages,
			DryRun:    true,
		}, nil
	}

	// Real run: require an enrich plugin.
	if e.enrichPlugin == nil {
		return nil, fmt.Errorf("enrichment pipeline not configured: no enrich plugin available")
	}

	processed := 0
	skipped := 0
	failed := 0

	for i, eng := range engrams {
		if eng == nil {
			skipped++
			continue
		}

		// Honour context cancellation (deadline, manual cancel) — report remaining work.
		if ctx.Err() != nil {
			return &ReplayEnrichmentResult{
				Processed: processed,
				Skipped:   skipped,
				Failed:    failed,
				Remaining: countNonNilEngrams(engrams[i:]),
				StagesRun: validStages,
				DryRun:    false,
			}, nil
		}

		// Check which stages are already done.
		// "pebble: not found" means no flags written yet — treat as 0 (all stages needed).
		flags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(eng.ID))

		// If all requested stages are already done, skip this engram.
		if flags&stageMask == stageMask {
			skipped++
			continue
		}

		// Run enrichment for this engram.
		result, enrichErr := e.enrichPlugin.Enrich(ctx, eng)
		if enrichErr != nil {
			if errors.Is(enrichErr, enrich.ErrNothingToEnrich) {
				slog.Debug("replay enrichment: nothing to enrich, skipping", "id", eng.ID.String())
				skipped++
				continue
			}
			slog.Warn("replay enrichment: enrich failed, skipping",
				"id", eng.ID.String(), "err", enrichErr)
			failed++
			continue
		}

		// Persist enrichment results (summary, key_points, memory_type, type_label).
		if updateErr := e.store.UpdateDigest(ctx, eng.ID, result.Summary, result.KeyPoints, result.MemoryType, result.TypeLabel); updateErr != nil {
			slog.Warn("replay enrichment: UpdateDigest failed",
				"id", eng.ID.String(), "err", updateErr)
			failed++
			continue
		}

		// Upsert entities if entities stage was requested and not already done.
		if flags&plugin.DigestEntities == 0 && seen["entities"] {
			var linkedNames []string
			for _, entity := range result.Entities {
				record := storage.EntityRecord{
					Name:       entity.Name,
					Type:       entity.Type,
					Confidence: entity.Confidence,
				}
				if upsertErr := e.store.UpsertEntityRecord(ctx, record, "replay:enrich"); upsertErr != nil {
					slog.Warn("replay enrichment: UpsertEntityRecord failed",
						"id", eng.ID.String(), "name", entity.Name, "err", upsertErr)
					continue
				}
				if linkErr := e.store.WriteEntityEngramLink(ctx, ws, eng.ID, entity.Name); linkErr != nil {
					slog.Warn("replay enrichment: WriteEntityEngramLink failed",
						"id", eng.ID.String(), "name", entity.Name, "err", linkErr)
					continue
				}
				linkedNames = append(linkedNames, entity.Name)
			}
			for i := 0; i < len(linkedNames); i++ {
				for j := i + 1; j < len(linkedNames); j++ {
					_ = e.store.IncrementEntityCoOccurrence(ctx, ws, linkedNames[i], linkedNames[j])
				}
			}
			if len(result.Entities) > 0 {
				if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestEntities); setErr != nil {
					slog.Warn("replay enrichment: SetDigestFlag(DigestEntities) failed",
						"id", eng.ID.String(), "err", setErr)
				}
			}
		}

		// Upsert relationships if relationships stage was requested and not already done.
		if flags&plugin.DigestRelationships == 0 && seen["relationships"] {
			for _, rel := range result.Relationships {
				record := storage.RelationshipRecord{
					FromEntity: rel.FromEntity,
					ToEntity:   rel.ToEntity,
					RelType:    rel.RelType,
					Weight:     rel.Weight,
					Source:     "replay:enrich",
				}
				if upsertErr := e.store.UpsertRelationshipRecord(ctx, ws, eng.ID, record); upsertErr != nil {
					slog.Warn("replay enrichment: UpsertRelationshipRecord failed",
						"id", eng.ID.String(), "err", upsertErr)
				}
			}
			if len(result.Relationships) > 0 {
				if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestRelationships); setErr != nil {
					slog.Warn("replay enrichment: SetDigestFlag(DigestRelationships) failed",
						"id", eng.ID.String(), "err", setErr)
				}
			}
		}

		// Set classification flag if requested and produced output.
		if flags&plugin.DigestClassified == 0 && seen["classification"] && result.Classification != "" {
			if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestClassified); setErr != nil {
				slog.Warn("replay enrichment: SetDigestFlag(DigestClassified) failed",
					"id", eng.ID.String(), "err", setErr)
			}
		}

		// Set summarized flag if requested and produced output.
		if flags&plugin.DigestSummarized == 0 && seen["summary"] && result.Summary != "" {
			if setErr := e.store.SetDigestFlag(ctx, plugin.ULID(eng.ID), plugin.DigestSummarized); setErr != nil {
				slog.Warn("replay enrichment: SetDigestFlag(DigestSummarized) failed",
					"id", eng.ID.String(), "err", setErr)
			}
		}

		processed++
	}

	return &ReplayEnrichmentResult{
		Processed: processed,
		Skipped:   skipped,
		Failed:    failed,
		StagesRun: validStages,
		DryRun:    false,
	}, nil
}

// countNonNilEngrams returns the number of non-nil entries in a slice of engram pointers.
func countNonNilEngrams(engrams []*storage.Engram) int {
	n := 0
	for _, eng := range engrams {
		if eng != nil {
			n++
		}
	}
	return n
}

// SetEnrichPlugin registers an EnrichPlugin for use by ReplayEnrichment.
// Must be called before ReplayEnrichment is used (not concurrency-safe after start).
func (e *Engine) SetEnrichPlugin(p plugin.EnrichPlugin) {
	e.enrichPlugin = p
}
