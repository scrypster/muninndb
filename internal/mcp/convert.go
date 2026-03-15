package mcp

import (
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const contentPreviewLen = 500

// activationToMemory converts an mbp.ActivationItem to an MCP Memory for recall responses.
// Prefers Summary when available; falls back to a content preview (500 chars) so that
// recall returns discovery-oriented data rather than a raw content slice.
func activationToMemory(item *mbp.ActivationItem) Memory {
	// Use the enrichment-generated summary when present; otherwise preview content.
	displayContent := item.Summary
	if displayContent == "" {
		displayContent = item.Content
		if len(displayContent) > contentPreviewLen {
			displayContent = displayContent[:contentPreviewLen] + "..."
		}
	}
	return Memory{
		ID:          item.ID,
		Concept:     item.Concept,
		Content:     displayContent,
		Summary:     item.Summary,
		Score:       float64(item.Score),
		VectorScore: float64(item.ScoreComponents.SemanticSimilarity),
		Confidence:  item.Confidence,
		Why:         item.Why,
		CreatedAt:   time.Unix(0, item.CreatedAt).UTC(),
		LastAccess:  time.Unix(0, item.LastAccess).UTC(),
		AccessCount: item.AccessCount,
		Relevance:   item.Relevance,
		SourceType:  item.SourceType,
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
