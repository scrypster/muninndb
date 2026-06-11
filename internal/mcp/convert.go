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
	return Memory{
		ID:          item.ID,
		Concept:     item.Concept,
		Content:     previewContent,
		Summary:     item.Summary,
		Score:       roundScore(item.Score),
		VectorScore: roundScore(item.ScoreComponents.SemanticSimilarity),
		Confidence:  item.Confidence,
		Why:         item.Why,
		// Map the lifecycle state label the same way the read path does (#502).
		State:       storage.LifecycleState(item.State).String(),
		CreatedAt:   time.Unix(0, item.CreatedAt).UTC(),
		LastAccess:  time.Unix(0, item.LastAccess).UTC(),
		AccessCount: item.AccessCount,
		Relevance:   item.Relevance,
		SourceType:  item.SourceType,
		Trust:       storage.TrustLevel(item.Trust).String(),
	}
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
		CreatedAt:   time.Unix(0, r.CreatedAt).UTC(),
		LastAccess:  time.Unix(0, r.LastAccess).UTC(),
		AccessCount: r.AccessCount,
		Relevance:   r.Relevance,
		Trust:       storage.TrustLevel(r.Trust).String(),
	}
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

// textContent wraps a string in the MCP tools/call result envelope.
func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s}},
	}
}
