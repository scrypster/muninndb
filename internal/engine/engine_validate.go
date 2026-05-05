package engine

import (
	"fmt"
	"time"
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
