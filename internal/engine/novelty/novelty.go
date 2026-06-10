package novelty

import (
	"sort"
	"sync"

	"github.com/scrypster/muninndb/internal/index/fts"
)

const (
	TopTerms  = 30   // terms per fingerprint
	Threshold = 0.70 // Jaccard similarity threshold
	CacheSize = 1000 // LRU capacity per vault (uint32 key)
	NumShards = 16   // shards to reduce lock contention
)

// Fingerprint is a set of the top TopTerms terms from an engram.
type Fingerprint struct {
	terms map[string]bool
}

// BuildFingerprint extracts the top-N terms from text (concept + " " + content).
// Uses term frequency since we don't have per-engram IDF at this call site.
// Returns a fingerprint with up to TopTerms unique terms.
func BuildFingerprint(concept, content string) Fingerprint {
	// Combine concept and content
	combined := concept + " " + content
	tokens := fts.Tokenize(combined)

	// Count term frequencies
	termFreq := make(map[string]int)
	for _, token := range tokens {
		termFreq[token]++
	}

	// Sort terms by frequency (descending)
	type termCount struct {
		term  string
		count int
	}
	var sorted []termCount
	for term, count := range termFreq {
		sorted = append(sorted, termCount{term, count})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	// Take top TopTerms
	fp := Fingerprint{
		terms: make(map[string]bool),
	}
	limit := TopTerms
	if len(sorted) < limit {
		limit = len(sorted)
	}
	for i := 0; i < limit; i++ {
		fp.terms[sorted[i].term] = true
	}

	return fp
}

// Jaccard returns the Jaccard similarity between two fingerprints.
// Returns 0.0 if either fingerprint is empty.
func Jaccard(a, b Fingerprint) float64 {
	if len(a.terms) == 0 || len(b.terms) == 0 {
		return 0.0
	}

	// Count intersection
	intersection := 0
	for term := range a.terms {
		if b.terms[term] {
			intersection++
		}
	}

	// Count union
	union := len(a.terms) + len(b.terms) - intersection

	if union == 0 {
		return 0.0
	}

	return float64(intersection) / float64(union)
}

// Match is returned when a near-duplicate is found.
type Match struct {
	ExistingULID string  // the ULID string of the similar existing engram
	Similarity   float64 // Jaccard similarity score
}

// entry is a cached fingerprint for one engram.
type entry struct {
	ulid        string
	fingerprint Fingerprint
	insertOrder int64 // for LRU eviction (monotonically increasing)
}

// vaultCache is a per-vault LRU of fingerprints.
type vaultCache struct {
	mu      sync.RWMutex
	entries map[string]*entry // key: ULID string
	counter int64             // monotonic insert counter
}

func newVaultCache() *vaultCache {
	return &vaultCache{
		entries: make(map[string]*entry),
		counter: 0,
	}
}

// Add adds a fingerprint to the cache, evicting oldest if over CacheSize.
func (c *vaultCache) Add(ulid string, fp Fingerprint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Always add the new entry first
	c.counter++
	c.entries[ulid] = &entry{
		ulid:        ulid,
		fingerprint: fp,
		insertOrder: c.counter,
	}

	// Evict if over capacity
	if len(c.entries) > CacheSize {
		// Find the entry with the lowest insertOrder
		var oldest *entry
		var oldestULID string
		for uid, ent := range c.entries {
			if oldest == nil || ent.insertOrder < oldest.insertOrder {
				oldest = ent
				oldestULID = uid
			}
		}
		if oldest != nil {
			delete(c.entries, oldestULID)
		}
	}
}

// FindSimilar scans the cache for any fingerprint with Jaccard >= Threshold.
// Returns the best match, or nil if none found.
func (c *vaultCache) FindSimilar(fp Fingerprint) *Match {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var bestMatch *Match
	bestSimilarity := 0.0

	for _, ent := range c.entries {
		sim := Jaccard(fp, ent.fingerprint)
		if sim >= Threshold && sim > bestSimilarity {
			bestSimilarity = sim
			bestMatch = &Match{
				ExistingULID: ent.ulid,
				Similarity:   sim,
			}
		}
	}

	return bestMatch
}

// shard holds a set of per-vault caches.
type shard struct {
	mu     sync.RWMutex
	vaults map[uint32]*vaultCache // key: vaultID (uint32)
}

// Detector is the novelty detection engine.
// It is safe for concurrent use.
type Detector struct {
	shards [NumShards]*shard
}

// New creates a new Detector.
func New() *Detector {
	d := &Detector{}
	for i := 0; i < NumShards; i++ {
		d.shards[i] = &shard{
			vaults: make(map[uint32]*vaultCache),
		}
	}
	return d
}

// shardFor returns the shard for the given vaultID.
func (d *Detector) shardFor(vaultID uint32) *shard {
	return d.shards[vaultID%uint32(NumShards)]
}

// getOrCreateVault returns the vaultCache for the given vaultID within a shard.
// Must be called with shard.mu held (write lock).
func (s *shard) getOrCreateVault(vaultID uint32) *vaultCache {
	vc, ok := s.vaults[vaultID]
	if !ok {
		vc = newVaultCache()
		s.vaults[vaultID] = vc
	}
	return vc
}

// PurgeVault removes all cached fingerprint data for the given vaultID from every
// shard. Call this after a vault clear or delete to release memory and prevent
// stale fingerprints from influencing novelty detection on future writes.
func (d *Detector) PurgeVault(vaultID uint32) {
	for i := range d.shards {
		d.shards[i].mu.Lock()
		delete(d.shards[i].vaults, vaultID)
		d.shards[i].mu.Unlock()
	}
}

// Check computes the fingerprint for the given engram text and checks for near-duplicates.
// Returns a Match if a similar engram exists, nil otherwise.
// After checking, adds the fingerprint to the cache for future comparisons.
// ulidStr is the ULID string of the newly written engram (for caching).
func (d *Detector) Check(vaultID uint32, ulidStr, concept, content string) *Match {
	// Step 1: Compute fingerprint
	fp := BuildFingerprint(concept, content)

	// Step 2: Get shard
	shard := d.shardFor(vaultID)

	// Step 3: Get vault cache (write lock for potential creation)
	shard.mu.Lock()
	vc := shard.getOrCreateVault(vaultID)
	shard.mu.Unlock()

	// Step 4: FindSimilar (read lock on vault cache)
	match := vc.FindSimilar(fp)

	// Step 5: Add the new entry (write lock on vault cache)
	vc.Add(ulidStr, fp)

	return match
}
