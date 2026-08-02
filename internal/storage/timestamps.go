package storage

import "time"

// normalizeEngramTimes applies the product-wide convention for the three
// engram timestamps that have NO on-disk zero-default sentinel:
//
//	CreatedAt  unset -> now
//	UpdatedAt  unset -> CreatedAt
//	LastAccess unset -> CreatedAt   ("created, never accessed")
//
// Every writer of a 0x01 record must funnel through this. It is a function
// rather than three copy-pasted if-blocks because duplicating the rule is
// exactly how #810 happened: WriteEngram, WriteEngramBatch and BatchWriter each
// carried their own copy, and CloneVaultData — which encodes and Set()s ERF
// bytes directly, bypassing all three — invented a second, incompatible answer
// (the zero time). ERF stores these fields as uint64(t.UnixNano()); time.Time{}
// is outside UnixNano's defined range, so the zero time round-trips to
// 1754-08-30, whose IsZero() is false. Every downstream guard waved it through.
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
