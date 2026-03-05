package storage

import (
	"github.com/bits-and-blooms/bloom/v3"
	"github.com/cockroachdb/pebble"
)

// archiveBloom is an in-memory Bloom filter over src engram IDs that have
// archived associations in the 0x25 namespace.
//
// Standard Bloom filters do not support removal. False positives from
// non-removed entries are acceptable — they trigger a cheap 0x25 prefix
// scan that finds nothing. The weekly GC pass calls RebuildArchiveBloom
// to periodically compact the filter.
type archiveBloom struct {
	f *bloom.BloomFilter
}

// newArchiveBloom creates a Bloom filter sized for n expected entries with
// ~1% false positive rate.
func newArchiveBloom(n uint) *archiveBloom {
	return &archiveBloom{f: bloom.NewWithEstimates(n, 0.01)}
}

// Add adds a src engram ID to the filter.
func (b *archiveBloom) Add(id [16]byte) {
	b.f.Add(id[:])
}

// MayContain returns true if the src engram ID may have archived edges.
// False positives are possible; false negatives are not.
func (b *archiveBloom) MayContain(id [16]byte) bool {
	return b.f.Test(id[:])
}

// RebuildArchiveBloom scans the entire 0x25 prefix across all vaults and
// returns a new Bloom filter populated with all src engram IDs found.
// Called on startup and after GC runs.
func (ps *PebbleStore) RebuildArchiveBloom() *archiveBloom {
	// Scan 0x25 | * (all vaults).
	startKey := []byte{0x25}
	endKey := []byte{0x26} // exclusive upper bound for prefix 0x25

	iterOpts := &pebble.IterOptions{
		LowerBound: startKey,
		UpperBound: endKey,
	}

	iter, err := ps.db.NewIter(iterOpts)
	if err != nil {
		return newArchiveBloom(1000)
	}
	defer iter.Close()

	b := newArchiveBloom(1_000_000) // sized for 1M entries (~decade of heavy use)
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		// Archive key layout: 0x25(1) | ws(8) | src(16) | dst(16) = 41 bytes
		if len(k) < 25 {
			continue
		}
		var srcID [16]byte
		copy(srcID[:], k[9:25]) // src starts at byte 9
		b.Add(srcID)
	}
	return b
}

// ArchiveBloomMayContain returns true if the src engram ID may have archived edges.
// Safe default: returns true (always scan) if the filter is not initialized.
func (ps *PebbleStore) ArchiveBloomMayContain(id [16]byte) bool {
	if ps.archiveBloom == nil {
		return true // safe default: always scan if filter not initialized
	}
	return ps.archiveBloom.MayContain(id)
}

// AddToArchiveBloom adds a src engram ID to the in-memory Bloom filter.
// Called after archiving an edge so the filter stays current without a full rebuild.
func (ps *PebbleStore) AddToArchiveBloom(id [16]byte) {
	if ps.archiveBloom != nil {
		ps.archiveBloom.Add(id)
	}
}
