package keys

import (
	"bytes"
	"testing"
)

// TestPrefixUpperBound_IsTightAndNeverCrossesIntoANeighbouringPrefix is the
// STO-11 property test for the mandated shared helper (#816).
//
// The two halves are not equally easy. "Every key under the prefix sorts below
// the bound" was already true of the pre-#816 implementation, which is why the
// defect survived. The half that matters is the second: NO key belonging to a
// DIFFERENT prefix may sort below the bound. The old implementation incremented
// the first sub-0xFF byte from the right and returned WITHOUT zeroing the
// trailing 0xFF bytes, so `03 aa ff` yielded `03 ab ff` — admitting the whole
// of `03 ab 00 .. 03 ab fe`, which is a different prefix's keyspace.
//
// Severity, stated accurately: ~1 prefix in 256 (any prefix whose last byte is
// 0xFF) got a LOOSE bound. That is the rate at which the bound is wrong, NOT a
// loss rate — see the STO-11/STO-12 notes for why an actual crossing needs a
// second engram sharing 14 of the victim's 16 ID bytes on top of it.
func TestPrefixUpperBound_IsTightAndNeverCrossesIntoANeighbouringPrefix(t *testing.T) {
	cases := []struct {
		name   string
		prefix []byte
		want   []byte // nil means "no finite upper bound exists" (all-0xFF)
	}{
		{
			name:   "no 0xFF byte — plain last-byte increment",
			prefix: []byte{0x03, 0xaa, 0x10},
			want:   []byte{0x03, 0xaa, 0x11},
		},
		{
			name:   "0xFF in the middle is untouched",
			prefix: []byte{0x03, 0xff, 0x10},
			want:   []byte{0x03, 0xff, 0x11},
		},
		{
			name:   "one trailing 0xFF — the ~1-in-256 case",
			prefix: []byte{0x03, 0xaa, 0xff},
			want:   []byte{0x03, 0xab},
		},
		{
			name:   "multiple trailing 0xFF",
			prefix: []byte{0x03, 0xaa, 0xff, 0xff, 0xff},
			want:   []byte{0x03, 0xab},
		},
		{
			name:   "carry reaches the type-prefix byte",
			prefix: []byte{0x03, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			want:   []byte{0x04},
		},
		{
			name:   "penultimate byte is 0xFE, last is 0xFF",
			prefix: []byte{0x25, 0xfe, 0xff},
			want:   []byte{0x25, 0xff},
		},
		{
			name: "all-0xFF has NO finite exclusive upper bound — nil (unbounded)",
			// Unreachable for every in-tree key layout: byte 0 is always a type
			// prefix and the highest allocated one is 0x45. Pinned so the
			// contract is explicit rather than incidental.
			prefix: []byte{0xff, 0xff, 0xff},
			want:   nil,
		},
		{
			// Same contract as all-0xFF: every key carries the empty prefix, so
			// no finite exclusive upper bound exists. The old []byte{0x01}
			// excluded every key from 0x01 up — a silently-empty scan.
			name:   "empty prefix — nil (unbounded)",
			prefix: nil,
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]byte{}, tc.prefix...)
			got := PrefixUpperBound(in)

			// Errorf, not Fatalf: the two property halves below are the real
			// assertions, and when the returned bound is wrong their messages
			// say WHICH direction it is wrong in. Bailing here would hide that.
			if !bytes.Equal(got, tc.want) {
				t.Errorf("PrefixUpperBound(%x) = %x, want %x", tc.prefix, got, tc.want)
			}

			// The helper must not mutate its argument — several call sites pass
			// a key builder's output and then reuse it as the lower bound.
			if !bytes.Equal(in, tc.prefix) {
				t.Errorf("PrefixUpperBound mutated its argument: %x -> %x", tc.prefix, in)
			}

			if tc.prefix == nil {
				return
			}

			if got == nil {
				// all-0xFF: assert the contract really is "nothing sorts above
				// this prefix", which is what makes a finite bound impossible.
				if !allFF(tc.prefix) {
					t.Fatalf("PrefixUpperBound(%x) returned nil for a prefix that is not all-0xFF", tc.prefix)
				}
				return
			}

			// Half one: every key carrying the prefix sorts strictly below the
			// bound. Probe the extremes of the suffix space plus the bare
			// prefix itself.
			for _, suffix := range [][]byte{
				nil,
				{0x00},
				{0xff},
				{0xff, 0xff, 0xff, 0xff},
				bytes.Repeat([]byte{0xff}, 32),
			} {
				k := append(append([]byte{}, tc.prefix...), suffix...)
				if bytes.Compare(k, got) >= 0 {
					t.Errorf("bound is TOO TIGHT: key %x carries the prefix but sorts at or above the bound %x", k, got)
				}
			}

			// Half two — the half the old implementation failed. Every key that
			// sorts in [prefix, bound) must carry the prefix. It suffices to
			// check the greatest key strictly below the bound: bound with its
			// last byte decremented, padded with 0xFF.
			if greatest := greatestKeyBelow(got); greatest != nil {
				if !bytes.HasPrefix(greatest, tc.prefix) {
					t.Errorf("bound is LOOSE: %x is admitted by the bound %x but belongs to a DIFFERENT prefix than %x",
						greatest, got, tc.prefix)
				}
			}
		})
	}
}

func allFF(b []byte) bool {
	for _, c := range b {
		if c != 0xFF {
			return false
		}
	}
	return len(b) > 0
}

// greatestKeyBelow returns the largest key that sorts strictly below bound
// among keys of length <= len(bound)+32: decrement the last byte and pad with
// 0xFF. Returns nil when bound's last byte is 0x00 and it is one byte long
// (no such key of interest).
func greatestKeyBelow(bound []byte) []byte {
	if len(bound) == 0 || bound[len(bound)-1] == 0x00 {
		return nil
	}
	k := append([]byte{}, bound...)
	k[len(k)-1]--
	return append(k, bytes.Repeat([]byte{0xff}, 32)...)
}
