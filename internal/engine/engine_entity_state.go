package engine

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// SetEntityState sets the lifecycle state of a named entity, and optionally
// corrects its type. For state="merged", mergedInto must be the canonical name.
// entityType may be empty — when empty the existing type is preserved.
func (e *Engine) SetEntityState(ctx context.Context, entityName, state, mergedInto, entityType string) error {
	if entityName == "" {
		return fmt.Errorf("set_entity_state: entity_name is required")
	}

	// Get existing to preserve other fields.
	existing, err := e.store.GetEntityRecord(ctx, entityName)
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

	return e.store.UpsertEntityRecord(ctx, record, "mcp:entity_state")
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
func (e *Engine) SetEntityStateBatch(ctx context.Context, ops []EntityStateOp) []error {
	errs := make([]error, len(ops))
	for i, op := range ops {
		if ctx.Err() != nil {
			errs[i] = ctx.Err()
			continue
		}
		errs[i] = e.SetEntityState(ctx, op.EntityName, op.State, op.MergedInto, op.EntityType)
	}
	return errs
}
