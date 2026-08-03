package engine

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// SetEntityState sets the lifecycle state of a named entity IN vault, and
// optionally corrects its type. For state="merged", mergedInto must be the
// canonical name. entityType may be empty — when empty the existing type is
// preserved.
//
// #683: this took no vault and wrote the global 0x1F record, so an agent
// authenticated for one vault could deprecate or merge another vault's entity.
// The record is vault-scoped now and so is this call.
func (e *Engine) SetEntityState(ctx context.Context, vault, entityName, state, mergedInto, entityType string) error {
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	if entityName == "" {
		return fmt.Errorf("set_entity_state: entity_name is required")
	}

	ws := e.store.ResolveVaultPrefix(vault)

	// Get existing to preserve other fields.
	existing, err := e.store.GetEntityRecord(ctx, ws, entityName)
	if err != nil {
		return fmt.Errorf("set_entity_state: read entity: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("set_entity_state: entity %q not found", entityName)
	}

	// Use provided type; fall back to existing when caller omits it.
	resolvedType := existing.Type
	if entityType != "" {
		resolvedType = entityType
	}

	// Build updated record — UpsertEntityRecord will validate state and MergedInto consistency.
	record := storage.EntityRecord{
		Name:       entityName,
		State:      state,
		MergedInto: mergedInto,
		Type:       resolvedType,
		Confidence: existing.Confidence,
	}

	return e.store.UpsertEntityRecord(ctx, ws, record, "mcp:entity_state")
}

// EntityStateOp is a single operation in a SetEntityStateBatch call.
type EntityStateOp struct {
	EntityName string
	State      string
	MergedInto string
	EntityType string
}

// SetEntityStateBatch applies multiple entity state updates sequentially.
// Returns one error per operation (nil = success). Never returns a top-level error —
// partial success is preserved. Respects context cancellation between items.
func (e *Engine) SetEntityStateBatch(ctx context.Context, vault string, ops []EntityStateOp) []error {
	if err := e.refuseWrite(ctx); err != nil {
		out := make([]error, len(ops))
		for i := range out {
			out[i] = err
		}
		return out
	}
	errs := make([]error, len(ops))
	for i, op := range ops {
		if ctx.Err() != nil {
			errs[i] = ctx.Err()
			continue
		}
		errs[i] = e.SetEntityState(ctx, vault, op.EntityName, op.State, op.MergedInto, op.EntityType)
	}
	return errs
}
