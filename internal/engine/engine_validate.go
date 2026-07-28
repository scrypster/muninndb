package engine

import (
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// createdAtFloor is the earliest acceptable CreatedAt for a WriteRequest.
// Values before this date are almost certainly bugs and risk ULID overflow.
var createdAtFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// createdAtSkew is the maximum amount of clock skew tolerated when a caller
// supplies a future CreatedAt.
const createdAtSkew = 5 * time.Minute

// validateCreatedAt returns an error if t is outside acceptable bounds:
//   - Lower bound: 2000-01-01T00:00:00Z  (protects against ULID overflow and nonsensical dates)
//   - Upper bound: now + 5 minutes       (clock skew tolerance; rejects future backdating)
func validateCreatedAt(t time.Time) error {
	if t.Before(createdAtFloor) {
		return fmt.Errorf("created_at must not be before 2000-01-01")
	}
	if t.After(time.Now().Add(createdAtSkew)) {
		return fmt.Errorf("created_at must not be more than 5 minutes in the future")
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
