package storage

import "time"

// MinPlausibleTimestampYear is the earliest year an engram timestamp produced by
// a real clock can have. Anything below it is an unset, uninitialized or
// corrupted value and must be treated as "never happened", never as an instant.
//
// Two distinct populations land under it, which is why the year test rather than
// IsZero() alone is the right shape:
//
//   - year 1 — a plain time.Time{} that reached a consumer without going through
//     the ERF encoder at all;
//   - year 1754 — erf.ZeroTimeSentinelNanos, the bit pattern a zero time
//     overflows to through uint64(t.UnixNano()), whose IsZero() is FALSE (#810).
//
// It is 2000 for one reason: engine.createdAtFloor already refuses any
// caller-supplied CreatedAt before 2000-01-01T00:00:00Z, so no legitimate record
// can carry a pre-2000 timestamp. Because IsUnsetTimestamp compares the UTC
// year, the two are EXACT COMPLEMENTS over the whole time domain rather than
// approximately aligned: a value the floor admits is never read as unset, in any
// location. TestIsUnsetTimestamp_IsLocationIndependent pins that both ways, and
// TestCreatedAtFloor_IsAboveERFZeroTimeSentinel pins the relationship that
// matters most (the floor must sit strictly above the 1754 sentinel).
//
// This exists because the same literal comparison had grown two independent
// copies at the merge base — computeACTR (activation/engine.go) and the pruner's
// base-level scan (engine/engine.go) — plus a semantically identical constant in
// engine_validate.go, and #810 immediately minted two more (computeComponents,
// MCP staleness) before the helper landed. Literals agreeing by coincidence are
// not an invariant. The set of sites that must call this is now REGENERATED,
// not asserted: TestLastAccessElapsedCensus (lastaccess_census_test.go) walks
// the AST of every non-test file in the module and fails on any unguarded
// elapsed-time computation from a LastAccess value.
const MinPlausibleTimestampYear = 2000

// IsUnsetTimestamp reports whether t should be read as "never set" rather than
// as an instant. Use it for LastAccess-style fields whose absence means "no
// event yet"; do NOT use it for ValidFrom/ValidUntil, which carry their own
// documented raw-0 sentinels (see erf.decodeValidity).
//
// The year is taken in UTC deliberately. t.Year() is evaluated in the value's
// own location, which put it out of step with engine.createdAtFloor (an
// absolute-instant Before() test) across a ~14-hour band on 2000-01-01: an
// instant the floor admitted could report a local Year() of 1999 and be read as
// unset. Cosmetic in effect — 26-year-old timestamps — but the claim that the
// two were in step was not true as written, and the fixed-location comparison
// makes it true structurally.
func IsUnsetTimestamp(t time.Time) bool {
	return t.IsZero() || t.UTC().Year() < MinPlausibleTimestampYear
}

// normalizeEngramTimes is the single definition of the product-wide convention
// for the three engram timestamps that have NO on-disk zero-default sentinel:
//
//	CreatedAt  unset -> now
//	UpdatedAt  unset -> CreatedAt
//	LastAccess unset -> CreatedAt   ("created, never accessed")
//
// SCOPE, precisely: it governs the writers that CREATE these timestamps —
// WriteEngram, WriteEngramBatch, BatchWriter and CloneVaultData. Note what the
// clone case means after the second #810 fix: CloneVaultData passes the SOURCE's
// LastAccess in, not time.Time{}, so the "unset -> CreatedAt" rule fires there
// only when the source record itself has no access time. Carrying the source's
// value is the point (a clone must be recall-equivalent to its source); routing
// it through here is what stops a pre-#810 vault's sentinel from being inherited
// by every re-clone. It is NOT a
// funnel every 0x01 write passes through, and the six read-modify-write paths
// that encode a 0x01 record without it are deliberate, not oversights:
// pebbleStoreBatch.mutateEngram (backing UpdateEngramState and SupersedeEngram),
// SoftDelete, UpdateTags, UpdateConfidence, UpdateConfidenceWithContradiction
// and UpdateDigest. Those paths preserve whatever timestamps are already on
// disk, which is correct for a partial update — but it means they also rewrite a
// PRE-EXISTING sentinel back verbatim rather than healing it. All six are
// EXERCISED, not merely named, by TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal
// (rmwWriters) — an earlier version of that pin named six and drove one, and
// adding normalization to any of the other five left it green.
//
// The consequence is worth stating plainly: a vault cloned before #810 never
// self-heals through ordinary writes. Exactly ONE code path repairs a record —
// TouchAccess, which supplies time.Now(). UpdateMetadata does NOT: it is a
// pass-through (erf.PatchAllMeta writes meta.LastAccess verbatim, and a zero
// time re-encodes to the sentinel), so whether it heals depends entirely on what
// the caller put in the field. Only one of its four callers supplies a clock:
//
//	engram.go        TouchAccess     time.Now()                 HEALS
//	consolidation/dedup.go           representative.LastAccess  perpetuates
//	engine/engine.go Restore         eng.LastAccess             perpetuates
//	storage/lease.go CompareAndSet   updated := *cur            perpetuates
//
// The three perpetuating callers each carry a value that came from a decode,
// which the read-side repair has already turned into the zero time, so they
// round-trip it straight back to the sentinel. Pinned by
// TestUpdateMetadata_HealsOnlyWhenTheCallerSuppliesALastAccess.
//
// Everything else is repaired on the READ side by erf.decodeTimestamp, which is
// why that half of the fix is not redundant — and why the #811 index-rebuild
// ordering trap noted at WriteLastAccessEntry stays live indefinitely rather
// than decaying away.
//
// It is a function rather than three copy-pasted if-blocks because duplicating
// the rule is exactly how #810 happened: WriteEngram, WriteEngramBatch and
// BatchWriter each carried their own copy, and CloneVaultData — which encodes
// and Set()s ERF bytes directly, bypassing all three — invented a second,
// incompatible answer (the zero time). ERF stores these fields as
// uint64(t.UnixNano()); time.Time{} is outside UnixNano's defined range, so the
// zero time round-trips to 1754-08-30, whose IsZero() is false. Every downstream
// guard waved it through.
//
// ValidFrom/ValidUntil are deliberately NOT handled here: they carry documented
// raw-0 sentinels on both the encode and the decode side (see erf.decodeValidity)
// and normalizing them would break COG-19's "open / still current" semantics.
func normalizeEngramTimes(createdAt, updatedAt, lastAccess time.Time) (time.Time, time.Time, time.Time) {
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	if lastAccess.IsZero() {
		lastAccess = createdAt
	}
	return createdAt, updatedAt, lastAccess
}
