package storage

import (
	"hash/fnv"
	"sort"
	"sync"
)

const lockStripes = 256

// stripedMutex is a fixed-size array of mutexes for lock striping.
// Replaces unbounded sync.Map-based lock pools. Memory usage is constant at
// lockStripes × sizeof(sync.Mutex) ≈ 6 KB regardless of key count.
//
// Two different keys may map to the same mutex stripe (false sharing), but at
// 256 stripes this is negligible in practice and safe — contention causes
// brief waiting, never data corruption.
type stripedMutex struct {
	mu [lockStripes]sync.Mutex
}

// For returns the mutex for the given byte key using FNV-32a hashing.
func (s *stripedMutex) For(key []byte) *sync.Mutex {
	return &s.mu[stripeIndex(key)]
}

func stripeIndex(key []byte) uint32 {
	h := fnv.New32a()
	h.Write(key)
	return h.Sum32() % lockStripes
}

// lockMany locks the stripes covering keys, each stripe once, in ascending
// stripe order, and returns the function that unlocks them. The fixed order
// is what keeps two multi-stripe holders from deadlocking each other. The
// stripes are not reentrant: the caller must hold none of them already.
func (s *stripedMutex) lockMany(keys [][]byte) (unlock func()) {
	idx := make([]uint32, 0, len(keys))
	seen := make(map[uint32]struct{}, len(keys))
	for _, k := range keys {
		i := stripeIndex(k)
		if _, dup := seen[i]; dup {
			continue
		}
		seen[i] = struct{}{}
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a] < idx[b] })
	for _, i := range idx {
		s.mu[i].Lock()
	}
	return func() {
		for j := len(idx) - 1; j >= 0; j-- {
			s.mu[idx[j]].Unlock()
		}
	}
}
