package keys

// PrefixLowerBound returns the lower bound for a prefix scan (inclusive).
func PrefixLowerBound(prefix []byte) []byte {
	return prefix
}

// PrefixUpperBound returns the exclusive upper bound for a prefix scan: the
// smallest key that sorts strictly above every key carrying prefix.
//
// STO-11. The bound has to be wrong in NEITHER direction:
//
//   - not inverted / too tight — a naive `bound[len-1]++` wraps to 0x00 for a
//     prefix ending in 0xFF and yields a bound BELOW the lower bound, so the
//     iterator returns nothing, silently, forever, for that prefix only; and
//   - not too loose — incrementing the first sub-0xFF byte from the right
//     WITHOUT clearing the trailing 0xFF bytes yields `03 ab ff` for `03 aa ff`,
//     admitting the whole of `03 ab 00 .. 03 ab fe`: a DIFFERENT prefix's
//     keyspace. That is the shape this helper shipped with until #816, for the
//     ~1 prefix in 256 whose last byte is 0xFF. (That is the rate at which the
//     BOUND was loose, not a loss rate — see STO-12 in the invariants for why an
//     actual crossing additionally needed a ~2^-64 ID coincidence.)
//
// Carrying leftward and TRUNCATING handles both: every byte after the
// incremented one is 0xFF in the input, so its carried value is 0x00, and
// dropping those bytes is equivalent to zeroing them while also excluding the
// bare key `03 ab` itself.
//
// # nil means UNBOUNDED
//
// When no finite exclusive upper bound exists — an all-0xFF prefix, or the
// empty prefix — this returns nil. `pebble.IterOptions.UpperBound == nil` means
// "no upper bound", which is the correct scan for those inputs. The values this
// helper used to return there (`append(prefix, 0x00)` and `[]byte{0x01}`) are
// bounds that EXCLUDE almost every key carrying the prefix — the silently-empty
// scan STO-11 exists to prevent.
//
// Callers on destructive paths should note that nil widens rather than narrows.
// No in-tree key layout can reach it: byte 0 of every scanned prefix is an
// allocated type prefix (internal/prefix, highest allocation 0x45), so the carry
// always terminates at or before byte 0. Pinned by
// TestPrefixUpperBound_IsTightAndNeverCrossesIntoANeighbouringPrefix.
func PrefixUpperBound(prefix []byte) []byte {
	if len(prefix) == 0 {
		return nil
	}

	bound := make([]byte, len(prefix))
	copy(bound, prefix)

	for i := len(bound) - 1; i >= 0; i-- {
		if bound[i] < 0xFF {
			bound[i]++
			return bound[:i+1]
		}
		// 0xFF: carry left.
	}

	// Every byte is 0xFF — no finite key sorts above this prefix.
	return nil
}
