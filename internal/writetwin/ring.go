package writetwin

import (
	"bytes"
	"sort"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/index/hnsw"
)

// Window is how far back a recent twin may have been written.
const Window = time.Hour

// Entry is one recent write vector.
//
// Vec is the vector the retroactive embed processor computed and inserted
// into HNSW for this engram — the same passage plugin on the same text
// (Concept+" "+Content), in the same batch composition. Nothing here assumes
// a self-cosine of 1.0: the bundled int8 model embeds the same text slightly
// differently alone and inside a batch (cosine ≈0.9985–0.9994), which is why
// the ring never re-embeds, and only ever compares stored-path vectors.
type Entry struct {
	ID        [16]byte
	Vec       []float32
	CreatedAt time.Time // the engram's CreatedAt: age and the window use it
	Seen      time.Time // process clock when the vector landed: eviction uses it
	BatchID   uint64    // non-zero for items of one WriteBatch call
}

// Match is a ring entry that clears the threshold against a probe.
type Match struct {
	ID         [16]byte
	Similarity float64
	CreatedAt  time.Time
	BatchID    uint64
}

// Ring holds, per vault, at most RingCap entries seen within Window. It is
// safe for concurrent use. All methods are O(RingCap) at worst — a brute-force
// scan of ≤512 dot products is microseconds and needs no index.
type Ring struct {
	mu     sync.Mutex
	vaults map[[8]byte]*vaultRing
	now    func() time.Time
}

type vaultRing struct {
	entries []Entry // insertion order, oldest first
}

// NewRing returns an empty ring. now may be nil (time.Now).
func NewRing(now func() time.Time) *Ring {
	if now == nil {
		now = time.Now
	}
	return &Ring{vaults: make(map[[8]byte]*vaultRing), now: now}
}

// Insert adds or replaces (by id) an entry for vault ws and evicts anything
// past Window or past RingCap. Vec is retained; callers hand over a copy.
func (r *Ring) Insert(ws [8]byte, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if e.Seen.IsZero() {
		e.Seen = now
	}
	vr := r.vaults[ws]
	if vr == nil {
		vr = &vaultRing{}
		r.vaults[ws] = vr
	}
	for i := range vr.entries {
		if vr.entries[i].ID == e.ID {
			// Same engram embedded again (re-embed, retry): keep one entry.
			vr.entries = append(vr.entries[:i], vr.entries[i+1:]...)
			break
		}
	}
	vr.entries = append(vr.entries, e)
	vr.evictLocked(now)
}

func (vr *vaultRing) evictLocked(now time.Time) {
	cut := 0
	for cut < len(vr.entries) && (now.Sub(vr.entries[cut].Seen) > Window || len(vr.entries)-cut > RingCap) {
		cut++
	}
	if cut > 0 {
		vr.entries = append(vr.entries[:0], vr.entries[cut:]...)
	}
}

// Get returns the entry for id in vault ws.
func (r *Ring) Get(ws [8]byte, id [16]byte) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if vr := r.vaults[ws]; vr != nil {
		for i := len(vr.entries) - 1; i >= 0; i-- {
			if vr.entries[i].ID == id {
				return vr.entries[i], true
			}
		}
	}
	return Entry{}, false
}

// Len reports how many entries vault ws holds (tests and metrics).
func (r *Ring) Len(ws [8]byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if vr := r.vaults[ws]; vr != nil {
		return len(vr.entries)
	}
	return 0
}

// ClearVault drops every entry of vault ws (vault clear / delete / re-embed).
func (r *Ring) ClearVault(ws [8]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.vaults, ws)
}

// older reports whether a was written before b: by CreatedAt, ties broken by
// ULID (monotonic within a millisecond).
func older(a, b Entry) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return bytes.Compare(a.ID[:], b.ID[:]) < 0
}

// Scan returns the entries of vault ws written BEFORE probe and within Window
// of it whose cosine with probe.Vec is at least t, most similar first (exact
// ties: newer first). considered is how many earlier in-window entries were
// compared. A probe never matches itself. sel picks the eligible entries:
// SelectRecent leaves a batch item's own batch-mates to the intra-batch rule;
// SelectBatch compares only those batch-mates.
func (r *Ring) Scan(ws [8]byte, probe Entry, t float64, sel Select) (matches []Match, considered int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vr := r.vaults[ws]
	if vr == nil || len(probe.Vec) == 0 {
		return nil, 0
	}
	for _, e := range vr.entries {
		if e.ID == probe.ID || !older(e, probe) {
			continue
		}
		if probe.CreatedAt.Sub(e.CreatedAt) > Window {
			continue
		}
		sameBatch := probe.BatchID != 0 && e.BatchID == probe.BatchID
		switch sel {
		case SelectRecent:
			if sameBatch {
				continue
			}
		case SelectBatch:
			if !sameBatch {
				continue
			}
		}
		if len(e.Vec) != len(probe.Vec) {
			continue // a different-dimension vector cannot be compared (#582)
		}
		considered++
		sim := float64(hnsw.CosineSimilarity(probe.Vec, e.Vec))
		if sim >= t {
			matches = append(matches, Match{ID: e.ID, Similarity: sim, CreatedAt: e.CreatedAt, BatchID: e.BatchID})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Similarity != matches[j].Similarity {
			return matches[i].Similarity > matches[j].Similarity
		}
		if !matches[i].CreatedAt.Equal(matches[j].CreatedAt) {
			return matches[i].CreatedAt.After(matches[j].CreatedAt)
		}
		return bytes.Compare(matches[i].ID[:], matches[j].ID[:]) > 0
	})
	return matches, considered
}

// Select chooses which earlier ring entries a Scan compares.
type Select int

const (
	// SelectRecent: every earlier in-window write except the probe's own
	// batch-mates.
	SelectRecent Select = iota
	// SelectBatch: only the probe's batch-mates (intra-batch twins).
	SelectBatch
)
