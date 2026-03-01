package enrich

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// EnrichmentPipeline orchestrates the LLM calls per engram.
// In full mode (default) it runs up to 4 calls: entity extraction,
// relationship extraction, classification, and summarization.
// In light mode it runs only summarization (1 call).
// Individual stages can be disabled via per-stage flags in the config.
type EnrichmentPipeline struct {
	provider LLMProvider
	prompts  *Prompts
	limiter  *TokenBucketLimiter
	cfg      *config.PluginConfig
}

// NewPipeline creates a new enrichment pipeline.
// cfg may be nil, in which case all stages are enabled in full mode.
func NewPipeline(provider LLMProvider, limiter *TokenBucketLimiter) *EnrichmentPipeline {
	return &EnrichmentPipeline{
		provider: provider,
		prompts:  DefaultPrompts(),
		limiter:  limiter,
	}
}

// SetConfig applies server-level enrichment configuration (per-stage flags, mode).
func (p *EnrichmentPipeline) SetConfig(cfg *config.PluginConfig) {
	p.cfg = cfg
}

// stageEnabled returns whether a named stage is enabled given config and light-mode rules.
func (p *EnrichmentPipeline) stageEnabled(stage string) bool {
	if p.cfg == nil {
		return true
	}
	if p.cfg.IsLightMode() {
		return stage == "summary"
	}
	return p.cfg.EnrichStageEnabled(stage)
}

// Run executes the enrichment pipeline for one engram.
// The engram's existing fields are checked: if a stage's output is already
// present (caller-provided via inline enrichment), that stage is skipped.
// Returns nil, nil if all calls fail (graceful degradation).
// Returns error only if the entire pipeline is completely unavailable.
func (p *EnrichmentPipeline) Run(ctx context.Context, eng *storage.Engram) (result *plugin.EnrichmentResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("enrich pipeline panic: %v", r)
			slog.Error("enrich: panic recovered", "panic", r)
		}
	}()

	result = &plugin.EnrichmentResult{}

	// Call 1: Entity extraction
	var entities []plugin.ExtractedEntity
	if p.stageEnabled("entities") && !engramHasEntities(eng) {
		ents, err := p.extractEntities(ctx, eng)
		if err != nil {
			slog.Warn("enrich: entity extraction failed", "id", eng.ID.String(), "err", err)
			ents = nil
		}
		entities = ents
		result.Entities = entities
	}

	// Call 2: Relationship extraction (only if we have entities and stage enabled)
	if p.stageEnabled("relationships") && len(entities) > 0 {
		rels, err := p.extractRelationships(ctx, eng, entities)
		if err != nil {
			slog.Warn("enrich: relationship extraction failed", "id", eng.ID.String(), "err", err)
			rels = nil
		}
		result.Relationships = rels
	}

	// Call 3: Classification
	if p.stageEnabled("classification") && !engramHasClassification(eng) {
		memType, typeLabel, category, subcategory, tags, err := p.classify(ctx, eng)
		if err != nil {
			slog.Warn("enrich: classification failed", "id", eng.ID.String(), "err", err)
		} else {
			mt, typeStr := resolveClassification(memType, typeLabel)
			result.MemoryType = typeStr
			_ = mt
			if category != "" && subcategory != "" {
				result.Classification = category + "/" + subcategory
			}
			_ = tags
		}
	}

	// Call 4: Summarization
	if p.stageEnabled("summary") && !engramHasSummary(eng) {
		summary, keyPoints, err := p.summarize(ctx, eng)
		if err != nil {
			slog.Warn("enrich: summarization failed", "id", eng.ID.String(), "err", err)
		} else {
			result.Summary = summary
			result.KeyPoints = keyPoints
		}
	}

	// If ALL stages produced nothing, return error so retry can be attempted
	if result.Summary == "" && len(result.Entities) == 0 &&
		result.MemoryType == "" && result.Classification == "" {
		return nil, fmt.Errorf("enrich: all pipeline stages failed for engram %s", eng.ID.String())
	}

	return result, nil
}

// engramHasEntities returns true if the caller explicitly provided key points as a proxy
// for pre-extracted entities on this engram. This is only used for inline enrichment skipping;
// the retroactive processor uses GetDigestFlags instead.
// NOTE: This check is a temporary heuristic — Task 2 will wire DigestEntities flag properly.
func engramHasEntities(eng *storage.Engram) bool {
	// For now, only skip if both KeyPoints AND Summary are present (caller provided full enrichment).
	// This is conservative: better to re-extract than to silently skip.
	return len(eng.KeyPoints) > 0 && eng.Summary != ""
}

