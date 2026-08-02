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
// caller-supplied CreatedAt before 2000-01-01, so no legitimate record can carry
// a pre-2000 timestamp and the test can never swallow real data. Keep the two in
// step — TestCreatedAtFloor_IsAboveERFZeroTimeSentinel pins the relationship
// that matters most (the floor must sit strictly above the 1754 sentinel).
//
// This exists because the same literal comparison had grown four independent
// copies — computeComponents, computeACTR, the pruner's base-level scan and MCP
// staleness annotation — plus a fifth, semantically identical constant in
// engine_validate.go. Five literals agreeing by coincidence is not an invariant.
const MinPlausibleTimestampYear = 2000

// IsUnsetTimestamp reports whether t should be read as "never set" rather than
// as an instant. Use it for LastAccess-style fields whose absence means "no
// event yet"; do NOT use it for ValidFrom/ValidUntil, which carry their own
// documented raw-0 sentinels (see erf.decodeValidity).
func IsUnsetTimestamp(t time.Time) bool {
	return t.IsZero() || t.Year() < MinPlausibleTimestampYear
}

// normalizeEngramTimes is the single definition of the product-wide convention
// for the three engram timestamps that have NO on-disk zero-default sentinel:
//
//	CreatedAt  unset -> now
//	UpdatedAt  unset -> CreatedAt
//	LastAccess unset -> CreatedAt   ("created, never accessed")
//
// SCOPE, precisely: it governs the writers that CREATE these timestamps —
// WriteEngram, WriteEngramBatch, BatchWriter and CloneVaultData. It is NOT a
// funnel every 0x01 write passes through, and the six read-modify-write paths
// that encode a 0x01 record without it are deliberate, not oversights:
// pebbleStoreBatch.mutateEngram (backing UpdateEngramState and SupersedeEngram),
// SoftDelete, UpdateTags, UpdateConfidence, UpdateConfidenceWithContradiction
// and UpdateDigest. Those paths preserve whatever timestamps are already on
// disk, which is correct for a partial update — but it means they also rewrite a
// PRE-EXISTING sentinel back verbatim rather than healing it.
//
// The consequence is worth stating plainly: a vault cloned before #810 never
// self-heals through ordinary writes. Only TouchAccess and UpdateMetadata, which
// set LastAccess outright, repair a record. Everything else is repaired on the
// READ side by erf.decodeTimestamp, which is why that half of the fix is not
// redundant — and why the #811 index-rebuild ordering trap noted at
// WriteLastAccessEntry stays live indefinitely rather than decaying away.
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
