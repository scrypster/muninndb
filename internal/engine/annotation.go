package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
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

// GetAnnotations returns annotation metadata for an engram by string ULID,
// gated by the caller's own visibility contract (#700).
//
// req carries the same view-defining fields the caller's Activate call used
// (AsOf/IncludeInvalid/CallerOwner/IncludeLeased/Filters) — pass the SAME
// *mbp.ActivateRequest the recall handler built, not a fresh one, or the gate
// answers for the wrong view. req may be nil; that resolves to the zero value
// (no as_of, no leased-visibility opt-in, no filters), which is the safe,
// restrictive default — never more permissive than an explicit request.
//
// BEFORE this, SupersededBy/ConflictsWith were filled from a raw reverse-edge
// lookup with no state check, no lease visibility, no trust/meta/structured
// filters, and no as-of view: a lease-hidden successor's ULID, a soft-deleted
// superseder's ID (deletion-means-deletion broken in metadata), or a
// view-future head's ID under as_of could all leak through this fallback even
// though the ranking phase's own SupersededBy/CurrentVersion annotation is
// gated by the identical visibilityGate.Nameable (COG-22). Every candidate
// this function names now clears that same gate; a candidate the gate refuses
// is silently omitted from the annotation (as if the edge did not exist), not
// an error — matching how the ranking phase's own gate failures behave.
//
// Returns a non-nil *AnnotationData with zero-value fields when the engram has
// no associations or provenance (normal case). Returns error only on storage failure.
func (e *Engine) GetAnnotations(ctx context.Context, vault, id string, req *mbp.ActivateRequest) (*AnnotationData, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	rawID, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	gate := e.annotationVisibilityGate(vault, req)
	nameable := func(target storage.ULID) bool {
		eng, gerr := e.store.GetEngram(ctx, ws, target)
		if gerr != nil || eng == nil {
			return false
		}
		return gate.Nameable(ctx, e.store, ws, eng)
	}

	// Forward associations: engrams THIS one contradicts (RelContradicts).
	forward, err := e.store.GetAssociations(ctx, ws, []storage.ULID{rawID}, 100)
	if err != nil {
		return nil, fmt.Errorf("get associations: %w", err)
	}
	var conflictsWith []string
	for _, a := range forward[rawID] {
		if a.RelType == storage.RelContradicts && nameable(a.TargetID) {
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
		if a.RelType == storage.RelSupersedes && nameable(a.TargetID) {
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

// annotationVisibilityGate builds the same visibilityGate the ranking phase
// uses (COG-22), from the caller's own request rather than a fresh one, so
// GetAnnotations answers for the exact view the caller queried under. Mirrors
// activateCore's ExcludeUntrusted resolution (the one Nameable predicate the
// wire request itself cannot carry — it is a per-vault plasticity setting).
func (e *Engine) annotationVisibilityGate(vault string, req *mbp.ActivateRequest) *visibilityGate {
	actReq := &activation.ActivateRequest{}
	now := time.Now()
	if req != nil {
		actReq.AsOf = req.AsOf
		actReq.IncludeInvalid = req.IncludeInvalid
		actReq.CallerOwner = req.CallerOwner
		actReq.IncludeLeased = req.IncludeLeased
		if len(req.Filters) > 0 {
			actReq.Filters = make([]activation.Filter, len(req.Filters))
			for i, f := range req.Filters {
				actReq.Filters[i] = activation.Filter{Field: f.Field, Op: f.Op, Value: f.Value}
			}
		}
	}
	if e.authStore != nil {
		if vaultCfg, err := e.authStore.GetVaultConfig(vault); err == nil {
			actReq.ExcludeUntrusted = auth.ResolvePlasticity(vaultCfg.Plasticity).ExcludeUntrusted
		}
	}
	return newVisibilityGate(actReq, now)
}
