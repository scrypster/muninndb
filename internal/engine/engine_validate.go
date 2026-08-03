package engine

import (
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// createdAtFloor is the earliest acceptable CreatedAt for a WriteRequest.
// Values before this date are almost certainly bugs and risk ULID overflow.
//
// It is ALSO load-bearing for an on-disk format decision in a different package,
// which is why it is derived from storage.MinPlausibleTimestampYear rather than
// spelled 2000 a second time: erf.decodeTimestamp maps the zero-time overflow
// bit pattern back to the zero time, and that mapping is aliased — the instant
// 1754-08-30T22:43:41.128654848Z encodes to the same bits and is destroyed on
// read. This floor is the ONLY thing that makes the alias unreachable. Lowering
// it to support a historical, genealogy or journal-import vault would make the
// collision live and silently lose data.
// Pinned by TestCreatedAtFloor_IsAboveERFZeroTimeSentinel.
var createdAtFloor = time.Date(storage.MinPlausibleTimestampYear, 1, 1, 0, 0, 0, 0, time.UTC)

// createdAtSkew is the maximum amount of clock skew tolerated when a caller
// supplies a future CreatedAt.
const createdAtSkew = 5 * time.Minute

// validateCreatedAt returns an error if t is outside acceptable bounds:
//   - Lower bound: createdAtFloor (2000-01-01T00:00:00Z) — protects against ULID
//     overflow and nonsensical dates, and keeps the ERF zero-time alias out of
//     reach; see the note on createdAtFloor before changing it.
//   - Upper bound: now + 5 minutes — clock skew tolerance; rejects future backdating.
func validateCreatedAt(t time.Time) error {
	if t.Before(createdAtFloor) {
		return fmt.Errorf("created_at must not be before %s", createdAtFloor.Format("2006-01-02"))
	}
	if t.After(time.Now().Add(createdAtSkew)) {
		return fmt.Errorf("created_at must not be more than 5 minutes in the future")
	}
	return nil
}

// validateStampTime guards a caller-supplied invalidation instant on the STAMP
// paths (forget.not_true_since, evolve.effective_at), which bypass applyValidity.
// It rejects — loudly, never silently — two truth-corrupting inputs:
//   - the Unix epoch (UnixNano()==0): the ERF zero-sentinel decodes raw 0 as
//     "open/current", so storing epoch as ValidUntil would leave the fact
//     appearing still-true while the handler reports it invalidated (a truth
//     inversion). Anything below createdAtFloor (2000-01-01) is likewise an
//     uninitialized-clock bug. NOTE: a genuine zero time (time.Time{}, year 1)
//     is the CLEAR sentinel used by restore and is intentionally NOT rejected
//     here — callers of this guard pass it only for real invalidation instants.
//   - an inverted window: until must be strictly after the fact's start, or the
//     [from, until) window is valid at no instant (a fact that was never true).
func validateStampTime(until, from time.Time) error {
	if until.Before(createdAtFloor) {
		return fmt.Errorf("%w: invalidation time (%s) is before %s — an epoch/uninitialized timestamp cannot be a validity bound (raw 0 is the ERF 'open/current' sentinel)",
			ErrInvalidRequest, until.UTC().Format(time.RFC3339), createdAtFloor.Format("2006-01-02"))
	}
	if !from.IsZero() && !until.After(from) {
		return fmt.Errorf("%w: invalidation time (%s) must be after the fact's start (%s) — the window would be valid at no instant",
			ErrInvalidRequest, until.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339))
	}
	return nil
}

// applyValidity validates and applies caller-supplied valid-time bounds to a
// new engram. The window is half-open [valid_from, valid_until); an empty or
// inverted window is rejected loudly (a fact that was never true is a caller
// bug, not something to store silently). Historical facts — both bounds in the
// past — are legitimate and accepted. Callers should have set eng.CreatedAt
// (if customized) before calling, so the ValidFrom default is correct.
func applyValidity(eng *storage.Engram, validFrom, validUntil *time.Time) error {
	if validFrom != nil {
		eng.ValidFrom = *validFrom
	}
	if validUntil != nil {
		from := eng.ValidFrom
		if from.IsZero() {
			from = eng.CreatedAt // may still be zero (= now at write time)
		}
		if from.IsZero() {
			from = time.Now()
		}
		if !validUntil.After(from) {
			return fmt.Errorf("%w: valid_until (%s) must be after valid_from (%s) — the window [valid_from, valid_until) would be empty",
				ErrInvalidRequest, validUntil.Format(time.RFC3339), from.Format(time.RFC3339))
		}
		eng.ValidUntil = *validUntil
	}
	return nil
}
