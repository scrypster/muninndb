package muninn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Recall returns up to limit engrams from vault that are relevant to query.
// When limit is 0 the server default (10) is used.
// Results are ordered by descending relevance score.
func (db *DB) Recall(ctx context.Context, vault, query string, limit int) ([]Engram, error) {
	resp, err := db.eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{query},
		MaxResults: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("muninndb recall: %w", err)
	}
	out := make([]Engram, len(resp.Activations))
	for i, a := range resp.Activations {
		out[i] = Engram{
			ID:         a.ID,
			Concept:    a.Concept,
			Content:    a.Content,
			Summary:    a.Summary,
			Score:      float64(a.Score),
			Confidence: a.Confidence,
			CreatedAt:  time.Unix(0, a.CreatedAt),
			LastAccess: time.Unix(0, a.LastAccess),
		}
	}
	return out, nil
}

// Read retrieves the engram with the given ID from vault.
// Returns [ErrNotFound] if no such engram exists.
func (db *DB) Read(ctx context.Context, vault, id string) (*Engram, error) {
	resp, err := db.eng.Read(ctx, &mbp.ReadRequest{
		Vault: vault,
		ID:    id,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("muninndb read: %w", err)
	}
	e := &Engram{
		ID:         resp.ID,
		Concept:    resp.Concept,
		Content:    resp.Content,
		Summary:    resp.Summary,
		State:      lifecycleStateName(resp.State),
		Confidence: resp.Confidence,
		Tags:       resp.Tags,
		CreatedAt:  time.Unix(0, resp.CreatedAt),
		LastAccess: time.Unix(0, resp.LastAccess),
	}
	return e, nil
}

// isNotFound reports whether err indicates a missing engram.
func isNotFound(err error) bool {
	return errors.Is(err, engine.ErrEngramNotFound)
}

// lifecycleStateName converts a raw state byte to a human-readable name.
// Delegates to storage.LifecycleState.String() — the single source of truth.
func lifecycleStateName(state uint8) string {
	return storage.LifecycleState(state).String()
}
