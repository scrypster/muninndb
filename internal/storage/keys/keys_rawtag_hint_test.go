package keys

import (
	"bytes"
	"testing"
)

// TestRawTagRangeKey_BytesIndependentOfCapacityHint pins that the capacity hint
// in RawTagRangeKey never changes the key: for values below, at and above the
// hint bound the result equals the layout built the plain way —
// 0x2B | ws(8) | Hash(tagKey)(4) | value(N) | 0x00 | id(16).
func TestRawTagRangeKey_BytesIndependentOfCapacityHint(t *testing.T) {
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	id := [16]byte{9, 9, 9, 9, 8, 8, 8, 8, 7, 7, 7, 7, 6, 6, 6, 6}
	const hash = uint32(0xA1B2C3D4)

	for _, n := range []int{0, 1, 7, 128, rawTagRangeKeyHintMaxValue - 1, rawTagRangeKeyHintMaxValue, rawTagRangeKeyHintMaxValue + 1, 3 * rawTagRangeKeyHintMaxValue} {
		value := bytes.Repeat([]byte{'v'}, n)

		want := append([]byte{}, RawTagRangePrefix(ws, hash)...)
		want = append(want, value...)
		want = append(want, 0x00)
		want = append(want, id[:]...)

		got := RawTagRangeKey(ws, hash, value, id)
		if !bytes.Equal(got, want) {
			t.Fatalf("value length %d: key differs from the documented layout (got %d bytes, want %d)", n, len(got), len(want))
		}
		if len(got) != rawTagRangeKeyFixedLen+n {
			t.Fatalf("value length %d: key length %d, want %d", n, len(got), rawTagRangeKeyFixedLen+n)
		}
	}
}
