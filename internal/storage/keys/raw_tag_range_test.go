package keys

import (
	"bytes"
	"testing"
)

// TestRawTagRange_PrefixOfEachOther verifies that raw-tag-range keys for
// values that are textual prefixes of one another sort correctly: "2026" <
// "2026-07" < "2026-07-27". This is the entire point of the 0x00 separator
// between value and id in RawTagRangeKey — without it, "2026-07-27" (which
// starts with the bytes of "2026") could sort BEFORE "2026" itself once id
// bytes are appended, since e.g. "2026-07-27" starts with '2','0','2','6'
// then '-' (0x2D), while "2026" transitions straight to the id bytes. The
// separator forces "2026"+0x00 < "2026-07-27"+0x00 because 0x00 < 0x2D.
func TestRawTagRange_PrefixOfEachOther(t *testing.T) {
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	tagKeyHash := Hash("due")

	id1 := [16]byte{1}
	id2 := [16]byte{2}
	id3 := [16]byte{3}

	kShort := RawTagRangeKey(ws, tagKeyHash, []byte("2026"), id1)
	kMid := RawTagRangeKey(ws, tagKeyHash, []byte("2026-07"), id2)
	kLong := RawTagRangeKey(ws, tagKeyHash, []byte("2026-07-27"), id3)

	if bytes.Compare(kShort, kMid) >= 0 {
		t.Errorf("expected %q < %q, got kShort >= kMid (% x vs % x)", "2026", "2026-07", kShort, kMid)
	}
	if bytes.Compare(kMid, kLong) >= 0 {
		t.Errorf("expected %q < %q, got kMid >= kLong (% x vs % x)", "2026-07", "2026-07-27", kMid, kLong)
	}
	if bytes.Compare(kShort, kLong) >= 0 {
		t.Errorf("expected %q < %q, got kShort >= kLong", "2026", "2026-07-27")
	}

	// Also confirm a bounded scan for lte "2026" only includes the value
	// "2026" (id1), not "2026-07" or "2026-07-27" — this is exactly the
	// due:<=X range-scan bound used by activation's tag_prefix seeding.
	lower, upper := RawTagRangeBound(ws, tagKeyHash, "lte", []byte("2026"))
	for _, tc := range []struct {
		name string
		key  []byte
		want bool
	}{
		{"2026 (exact)", kShort, true},
		{"2026-07 (longer, greater)", kMid, false},
		{"2026-07-27 (longer, greater)", kLong, false},
	} {
		got := bytes.Compare(tc.key, lower) >= 0 && bytes.Compare(tc.key, upper) < 0
		if got != tc.want {
			t.Errorf("lte 2026 bound: key %s in range = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRawTagRange_NonISODateDocumented documents (and asserts) the known
// limitation that non-zero-padded ("non-ISO") date strings sort lexically,
// not chronologically, in the raw-tag-range index. "due:2026-7-4" (July 4,
// unpadded) sorts, byte-for-byte, AFTER "due:2026-12-1" (December 1) because
// '7' (0x37) > '1' (0x31) at the first differing byte — even though July is
// chronologically before December. Callers relying on lte/gte range bounds
// MUST write zero-padded ISO 8601 dates ("2026-07-04", not "2026-7-4") for
// the ordering to match calendar order. This is a deliberate, documented
// consequence of the raw-value byte-sort design (S1), not a bug.
func TestRawTagRange_NonISODateDocumented(t *testing.T) {
	ws := [8]byte{1}
	tagKeyHash := Hash("due")
	id1 := [16]byte{1}
	id2 := [16]byte{2}

	july := RawTagRangeKey(ws, tagKeyHash, []byte("2026-7-4"), id1)      // non-ISO, unpadded month
	december := RawTagRangeKey(ws, tagKeyHash, []byte("2026-12-1"), id2) // non-ISO, unpadded

	// Lexical order: "2026-7-4" > "2026-12-1" (byte '7' > '1'), even though
	// July 4 is chronologically BEFORE December 1. Document this explicitly.
	if bytes.Compare(july, december) <= 0 {
		t.Fatalf("expected non-ISO %q to sort AFTER %q lexically (documenting the limitation), got july <= december",
			"2026-7-4", "2026-12-1")
	}

	// The ISO 8601 zero-padded equivalents sort correctly (chronologically).
	julyISO := RawTagRangeKey(ws, tagKeyHash, []byte("2026-07-04"), id1)
	decemberISO := RawTagRangeKey(ws, tagKeyHash, []byte("2026-12-01"), id2)
	if bytes.Compare(julyISO, decemberISO) >= 0 {
		t.Fatalf("expected zero-padded ISO %q to sort BEFORE %q, got julyISO >= decemberISO",
			"2026-07-04", "2026-12-01")
	}
}
