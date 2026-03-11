package enrich

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// ErrNothingToEnrich is returned when all pipeline stages are skipped because
// the engram already has inline data (e.g., Summary set by caller during Write).
// This is distinct from a real failure where LLM/network errors caused stages to fail.
// Defined in the plugin package; aliased here for backwards compatibility.
var ErrNothingToEnrich = plugin.ErrNothingToEnrich

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
// Returns an error if every enabled stage either fails or produces no output.
// Partial failures are logged per-stage and still return any successful output.
func (p *EnrichmentPipeline) Run(ctx context.Context, eng *storage.Engram) (result *plugin.EnrichmentResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("enrich pipeline panic: %v", r)
			slog.Error("enrich: panic recovered", "panic", r)
		}
	}()

	result = &plugin.EnrichmentResult{}
	var stageErrors []string

	// Call 1: Entity extraction
	var entities []plugin.ExtractedEntity
	if p.stageEnabled("entities") && !engramHasEntities(eng) {
		ents, err := p.extractEntities(ctx, eng)
		if err != nil {
			slog.Warn("enrich: entity extraction failed", "id", eng.ID.String(), "err", err)
			stageErrors = append(stageErrors, fmt.Sprintf("entities: %v", err))
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
			stageErrors = append(stageErrors, fmt.Sprintf("relationships: %v", err))
			rels = nil
		}
		result.Relationships = rels
	}

	// Call 3: Classification
	if p.stageEnabled("classification") && !engramHasClassification(eng) {
		memType, typeLabel, category, subcategory, tags, err := p.classify(ctx, eng)
		if err != nil {
			slog.Warn("enrich: classification failed", "id", eng.ID.String(), "err", err)
			stageErrors = append(stageErrors, fmt.Sprintf("classification: %v", err))
		} else {
			mt, _ := resolveClassification(memType, typeLabel)
			result.MemoryType = mt.String()
			result.TypeLabel = typeLabel
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
			stageErrors = append(stageErrors, fmt.Sprintf("summary: %v", err))
		} else {
			result.Summary = summary
			result.KeyPoints = keyPoints
		}
	}

	// If ALL stages produced nothing, return error so retry can be attempted
	if result.Summary == "" && len(result.KeyPoints) == 0 &&
		len(result.Entities) == 0 && result.MemoryType == "" &&
		result.TypeLabel == "" && result.Classification == "" {
		if len(stageErrors) > 0 {
			return nil, fmt.Errorf("enrich: all pipeline stages failed for engram %s: %s", eng.ID.String(), strings.Join(stageErrors, "; "))
		}
		return nil, fmt.Errorf("engram %s: %w", eng.ID.String(), ErrNothingToEnrich)
	}

	return result, nil
}

// engramHasEntities returns true if the engram already has caller-provided entities,
// used as a skip-if-present guard in pipeline.Run for inline enrichment only.
// The retroactive processor uses GetDigestFlags (DigestEntities flag) instead of this check.
// This heuristic: only skip if both KeyPoints AND Summary are present, indicating the
// caller provided a fully pre-enriched engram.
func engramHasEntities(eng *storage.Engram) bool {
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
