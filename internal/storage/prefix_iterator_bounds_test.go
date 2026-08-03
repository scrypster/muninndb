package storage

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestPrefixIterator_DoesNotEscapeIntoTheNeighbouringPrefix pins the third copy
// of the #816 defect.
//
// storage.PrefixIterator open-coded a byte-identical duplicate of
// keys.PrefixUpperBound's pre-#816 carry loop — increment the first sub-0xFF
// byte from the right, break, never clear the trailing 0xFF bytes — which is why
// the STO-12 census had to name it alongside the helper. Two implementations of
// one keyspace rule means fixing one of them fixes nothing; it now delegates.
//
// Everything under a 25-byte kind|ws|id prefix that PrefixIterator serves to a
// DELETE loop (DeleteEngram's 0x25 archived-source cascade) is held by an
// explicit bytes.Equal guard as well, so this is not a data-loss report — it is
// the bound itself, tested without the compensation in the way.
func TestPrefixIterator_DoesNotEscapeIntoTheNeighbouringPrefix(t *testing.T) {
	ps := newTestStore(t)

	// A prefix ending in 0xFF, and the neighbour that a loose bound admits.
	target := []byte{0x25, 0xa1, 0xff}
	neighbour := []byte{0x25, 0xa2, 0x00}

	rows := [][]byte{
		append(append([]byte{}, target...), 0x00),
		append(append([]byte{}, target...), 0xff),
		append(append([]byte{}, neighbour...), 0x01),
		append(append([]byte{}, neighbour...), 0xff),
	}
	for _, k := range rows {
		if err := ps.db.Set(k, []byte("x"), pebble.NoSync); err != nil {
			t.Fatalf("seed %x: %v", k, err)
		}
	}

	iter, err := PrefixIterator(ps.db, target)
	if err != nil {
		t.Fatalf("PrefixIterator: %v", err)
	}
	defer iter.Close()

	seen := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		k := append([]byte{}, iter.Key()...)
		if !bytes.HasPrefix(k, target) {
			t.Errorf("PrefixIterator returned %x, which does not carry the prefix %x — the upper bound reaches into a neighbouring prefix", k, target)
			continue
		}
		seen++
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("iter: %v", err)
	}
	if seen != 2 {
		t.Errorf("PrefixIterator returned %d of the prefix's own 2 rows — the bound is too tight", seen)
	}
}
