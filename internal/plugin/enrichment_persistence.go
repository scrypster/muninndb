package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// PersistEnrichmentResult writes an enrichment result and marks per-stage flags
// only after the corresponding stage data has been stored successfully.
func PersistEnrichmentResult(ctx context.Context, store PluginStore, id ULID, result *EnrichmentResult) error {
	if result == nil {
		return fmt.Errorf("enrich returned nil result")
	}
	if err := store.UpdateDigest(ctx, id, result); err != nil {
		return fmt.Errorf("persist enriched digest: %w", err)
	}

	var stageErrs []string
	if result.Entities != nil {
		if err := persistEntityStage(ctx, store, id, result.Entities); err != nil {
			stageErrs = append(stageErrs, err.Error())
		}
	}

	switch {
	case result.Relationships != nil:
		if err := persistRelationshipStage(ctx, store, id, result.Relationships); err != nil {
			stageErrs = append(stageErrs, err.Error())
		}
	case result.Entities != nil && len(result.Entities) == 0:
		// An entity pass that found nothing also implies there can be no
		// relationship records for this engram.
		if err := store.SetDigestFlag(ctx, id, DigestRelationships); err != nil {
			stageErrs = append(stageErrs, fmt.Sprintf("set DigestRelationships flag: %v", err))
		}
	}

	if len(stageErrs) > 0 {
		return fmt.Errorf("persist enrich graph: %s", strings.Join(stageErrs, "; "))
	}
	return nil
}

func persistEntityStage(ctx context.Context, store PluginStore, id ULID, entities []ExtractedEntity) error {
	var linkedEntityNames []string
	var errs []string

	for _, entity := range entities {
		if err := store.UpsertEntity(ctx, entity); err != nil {
			slog.Warn("enrich: failed to upsert entity", "id", id.String(), "name", entity.Name, "err", err)
			errs = append(errs, fmt.Sprintf("upsert entity %q: %v", entity.Name, err))
			continue
		}
		if err := store.LinkEngramToEntity(ctx, id, entity.Name); err != nil {
			slog.Warn("enrich: failed to link engram to entity", "id", id.String(), "name", entity.Name, "err", err)
			errs = append(errs, fmt.Sprintf("link entity %q: %v", entity.Name, err))
			continue
		}
		linkedEntityNames = append(linkedEntityNames, entity.Name)
	}

	for i := 0; i < len(linkedEntityNames); i++ {
		for j := i + 1; j < len(linkedEntityNames); j++ {
			if err := store.IncrementEntityCoOccurrence(ctx, id, linkedEntityNames[i], linkedEntityNames[j]); err != nil {
				slog.Warn("enrich: failed to increment entity co-occurrence",
					"id", id.String(),
					"name_a", linkedEntityNames[i],
					"name_b", linkedEntityNames[j],
					"err", err)
				errs = append(errs, fmt.Sprintf("increment entity co-occurrence %q/%q: %v", linkedEntityNames[i], linkedEntityNames[j], err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("entities: %s", strings.Join(errs, "; "))
	}
	if err := store.SetDigestFlag(ctx, id, DigestEntities); err != nil {
		return fmt.Errorf("set DigestEntities flag: %w", err)
	}
	return nil
}

func persistRelationshipStage(ctx context.Context, store PluginStore, id ULID, relationships []ExtractedRelation) error {
	var errs []string
	for _, rel := range relationships {
		if err := store.UpsertRelationship(ctx, id, rel); err != nil {
			slog.Warn("enrich: failed to upsert relationship",
				"id", id.String(),
				"from", rel.FromEntity,
				"to", rel.ToEntity,
				"type", rel.RelType,
				"err", err)
			errs = append(errs, fmt.Sprintf("upsert relationship %q->%q (%s): %v", rel.FromEntity, rel.ToEntity, rel.RelType, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("relationships: %s", strings.Join(errs, "; "))
	}
	if err := store.SetDigestFlag(ctx, id, DigestRelationships); err != nil {
		return fmt.Errorf("set DigestRelationships flag: %w", err)
	}
	return nil
}
