package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// AnnotationData is the raw annotation result for a single engram.
// Staleness is NOT included here — it is computed from ActivationItem.LastAccess
// in the MCP handler (buildAnnotations in handlers.go).
type AnnotationData struct {
	// ConflictsWith holds ULIDs of engrams this one contradicts
	// (forward RelContradicts associations from this engram).
	ConflictsWith []string

	// SupersededBy is the ULID of the engram that supersedes this one.
	// Empty string means no supersession exists.
	// Populated from reverse RelSupersedes edges (another engram points TO this one with RelSupersedes).
	SupersededBy string

	// LastVerified is the timestamp of the last provenance entry for this engram.
	// nil when no provenance entries exist.
	LastVerified *time.Time
}

// GetAnnotations returns annotation metadata for an engram by string ULID.
// Returns a non-nil *AnnotationData with zero-value fields when the engram has
// no associations or provenance (normal case). Returns error only on storage failure.
func (e *Engine) GetAnnotations(ctx context.Context, vault, id string) (*AnnotationData, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	rawID, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	// Forward associations: engrams THIS one contradicts (RelContradicts).
	forward, err := e.store.GetAssociations(ctx, ws, []storage.ULID{rawID}, 100)
	if err != nil {
		return nil, fmt.Errorf("get associations: %w", err)
	}
	var conflictsWith []string
	for _, a := range forward[rawID] {
		if a.RelType == storage.RelContradicts {
			conflictsWith = append(conflictsWith, a.TargetID.String())
		}
	}

	// Reverse associations: engrams that supersede THIS one (RelSupersedes pointing TO this engram).
	reverse, err := e.store.GetReverseAssociations(ctx, ws, rawID, 10)
	if err != nil {
		return nil, fmt.Errorf("get reverse associations: %w", err)
	}
	var supersededBy string
	for _, a := range reverse {
		if a.RelType == storage.RelSupersedes {
			supersededBy = a.TargetID.String() // TargetID = the source of the reverse edge (the superseding engram)
			break
		}
	}

	// Last provenance timestamp.
	entries, err := e.prov.Get(ctx, ws, [16]byte(rawID))
	var lastVerified *time.Time
	if err == nil && len(entries) > 0 {
		t := entries[len(entries)-1].Timestamp
		lastVerified = &t
	}
	// provenance errors are non-fatal: annotations are best-effort

	return &AnnotationData{
		ConflictsWith: conflictsWith,
		SupersededBy:  supersededBy,
		LastVerified:  lastVerified,
	}, nil
}
