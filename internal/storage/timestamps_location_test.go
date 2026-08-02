package storage

import (
	"testing"
	"time"
)

// TestIsUnsetTimestamp_IsLocationIndependent closes a gap between two things
// #810 claimed were "kept in step": storage.IsUnsetTimestamp and
// engine.createdAtFloor.
//
// createdAtFloor is 2000-01-01T00:00:00Z compared with Before(), which is an
// absolute-instant test. IsUnsetTimestamp used t.Year(), which is evaluated in
// the VALUE'S OWN LOCATION. For any instant in the ~14-hour band from
// 2000-01-01T00:00:00Z up to 2000-01-01T14:00:00Z, a time.Time carrying a
// negative-offset zone reports Year() == 1999 while the floor happily admits
// the same instant:
//
//	2000-01-01T05:00:00Z  loc=UTC-11  Year()=1999  IsUnsetTimestamp=true
//
// A caller-supplied CreatedAt in that band therefore passed validateCreatedAt
// and was then read as "never set" by every guard — a 26-year-old timestamp, so
// genuinely cosmetic in practice. The overstatement was not: "the test can never
// swallow real data" and "must stay in step with it" were false as written.
//
// Comparing t.UTC().Year() makes IsUnsetTimestamp the EXACT complement of the
// floor rather than an approximation of it, so the two are in step structurally
// instead of by inspection.
func TestIsUnsetTimestamp_IsLocationIndependent(t *testing.T) {
	floor := time.Date(MinPlausibleTimestampYear, 1, 1, 0, 0, 0, 0, time.UTC)

	// Walk the whole band the disagreement could live in: every hour of the
	// first UTC day of the floor year, in every whole-hour zone offset.
	for hour := 0; hour < 24; hour++ {
		instant := floor.Add(time.Duration(hour) * time.Hour)
		for offset := -12; offset <= 14; offset++ {
			loc := time.FixedZone("test", offset*3600)
			v := instant.In(loc)

			admittedByFloor := !v.Before(floor)
			readAsUnset := IsUnsetTimestamp(v)
			if admittedByFloor && readAsUnset {
				t.Errorf("%s (loc=UTC%+d) is admitted by createdAtFloor but IsUnsetTimestamp reports "+
					"it unset: a real, storable timestamp is read as \"never happened\". Compare in a "+
					"fixed location (t.UTC().Year()), not the value's own.",
					v.Format(time.RFC3339), offset)
			}
			if !admittedByFloor && !readAsUnset {
				t.Errorf("%s (loc=UTC%+d) is refused by createdAtFloor but IsUnsetTimestamp reports it "+
					"SET: the two are out of step in the other direction",
					v.Format(time.RFC3339), offset)
			}
		}
	}

	// The concrete case from the review round, asserted directly so the
	// regression is legible without reading the loop.
	v := floor.Add(5 * time.Hour).In(time.FixedZone("negative", -11*3600))
	if v.Year() != MinPlausibleTimestampYear-1 {
		t.Fatalf("fixture no longer exhibits the local-year skew (Year()=%d); the case is vacuous", v.Year())
	}
	if IsUnsetTimestamp(v) {
		t.Errorf("IsUnsetTimestamp(%s) = true; its local Year() is %d but its UTC year is %d and the "+
			"floor admits it", v.Format(time.RFC3339), v.Year(), v.UTC().Year())
	}
}
