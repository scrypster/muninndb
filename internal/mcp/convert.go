package mcp

import (
	"math"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const contentPreviewLen = 500

// roundScore widens a float32 score to float64 while stripping float32 quantization
// noise (e.g. float64(float32(1.15)) = 1.149999976...). Rounding to 6 decimals keeps
// meaningful precision while serializing clean values. See #502.
func roundScore(f float32) float64 {
	return math.Round(float64(f)*1e6) / 1e6
}

// activationToMemory converts an mbp.ActivationItem to an MCP Memory for recall responses.
// Summary-first by design (#112): Summary carries the enrichment summary, while Content
// carries the real engram content (truncated to a preview). The summary is never copied
// into Content, so the same string is not serialized twice (#502).
func activationToMemory(item *mbp.ActivationItem) Memory {
	// Content is the real engram content, truncated to a preview length. The summary
	// stays in its own field; recall never overwrites Content with Summary.
	previewContent := item.Content
	if len(previewContent) > contentPreviewLen {
		previewContent = previewContent[:contentPreviewLen] + "..."
	}
	// Supersession annotation is ALWAYS surfaced (no annotate flag) so an agent is
	// never handed a stale fact without being told the current one — the "was 8 in
	// May, now 11" narration comes from the payload alone. The annotate=true path
	// augments this same struct with staleness/conflicts/provenance.
	var annotations *MemoryAnnotations
	// SupersededBy/CurrentVersion (asserted) and PossiblySupersededBy/
	// VersionCluster/NewestOfCluster (heuristic, never an authority) are all
	// always-on; annotate=true only augments this struct further below.
	if item.SupersededBy != "" || item.CurrentVersion != "" ||
		item.PossiblySupersededBy != "" || item.VersionCluster != "" || item.NewestOfCluster ||
		item.SubstitutedFor != "" {
		annotations = &MemoryAnnotations{
			SupersededBy:         item.SupersededBy,
			CurrentVersion:       item.CurrentVersion,
			PossiblySupersededBy: item.PossiblySupersededBy,
			VersionCluster:       item.VersionCluster,
			NewestOfCluster:      item.NewestOfCluster,
			ClusterSize:          item.ClusterSize,
			// COG-28 (#763): asserted substitution provenance. Always-on for
			// the same reason superseded_by is — an agent must never be handed
			// a row admitted by a DIFFERENT memory's match without being told.
			SubstitutedFor:    item.SubstitutedFor,
			ChainTruncated:    item.ChainTruncated,
			HeadNotIndexedYet: item.HeadNotIndexedYet,
		}
		if b := item.SubstitutionBasis; b != nil {
			annotations.SubstitutionBasis = &SubstitutionBasis{
				AbsoluteScore:      roundScore(b.AbsoluteScore),
				ContentMatch:       roundScore(b.ContentMatch),
				SemanticSimilarity: roundScore(b.SemanticSimilarity),
				FullTextRelevance:  roundScore(b.FullTextRelevance),
			}
		}
	}
	m := Memory{
		Annotations:    annotations,
		ID:             item.ID,
		Concept:        item.Concept,
		Content:        previewContent,
		Summary:        item.Summary,
		Score:          roundScore(item.Score),
		VectorScore:    roundScore(item.ScoreComponents.SemanticSimilarity),
		VectorScoreRaw: roundScore(item.ScoreComponents.SemanticSimilarityRaw),
		EntityBoost:    roundScore(item.ScoreComponents.EntityBoost),
		Confidence:     item.Confidence,
		Why:            item.Why,
		// Map the lifecycle state label the same way the read path does (#502).
		State: storage.LifecycleState(item.State).String(),
		// Type mirrors the vocabulary muninn_remember accepts (storage.ParseMemoryType).
		Type:        storage.MemoryType(item.MemoryType).String(),
		TypeLabel:   item.TypeLabel,
		CreatedAt:   time.Unix(0, item.CreatedAt).UTC(),
		LastAccess:  time.Unix(0, item.LastAccess).UTC(),
		AccessCount: item.AccessCount,
		Relevance:   item.Relevance,
		SourceType:  item.SourceType,
		Trust:       storage.TrustLevel(item.Trust).String(),
		Tags:        item.Tags,
		Expired:     item.Expired,
	}
	m.Importance, m.ImportanceSource = importanceFields(item.Importance,
		storage.MemoryType(item.MemoryType), storage.TrustLevel(item.Trust))
	// Valid-time annotations: only present when meaningful (backdated
	// valid_from, or a closed window).
	if item.ValidFrom != 0 {
		vf := time.Unix(0, item.ValidFrom).UTC()
		m.ValidFrom = &vf
	}
	if item.ValidUntil != 0 {
		vu := time.Unix(0, item.ValidUntil).UTC()
		m.ValidUntil = &vu
	}
	return m
}

// readResponseToMemory converts a ReadResponse to a Memory for the muninn_read tool.
// Returns the full content without truncation, and maps Summary when present.
// Entities and EntityRelationships are included when populated by the engine.
func readResponseToMemory(r *mbp.ReadResponse) Memory {
	m := Memory{
		ID:          r.ID,
		Concept:     r.Concept,
		Content:     r.Content, // full content, no truncation
		Summary:     r.Summary,
		Confidence:  r.Confidence,
		Tags:        r.Tags,
		State:       storage.LifecycleState(r.State).String(),
		Type:        storage.MemoryType(r.MemoryType).String(),
		TypeLabel:   r.TypeLabel,
		CreatedAt:   time.Unix(0, r.CreatedAt).UTC(),
		LastAccess:  time.Unix(0, r.LastAccess).UTC(),
		AccessCount: r.AccessCount,
		Relevance:   r.Relevance,
		Trust:       storage.TrustLevel(r.Trust).String(),
	}
	m.Importance, m.ImportanceSource = importanceFields(r.Importance,
		storage.MemoryType(r.MemoryType), storage.TrustLevel(r.Trust))
	// muninn_read always echoes the valid-time axis (teaches the two axes:
	// created_at is transaction time, valid_from/valid_until application time).
	if r.ValidFrom != 0 {
		vf := time.Unix(0, r.ValidFrom).UTC()
		m.ValidFrom = &vf
	}
	if r.ValidUntil != 0 {
		vu := time.Unix(0, r.ValidUntil).UTC()
		m.ValidUntil = &vu
	}
	isCurrent := r.IsCurrent
	m.IsCurrent = &isCurrent
	for _, e := range r.Entities {
		m.Entities = append(m.Entities, ReadEntity{Name: e.Name, Type: e.Type})
	}
	for _, rel := range r.EntityRelationships {
		m.EntityRelationships = append(m.EntityRelationships, ReadEntityRel{
			FromEntity: rel.FromEntity,
			ToEntity:   rel.ToEntity,
			RelType:    rel.RelType,
			Weight:     rel.Weight,
		})
	}
	return m
}

// importanceFields resolves the (importance, importance_source) presentation
// pair from a stored importance plus the memory type and trust the effective
// value derives from when unset. "explicit" = the caller asserted the value
// (stored > 0 — writes quantize explicit 0 to 0.01); "derived" = the use-time
// type-table default (never stored). Mirrors storage.EffectiveImportance.
func importanceFields(stored float32, memType storage.MemoryType, trust storage.TrustLevel) (float64, string) {
	eff := roundScore(storage.EffectiveImportance(stored, memType, trust))
	if storage.ImportanceExplicit(stored) {
		return eff, "explicit"
	}
	return eff, "derived"
}

// textContent wraps a string in the MCP tools/call result envelope.
func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s}},
	}
}
