package activation

import (
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// PassesLifecycle is the lifecycle half of the recall visibility contract:
// whether eng's lifecycle STATE permits it to be named under the caller's
// temporal view. It is shared by phase-6 scoring and the engine's
// post-pipeline visibility gate so the two paths cannot drift apart.
//
// Soft-delete means "no longer current", not "erased". Supersession (evolve,
// and Link with relation=supersedes) demotes the predecessor by soft-deleting
// it AND stamping a closed ValidUntil in the same atomic batch, precisely so
// that the record stays readable on the valid-time axis — Evolve's own comment
// calls the stamp what "re-opens the record for as_of time-travel only", and
// muninn_guide promises agents that an evolved fact is never destroyed because
// as_of still sees it. Applying the lifecycle cut unconditionally, BEFORE and
// INDEPENDENT of the valid-time gate, made evolve's supersession mechanism
// erase its own history: neither as_of nor include_invalid could reach the
// predecessor (proven RED by TestAsOf_ReadsEvolvedPredecessor; the control is
// forget(not_true_since), which stamps WITHOUT soft-deleting and was always
// reachable — so the defect was the lifecycle cut, never the validity axis).
//
// A soft-deleted engram is therefore admitted only when BOTH hold:
//
//   - the caller asked an explicit HISTORICAL question (asOf != nil or
//     includeInvalid). Default recall is unchanged: no soft-deleted engram
//     ever enters it, so "never lead with a fact known to be stale" and the
//     #548-class rule that for existence predicates the ID itself is the
//     secret are both untouched.
//   - the record carries a CLOSED ValidUntil, i.e. it was invalidated on the
//     valid-time axis rather than merely thrown away. That is exactly the
//     supersession signature. A plain soft-delete (muninn_forget without
//     not_true_since) leaves ValidUntil open — it is trash, not history — and
//     stays invisible to recall on every path, reachable only through
//     list_deleted / read / restore as before.
//
// Admission here is necessary, not sufficient: PassesValidity still decides
// whether the record may be named at the caller's instant, so under as_of=T
// only a predecessor whose half-open window actually covers T comes back, and
// under include_invalid it comes back annotated expired.
//
// StateArchived is deliberately NOT relaxed: archival is a storage-tier
// eviction, not a statement about truth, and an archived record's payload is
// not guaranteed to be resident.
func PassesLifecycle(eng *storage.Engram, asOf *time.Time, includeInvalid bool) bool {
	if eng == nil {
		return false
	}
	switch eng.State {
	case storage.StateArchived:
		return false
	case storage.StateSoftDeleted:
		return (asOf != nil || includeInvalid) && !eng.ValidUntil.IsZero()
	default:
		return true
	}
}
