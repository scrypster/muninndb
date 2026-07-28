package storage

// Importance — the caller-asserted priority axis.
//
// Importance is orthogonal to Confidence (truth) and AccessCount (use): it says
// "this matters", not "this is correct" or "this is used often". The stored
// field (Engram.Importance / EngramMeta.Importance, ERF offset 88) holds ONLY
// what the caller explicitly asserted at write/evolve time; 0 means unset.
//
// EffectiveImportance is the single use-time derivation. It is NEVER written to
// storage: storing a derived value would be a silent write the caller never
// made (principle #1 — explicit config is never silently substituted) and
// would freeze the heuristic table into every record. Because derivation
// happens at read time, "asserted" (stored > 0) and "assumed" (stored == 0,
// table applied) stay structurally distinct, and the table below can be tuned
// later with zero migration.
//
// In this increment importance drives exactly one behavior: the MaxEngrams
// pruner exemption (COG-20) — plus exposure on the read surfaces. It does NOT
// modify decay rates, reinforcement, or recall ranking; importance-modulated
// decay is deferred until it is designed against all three scoring modes
// (ACT-R, weighted_sum, RRF — Stability/Relevance feed the score in the
// latter two, so modulating them changes recall results).
//
// This lives in package storage (not internal/cognitive) because it derives
// from storage types, matching the valid-time precedent: EffectiveValidFrom /
// ValidAt / IsExpired are use-time derivations defined beside the types they
// derive from.

// HighImportanceFloor is the EffectiveImportance value at or above which an
// engram is exempt from the MaxEngrams (retrieval-strength) prune path
// (COG-20). RetentionDays remains an authoritative age policy and is NOT
// gated by this floor.
const HighImportanceFloor = 0.7

// EffectiveImportance returns the use-time importance in [0,1] for an engram
// with the given explicitly stored importance, memory type, and trust level.
//
// If the caller asserted a value (explicit > 0 — the write path quantizes an
// explicit 0.0 to 0.01, so stored 0 always means "unset"), that value wins,
// clamped to [0,1].
//
// Otherwise the value is derived from the memory type:
//
//	Decision, Goal, Constraint, Identity  → 0.6
//	Preference, Procedure                 → 0.5
//	Fact, Reference, Issue                → 0.4
//	Observation, Event, Task              → 0.3
//	(unknown types fall back to 0.3)
//
// plus +0.1 when Trust == TrustVerified (human-confirmed content matters
// more), capped at 1.0. The trust bump applies only to the derived path — an
// explicit assertion is honored verbatim.
func EffectiveImportance(explicit float32, memType MemoryType, trust TrustLevel) float32 {
	if explicit > 0 {
		if explicit > 1 {
			return 1
		}
		return explicit
	}
	var imp float32
	switch memType {
	case TypeDecision, TypeGoal, TypeConstraint, TypeIdentity:
		imp = 0.6
	case TypePreference, TypeProcedure:
		imp = 0.5
	case TypeFact, TypeReference, TypeIssue:
		imp = 0.4
	case TypeObservation, TypeEvent, TypeTask:
		imp = 0.3
	default:
		imp = 0.3
	}
	if trust == TrustVerified {
		imp += 0.1
	}
	if imp > 1 {
		imp = 1
	}
	return imp
}

// EffectiveImportance returns the use-time importance for a full engram.
// See the package-level EffectiveImportance for the derivation table.
func (e *Engram) EffectiveImportance() float32 {
	return EffectiveImportance(e.Importance, e.MemoryType, e.Trust)
}

// EffectiveImportance returns the use-time importance for slim metadata.
// See the package-level EffectiveImportance for the derivation table.
func (m *EngramMeta) EffectiveImportance() float32 {
	return EffectiveImportance(m.Importance, m.MemoryType, m.Trust)
}

// ImportanceExplicit reports whether the stored importance was explicitly
// asserted by the caller (stored > 0) rather than derived from the type table.
func ImportanceExplicit(stored float32) bool {
	return stored > 0
}
