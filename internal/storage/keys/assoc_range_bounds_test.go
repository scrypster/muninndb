package keys

import (
	"bytes"
	"testing"

	"github.com/scrypster/muninndb/internal/prefix"
)

// TestAssocRangeEnds_NeverInvertTheirBound is the STO-11 unit pin for both
// vault-wide association range bounds (#819).
//
// AssocFwdRangeEnd used to open-code its own carry loop that stopped at index 1
// so it could not touch the 0x03 type byte. For an ALL-0xFF workspace prefix
// every workspace byte wrapped to 0x00 and the loop ran out of indices, giving
// 0x03|00..00 — an upper bound BELOW the lower bound. Pebble then returns
// nothing, silently and forever, for that vault only. AssocRevRangeEnd already
// delegated to PrefixUpperBound and had no such edge; #819 makes the forward
// side do the same so the keyspace has one rule rather than two.
//
// Probability of an all-0xFF SipHash vault prefix is 2^-64, i.e. it will not
// happen. That is why this is a one-line delegation and a unit pin rather than
// an incident: the point is that there is exactly one bound rule.
func TestAssocRangeEnds_NeverInvertTheirBound(t *testing.T) {
	cases := []struct {
		name string
		ws   [8]byte
	}{
		{"ordinary workspace prefix", [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}},
		{"last byte 0xFF — the ~1-in-256 case", [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0xFF}},
		{"trailing 0xFF run", [8]byte{0x11, 0x22, 0x33, 0x44, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"all-0xFF workspace prefix (2^-64)", [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
		{"all-zero workspace prefix", [8]byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, side := range []struct {
				name  string
				start func([8]byte) []byte
				end   func([8]byte) []byte
				kind  byte
			}{
				{"forward 0x03", AssocFwdRangeStart, AssocFwdRangeEnd, prefix.AssocFwd},
				{"reverse 0x04", AssocRevRangeStart, AssocRevRangeEnd, prefix.AssocRev},
			} {
				lower := side.start(tc.ws)
				upper := side.end(tc.ws)

				if upper == nil {
					t.Errorf("%s: upper bound is nil (unbounded) — byte 0 is the %#x type prefix and can never be 0xFF, so a finite bound always exists",
						side.name, side.kind)
					continue
				}
				if bytes.Compare(upper, lower) <= 0 {
					t.Errorf("%s: INVERTED bound for ws %x: upper %x <= lower %x — the iterator returns nothing, silently, forever, for this vault",
						side.name, tc.ws, upper, lower)
				}

				// Every key in this vault's range must sort strictly below the
				// bound: prefix | ws | id(16) | ... — probe the maximal suffix.
				maxKey := append(append([]byte{}, lower...), bytes.Repeat([]byte{0xFF}, 36)...)
				if bytes.Compare(maxKey, upper) >= 0 {
					t.Errorf("%s: bound is TOO TIGHT for ws %x: key %x is in this vault's range but sorts at or above upper %x",
						side.name, tc.ws, maxKey, upper)
				}

				// And nothing from the NEXT vault's range may be admitted.
				if greatest := greatestKeyBelow(upper); greatest != nil && !bytes.HasPrefix(greatest, lower) {
					t.Errorf("%s: bound is LOOSE for ws %x: %x is admitted by upper %x but belongs to a different vault or key kind",
						side.name, tc.ws, greatest, upper)
				}
			}
		})
	}
}
