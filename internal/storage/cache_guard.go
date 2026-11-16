package storage

import (
	"encoding/binary"
	"sync/atomic"
)

// fillGuard makes a stale read-through cache fill impossible: a value read
// BEFORE a write committed can never be left cached AFTER that write.
//
// # Protocol
//
// Writer, for every engram/association key a committed batch touched
// (PebbleStore.commitBatch does this, so no write path can skip it):
//
//	W1  commit the Pebble batch
//	W2  bump:  v = clock.Add(1);  stripe[k] = max(stripe[k], v)
//	W3  invalidate the cache entries for k
//
// Reader (any read-through fill):
//
//	R1  t = clock.Load()        — BEFORE the source is pinned: before the
//	                              Pebble Get / NewIter, or before NewSnapshot
//	                              for a snapshot read (NewSnapshotContext)
//	R2  read the source
//	R3  install the value (skipped if stripe[k] > t already)
//	R4  if stripe[k] > t: remove the value R3 installed
//
// # Why it is correct
//
// Suppose R2 did not see write W's commit. If W's bump value v were <= t,
// then W2's Add precedes R1's Load in the total order of atomic operations on
// clock, so W1 happens-before W2 happens-before R1 happens-before R2 and
// Pebble guarantees R2 sees W1 — a contradiction. So v > t, and stripe[k] >= v
// from W2 onwards (it only grows). Go's atomics and sync.Map operations are
// sequentially consistent, so either W2 precedes R4 — R4 sees stripe[k] > t
// and removes the value — or R4 precedes W2, in which case R3 precedes W3
// and W3 removes it. Either way the stale value is gone once both finish. A
// value whose source DID see W is not stale, whatever the guard decides.
//
// A false positive (two keys sharing a stripe, or the conservative bumps)
// only skips a fill — a later Pebble read — never serves a wrong value.
//
// The only cost on the hit path is zero: hits do not consult the guard. A
// miss pays one atomic load for t and two for the stripe.
type fillGuard struct {
	clock atomic.Uint64
	// eng guards per-engram values (L1 engram cache, metaCache), keyed on
	// the engram ID. assoc guards the association-list caches (forward keyed
	// on source, reverse keyed on destination); it is a separate array so
	// the Hebbian worker's frequent edge writes never reject engram fills.
	eng   [guardStripes]atomic.Uint64
	assoc [guardStripes]atomic.Uint64
}

const guardStripes = 4096 // power of two; 64 KiB for both arrays

// noFillToken marks a read whose freshness lower bound is unknown (a snapshot
// pinned without NewSnapshotContext). No fill from such a read is installed.
const noFillToken = ^uint64(0)

func stripeOf(id [16]byte) int {
	// The ULID's trailing bytes are random; the leading ones are a timestamp.
	return int(binary.LittleEndian.Uint16(id[14:16]) & (guardStripes - 1))
}

// token is R1.
func (g *fillGuard) token() uint64 { return g.clock.Load() }

func bumpStripe(s *atomic.Uint64, v uint64) {
	for {
		cur := s.Load()
		if cur >= v || s.CompareAndSwap(cur, v) {
			return
		}
	}
}

// bumpEngram / bumpAssoc are W2 for one key.
func (g *fillGuard) bumpEngram(id [16]byte) { bumpStripe(&g.eng[stripeOf(id)], g.clock.Add(1)) }
func (g *fillGuard) bumpAssoc(id [16]byte)  { bumpStripe(&g.assoc[stripeOf(id)], g.clock.Add(1)) }

// bumpAll is W2 for a range write (vault clear): every stripe.
func (g *fillGuard) bumpAll() {
	v := g.clock.Add(1)
	for i := range g.eng {
		bumpStripe(&g.eng[i], v)
		bumpStripe(&g.assoc[i], v)
	}
}

// engramClean / assocClean are the R3/R4 test: true when no write to the
// key's stripe has been bumped past t.
func (g *fillGuard) engramClean(id [16]byte, t uint64) bool {
	return t != noFillToken && g.eng[stripeOf(id)].Load() <= t
}

func (g *fillGuard) assocClean(id [16]byte, t uint64) bool {
	return t != noFillToken && g.assoc[stripeOf(id)].Load() <= t
}
