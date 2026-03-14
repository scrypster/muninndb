package muninn

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Remember stores a new memory in the given vault and returns its ID.
// concept is a short label (e.g. "Go tips"); content is the full text.
func (db *DB) Remember(ctx context.Context, vault, concept, content string) (string, error) {
	resp, err := db.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: concept,
		Content: content,
	})
	if err != nil {
		return "", fmt.Errorf("muninndb remember: %w", err)
	}
	return resp.ID, nil
}

// Forget permanently deletes the engram with the given ID from vault.
// Returns [ErrNotFound] if no such engram exists.
func (db *DB) Forget(ctx context.Context, vault, id string) error {
	_, err := db.eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: vault,
		ID:    id,
		Hard:  true,
	})
	if err != nil {
		if isNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("muninndb forget: %w", err)
	}
	return nil
}