// engramHasSummary returns true if the engram already has a caller-provided summary.
func engramHasSummary(eng *storage.Engram) bool {
	return eng.Summary != ""
}

// engramHasClassification returns true if the engram already has a non-default MemoryType
// set by the caller (anything beyond the zero-value TypeFact with a TypeLabel).
func engramHasClassification(eng *storage.Engram) bool {
	return eng.MemoryType != storage.TypeFact || eng.TypeLabel != ""
}

// memoryTypeNames maps LLM classification output strings to storage.MemoryType values.
var memoryTypeNames = map[string]storage.MemoryType{
	"fact":        storage.TypeFact,
	"decision":    storage.TypeDecision,
	"observation": storage.TypeObservation,
	"preference":  storage.TypePreference,
	"issue":       storage.TypeIssue,
	"bugfix":      storage.TypeIssue,
	"bug_report":  storage.TypeIssue,
	"task":        storage.TypeTask,
	"procedure":   storage.TypeProcedure,
	"event":       storage.TypeEvent,
	"experience":  storage.TypeEvent,
	"goal":        storage.TypeGoal,
	"constraint":  storage.TypeConstraint,
	"identity":    storage.TypeIdentity,
	"reference":   storage.TypeReference,
}

// resolveClassification maps the LLM's memory_type and type_label strings to
// the storage.MemoryType enum and a display string. The display string prefers
// type_label if present, otherwise falls back to the canonical enum name.
func resolveClassification(memType, typeLabel string) (storage.MemoryType, string) {
	mt, ok := memoryTypeNames[memType]
	if !ok {
		mt = storage.TypeFact
	}
	if typeLabel != "" {
		return mt, typeLabel
	}
	return mt, memType
}

// extractEntities executes Call 1: entity extraction.
func (p *EnrichmentPipeline) extractEntities(ctx context.Context, eng *storage.Engram) ([]plugin.ExtractedEntity, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	userMsg := fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)
	resp, err := p.provider.Complete(ctx, p.prompts.EntitiesSystem, userMsg)
	if err != nil {
		return nil, err
	}

	return ParseEntityResponse(resp)
}

// extractRelationships executes Call 2: relationship extraction.
func (p *EnrichmentPipeline) extractRelationships(ctx context.Context, eng *storage.Engram, entities []plugin.ExtractedEntity) ([]plugin.ExtractedRelation, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Build entities JSON for the prompt
	entitiesJSON := "["
	for i, e := range entities {
		if i > 0 {
			entitiesJSON += ", "
		}
		entitiesJSON += fmt.Sprintf(`{"name": %q, "type": %q, "confidence": %.2f}`, e.Name, e.Type, e.Confidence)
	}
	entitiesJSON += "]"

	userMsg := fmt.Sprintf("Entities: %s\n\nConcept: %s\n\nContent: %s",
		entitiesJSON, eng.Concept, eng.Content)
	resp, err := p.provider.Complete(ctx, p.prompts.RelationshipsSystem, userMsg)
	if err != nil {
		return nil, err
	}

	return ParseRelationshipResponse(resp)
}

// classify executes Call 3: classification.
func (p *EnrichmentPipeline) classify(ctx context.Context, eng *storage.Engram) (memType, typeLabel, category, subcategory string, tags []string, err error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return "", "", "", "", nil, err
	}

	userMsg := fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)
	resp, err := p.provider.Complete(ctx, p.prompts.ClassifySystem, userMsg)
	if err != nil {
		return "", "", "", "", nil, err
	}

	return ParseClassificationResponse(resp)
}

// summarize executes Call 4: summarization.
func (p *EnrichmentPipeline) summarize(ctx context.Context, eng *storage.Engram) (summary string, keyPoints []string, err error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return "", nil, err
	}

	userMsg := fmt.Sprintf("Concept: %s\n\nContent: %s", eng.Concept, eng.Content)
	resp, err := p.provider.Complete(ctx, p.prompts.SummarizeSystem, userMsg)
	if err != nil {
		return "", nil, err
	}

	return ParseSummarizeResponse(resp)
}
