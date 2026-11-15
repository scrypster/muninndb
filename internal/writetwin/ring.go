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

// RingTotalBytes caps the ring's vectors across ALL vaults. Per-vault caps
// alone are not a bound: any write auto-creates a vault, and 300 vaults x 512
// bge-small entries measured +255.6 MB. Past this cap the globally oldest
// entries go first.
const RingTotalBytes = 16 << 20

// entryOverhead approximates an entry's non-vector bytes (struct, slice
// header, the vault slot) for the byte accounting.
const entryOverhead = 96

func entryBytes(e Entry) int { return len(e.Vec)*4 + entryOverhead }

// sweepEvery bounds how often an Insert sweeps every vault for entries past
// the window (the engine worker also sweeps on a ticker, so an idle process
// still empties).
const sweepEvery = time.Minute

// Ring holds, per vault, at most RingCap entries seen within Window, and at
// most RingTotalBytes across vaults. It is safe for concurrent use. Lookups
// are O(RingCap) — a brute-force scan of ≤512 dot products is microseconds and
// needs no index.
type Ring struct {
	mu        sync.Mutex
	vaults    map[[8]byte]*vaultRing
	now       func() time.Time
	total     int // bytes held across vaults
	byteCap   int
	lastSweep time.Time
	// OnEvict, when set, is told how many entries left and why ("age",
	// "vault_cap", "total_cap"). Called with the ring locked: keep it cheap.
	OnEvict func(reason string, n int)
}

type vaultRing struct {
	entries []Entry // insertion order, oldest first
}

// NewRing returns an empty ring. now may be nil (time.Now).
func NewRing(now func() time.Time) *Ring {
	if now == nil {
		now = time.Now
	}
	return &Ring{vaults: make(map[[8]byte]*vaultRing), now: now, byteCap: RingTotalBytes}
}

// SetByteCap overrides RingTotalBytes (tests).
func (r *Ring) SetByteCap(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byteCap = n
}

// Bytes reports the bytes the ring's entries hold across vaults.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// Vaults reports how many vaults hold at least one entry.
func (r *Ring) Vaults() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vaults)
}

// Sweep drops every entry past the window in every vault, and every vault
// left empty. The engine worker calls it periodically.
func (r *Ring) Sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(r.now())
}

func (r *Ring) sweepLocked(now time.Time) {
	r.lastSweep = now
	for ws, vr := range r.vaults {
		r.evictVaultLocked(ws, vr, now)
	}
}

func (r *Ring) evicted(reason string, n int) {
	if n > 0 && r.OnEvict != nil {
		r.OnEvict(reason, n)
	}
}

// evictVaultLocked drops vr's entries past the window or the per-vault cap,
// and the vault itself once empty.
func (r *Ring) evictVaultLocked(ws [8]byte, vr *vaultRing, now time.Time) {
	age, capped := 0, 0
	cut := 0
	for cut < len(vr.entries) {
		e := vr.entries[cut]
		switch {
		case now.Sub(e.Seen) > Window:
			age++
		case len(vr.entries)-cut > RingCap:
			capped++
		default:
			goto done
		}
		r.total -= entryBytes(e)
		cut++
	}
done:
	if cut > 0 {
		vr.entries = append(vr.entries[:0], vr.entries[cut:]...)
	}
	if len(vr.entries) == 0 {
		delete(r.vaults, ws)
	}
	r.evicted("age", age)
	r.evicted("vault_cap", capped)
}

// enforceTotalLocked evicts the globally oldest entries until the ring is
// back under its byte cap.
func (r *Ring) enforceTotalLocked() {
	n := 0
	for r.total > r.byteCap && len(r.vaults) > 0 {
		var oldWS [8]byte
		var oldest *vaultRing
		for ws, vr := range r.vaults {
			if oldest == nil || vr.entries[0].Seen.Before(oldest.entries[0].Seen) {
				oldWS, oldest = ws, vr
			}
		}
		r.total -= entryBytes(oldest.entries[0])
		oldest.entries = append(oldest.entries[:0], oldest.entries[1:]...)
		if len(oldest.entries) == 0 {
			delete(r.vaults, oldWS)
		}
		n++
	}
	r.evicted("total_cap", n)
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
			r.total -= entryBytes(vr.entries[i])
			vr.entries = append(vr.entries[:i], vr.entries[i+1:]...)
			break
		}
	}
	vr.entries = append(vr.entries, e)
	r.total += entryBytes(e)
	r.evictVaultLocked(ws, vr, now)
	if now.Sub(r.lastSweep) >= sweepEvery {
		r.sweepLocked(now) // lazily, from any vault's write
	}
	r.enforceTotalLocked()
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
	if vr := r.vaults[ws]; vr != nil {
		for _, e := range vr.entries {
			r.total -= entryBytes(e)
		}
	}
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
