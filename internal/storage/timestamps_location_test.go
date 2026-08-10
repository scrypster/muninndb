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

	// Walk the whole band the disagreement could live in, on BOTH sides of the
	// floor: every hour of the last UTC day BEFORE the floor year and the first
	// UTC day of it, in every whole-hour zone offset.
	//
	// The negative half is load-bearing and was missing. With `hour := 0` the
	// generated instants are all >= floor, so admittedByFloor is always true and
	// the second assertion below is structurally dead — it can never execute its
	// error. Measured: loosening IsUnsetTimestamp to `< MinPlausibleTimestampYear-1`
	// breaks direction 2 alone, and the 0..23 loop still passed.
	//
	// Direction 2 is the half that says WHY the comparison is .UTC() and not
	// merely "some fixed location that isn't the value's own". Its witness is the
	// mirror of the one in the doc comment: 1999-12-31T20:00:00Z rendered in
	// UTC+14 has a LOCAL Year() of 2000, so the pre-fix `t.Year()` read it as SET
	// while the floor refuses the instant outright — a value nothing may store,
	// treated as a real timestamp. Positive-offset zones produce that half;
	// negative-offset zones produce the first half. Only a UTC comparison closes
	// both, which is what makes IsUnsetTimestamp the floor's exact complement
	// rather than an approximation with a bias in one direction.
	for hour := -24; hour < 24; hour++ {
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

	// The mirror case, direction 2, asserted directly for the same reason: the
	// loop above proves it, but the loop is now 48x27 iterations and this is the
	// one that has to be legible. 1999-12-31T20:00:00Z in UTC+14 reports a local
	// Year() of 2000, so the pre-fix t.Year() called it SET — while the floor
	// refuses the instant. A value the floor will not store, read as a real
	// timestamp: the exact complement failing in the other direction.
	w := floor.Add(-4 * time.Hour).In(time.FixedZone("positive", 14*3600))
	if w.Year() != MinPlausibleTimestampYear {
		t.Fatalf("mirror fixture no longer exhibits the local-year skew (Year()=%d); the case is vacuous", w.Year())
	}
	if !w.Before(floor) {
		t.Fatalf("mirror fixture is no longer refused by the floor; the case is vacuous")
	}
	if !IsUnsetTimestamp(w) {
		t.Errorf("IsUnsetTimestamp(%s) = false; its local Year() is %d but its UTC year is %d and the "+
			"floor REFUSES it, so it must read as unset", w.Format(time.RFC3339), w.Year(), w.UTC().Year())
	}
}
