package mcp

import (
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

const contentMaxLen = 500

// activationToMemory converts an mbp.ActivationItem to an MCP Memory for recall responses.
// Truncates Content to contentMaxLen chars if over limit.
func activationToMemory(item *mbp.ActivationItem) Memory {
	content := item.Content
	if len(content) > contentMaxLen {
		content = content[:contentMaxLen] + "..."
	}
	return Memory{
		ID:          item.ID,
		Concept:     item.Concept,
		Content:     content,
		Score:       float64(item.Score),
		VectorScore: float64(item.ScoreComponents.SemanticSimilarity),
		Confidence:  item.Confidence,
		Why:         item.Why,
		LastAccess:  time.Unix(0, item.LastAccess).UTC(),
		AccessCount: item.AccessCount,
		Relevance:   item.Relevance,
		SourceType:  item.SourceType,
	}
}

// readResponseToMemory converts a ReadResponse to a Memory for the muninn_read tool.
func readResponseToMemory(r *mbp.ReadResponse) Memory {
	content := r.Content
	if len(content) > contentMaxLen {
		content = content[:contentMaxLen] + "..."
	}
	return Memory{
		ID:         r.ID,
		Concept:    r.Concept,
		Content:    content,
		Confidence: r.Confidence,
		Tags:       r.Tags,
		State:       fmt.Sprintf("%d", r.State),
		CreatedAt:   time.Unix(0, r.CreatedAt), // r.CreatedAt is nanoseconds since epoch
		LastAccess:  time.Unix(0, int64(r.LastAccess)).UTC(),
		AccessCount: r.AccessCount,
		Relevance:   r.Relevance,
	}
}

// textContent wraps a string in the MCP tools/call result envelope.
func textContent(s string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": s}},
	}
}
