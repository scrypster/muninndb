package hnsw

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/rankorder"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const (
	M              = 16
	M0             = 32
	EfConstruction = 200
	EfSearch       = 50
	MlFactor       = 1.0 / math.Ln2 // 1/ln(M) where M=16 but simplified
)

// ScoredID is a search result with a similarity score.
type ScoredID struct {
	ID    [16]byte
	Score float64
}

// HNSWNode is an in-memory node in the HNSW graph.
type HNSWNode struct {
	id     [16]byte
	vec    []float32    // in-memory vector cache; set once on Insert, never mutated
	norm   float32      // vecNorm(vec), set with vec; used by neighbour selection
	layers [][][16]byte // layers[l] = neighbor list at layer l
	mu     sync.RWMutex
}

func (n *HNSWNode) getLayer(l int) [][16]byte {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if l >= len(n.layers) {
		return nil
	}
	result := make([][16]byte, len(n.layers[l]))
	copy(result, n.layers[l])
	return result
}

func (n *HNSWNode) setLayer(l int, neighbors [][16]byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for len(n.layers) <= l {
		n.layers = append(n.layers, nil)
	}
	n.layers[l] = neighbors
}

func (n *HNSWNode) maxLayer() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.layers) - 1
}

// Index is the HNSW vector index.
type Index struct {
	mu             sync.RWMutex
	nodes          map[[16]byte]*HNSWNode
	entryPoint     [16]byte
	maxLevel       int
	db             *pebble.DB
	ws             [8]byte
	rng            *rand.Rand
	rngMu          sync.Mutex
	persistWg      sync.WaitGroup // tracks in-flight persistNode goroutines
	efConstruction int            // beam width during insert; 0 → uses EfConstruction constant
	efSearch       int            // beam width during query; 0 → uses EfSearch constant
	deleted        sync.Map       // key: [16]byte id → struct{} (tombstoned hard-deleted nodes)

	// loadErrHook is a test-only seam: when non-nil, LoadFromPebble returns the
	// hook's result immediately, allowing tests to exercise the failed-load path
	// (e.g. the registry's no-cache-on-error behaviour) without corrupting Pebble.
	// Always nil in production.
	loadErrHook func() error

	// dim is the vault's established vector dimension; 0 until the first
	// vector is inserted or loaded. On load it is taken from the vector with
	// the smallest ID, so it is deterministic per process AND across restarts
	// (and agrees with the storage layer's first-key derivation) even for a
	// legacy vault that already holds mixed dimensions. Guarded by mu.
	dim int

	// loadErr is set by the registry when LoadFromPebble failed for this
	// (uncached) index, so writes can refuse instead of treating the vault as
	// empty — inserting into a vault whose real dimension is unknown could
	// recreate the #582 split. Set once before the index escapes getOrCreate.
	loadErr error

	// legacy selects the pre-fix neighbour-selection and entry-point rules
	// (nearest-only lists; see select.go). Set once in New from the
	// build-tag-only trial switch — constant false in every shipped build — and
	// by in-package tests that measure the old rule against the new one.
	legacy bool

	// noRetiredBeamTrigger disables SearchLive's "walked through ef retired
	// nodes" exact-pass trigger. Test-only measurement seam (the trigger's
	// value on the diverse graph is re-measured by the harness); never set
	// outside in-package tests.
	noRetiredBeamTrigger bool

	// repair tracks this index's background graph repair (repair.go).
	repair repairState
}

// Dim returns the vault's established vector dimension.
// Returns 0 if the index is empty (no vectors inserted yet).
func (idx *Index) Dim() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.dim
}

// LoadError reports the load failure recorded for this index, if any.
func (idx *Index) LoadError() error {
	return idx.loadErr
}

// establishDim atomically checks a vector's dimension against the vault's
// established dimension, establishing it on first use — check and
// establishment happen under one lock, so two concurrent first inserts with
// different dimensions cannot both pass (one establishes, the other is
// refused). The established dimension deliberately survives the deletion of
// the vector that set it: refusing until a reload/reset is safer than letting
// the dimension flap.
func (idx *Index) establishDim(n int) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.dim == 0 {
		idx.dim = n
		return nil
	}
	if idx.dim != n {
		return &DimMismatchError{Got: n, Want: idx.dim}
	}
	return nil
}

// Tombstone marks a node as deleted so it is skipped in future Search results.
// The node's memory is reclaimed on the next full index rebuild.
func (idx *Index) Tombstone(id [16]byte) {
	idx.deleted.Store(id, struct{}{})
}

// IsTombstoned reports whether the node has been tombstoned. Read-only; exists
// so a caller (and STO-12's cascade tests) can assert the hard-delete cleanup
// actually reached the vector index rather than inferring it from a Search that
// a zero-vector fixture would answer the same way either.
func (idx *Index) IsTombstoned(id [16]byte) bool {
	_, ok := idx.deleted.Load(id)
	return ok
}

// efC returns the effective EfConstruction for this index.
// Allows per-index override (e.g., lower value during bulk eval loading).
func (idx *Index) efC() int {
	if idx.efConstruction > 0 {
		return idx.efConstruction
	}
	return EfConstruction
}

// efS returns the effective EfSearch for this index.
// Allows per-index override (e.g., higher value for larger corpora).
func (idx *Index) efS() int {
	if idx.efSearch > 0 {
		return idx.efSearch
	}
	return EfSearch
}

func New(db *pebble.DB, ws [8]byte) *Index {
	// Seed the per-index RNG from the workspace prefix so that level
	// assignment is deterministic for a given vault — enabling reproducible
	// graph construction and reliable test behaviour.
	seed := int64(binary.BigEndian.Uint64(ws[:]))
	return &Index{
		nodes:  make(map[[16]byte]*HNSWNode),
		db:     db,
		ws:     ws,
		rng:    rand.New(rand.NewSource(seed)),
		legacy: legacySelectArm(),
	}
}

// NewWithEfConstruction creates a new Index with a custom EfConstruction.
// Use a lower value (e.g., 50) for bulk eval loading to trade graph quality
// for speed. Note: EfConstruction affects graph build quality but EfSearch
// (query-time beam width) is the dominant factor for retrieval recall at scale.
func NewWithEfConstruction(db *pebble.DB, ws [8]byte, efC int) *Index {
	idx := New(db, ws)
	idx.efConstruction = efC
	return idx
}

// NewWithParams creates a new Index with custom EfConstruction and EfSearch.
// Use for eval configurations that need explicit control over both build and
// query beam widths (e.g., efC=200, efSearch=200 for large-corpus eval).
func NewWithParams(db *pebble.DB, ws [8]byte, efC, efS int) *Index {
	idx := New(db, ws)
	idx.efConstruction = efC
	idx.efSearch = efS
	return idx
}

// Len returns the number of nodes in the index.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.nodes)
}

// VectorBytes returns the total size in bytes of all in-memory vectors.
func (idx *Index) VectorBytes() int64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var total int64
	for _, n := range idx.nodes {
		total += int64(len(n.vec)) * 4 // float32 = 4 bytes
	}
	return total
}

// CosineSimilarity computes cosine similarity between two vectors.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	i := 0
	for ; i+3 < len(a); i += 4 {
		dot += a[i]*b[i] + a[i+1]*b[i+1] + a[i+2]*b[i+2] + a[i+3]*b[i+3]
		na += a[i]*a[i] + a[i+1]*a[i+1] + a[i+2]*a[i+2] + a[i+3]*a[i+3]
		nb += b[i]*b[i] + b[i+1]*b[i+1] + b[i+2]*b[i+2] + b[i+3]*b[i+3]
	}
	for ; i < len(a); i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb))))
}

func (idx *Index) randomLevel() int {
	idx.rngMu.Lock()
	defer idx.rngMu.Unlock()
	level := 0
	for idx.rng.Float64() < (1.0/float64(M)) && level < 16 {
		level++
	}
	return level
}

func maxConnections(layer int) int {
	if layer == 0 {
		return M0
	}
	return M
}

type candidate struct {
	id   [16]byte
	dist float64
}

// Search finds the k nearest neighbors to the query vector.
func (idx *Index) Search(ctx context.Context, query []float32, k int) ([]ScoredID, error) {
	return idx.search(query, k, nil), nil
}

// search is Search; when dist is non-nil it is incremented once per distance
// computed, so a measurement can report the cost of a query.
func (idx *Index) search(query []float32, k int, dist *int) []ScoredID {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.nodes) == 0 {
		return nil
	}

	ep := idx.entryPoint
	epNode := idx.nodes[ep]
	if epNode == nil || len(epNode.vec) == 0 {
		return nil
	}
	count := func() {
		if dist != nil {
			*dist++
		}
	}
	epVec := epNode.vec
	epDist := 1.0 - float64(CosineSimilarity(query, epVec))
	count()

	// Phase 1: greedy descent through upper layers
	for l := idx.maxLevel; l > 0; l-- {
		node := idx.nodes[ep]
		if node == nil {
			break
		}
		changed := true
		for changed {
			changed = false
			for _, nID := range node.getLayer(l) {
				nNode := idx.nodes[nID]
				if nNode == nil || len(nNode.vec) == 0 {
					continue
				}
				nDist := 1.0 - float64(CosineSimilarity(query, nNode.vec))
				count()
				if nDist < epDist {
					ep = nID
					epDist = nDist
					node = idx.nodes[ep]
					changed = true
				}
			}
		}
	}

	// Phase 2: beam search at layer 0
	ef := idx.efS()
	if k > ef {
		ef = k
	}

	type heapItem struct {
		id   [16]byte
		dist float64
	}

	// minHeap for candidates (explore nearest first)
	candidates := []heapItem{{id: ep, dist: epDist}}
	// maxHeap for visited (keep ef best)
	visited := []heapItem{{id: ep, dist: epDist}}
	seen := map[[16]byte]bool{ep: true}

	// Track visited max distance to avoid rescanning on every iteration (D4).
	visitedMax := epDist

	for len(candidates) > 0 {
		// Pop min from candidates
		minIdx := 0
		for i := 1; i < len(candidates); i++ {
			if candidates[i].dist < candidates[minIdx].dist {
				minIdx = i
			}
		}
		curr := candidates[minIdx]
		candidates[minIdx] = candidates[len(candidates)-1]
		candidates = candidates[:len(candidates)-1]

		if curr.dist > visitedMax {
			break
		}

		node := idx.nodes[curr.id]
		if node == nil {
			continue
		}

		for _, nID := range node.getLayer(0) {
			if seen[nID] {
				continue
			}
			seen[nID] = true

			nNode := idx.nodes[nID]
			if nNode == nil || len(nNode.vec) == 0 {
				continue
			}
			nDist := 1.0 - float64(CosineSimilarity(query, nNode.vec))
			count()

			if nDist < visitedMax || len(visited) < ef {
				candidates = append(candidates, heapItem{nID, nDist})

				// Prune candidates if it grows beyond ef*2 to prevent O(n) growth
				// in large, highly connected graphs. Remove the farthest candidate.
				if len(candidates) > ef*2 {
					maxCIdx := 0
					for i := 1; i < len(candidates); i++ {
						if candidates[i].dist > candidates[maxCIdx].dist {
							maxCIdx = i
						}
					}
					candidates[maxCIdx] = candidates[len(candidates)-1]
					candidates = candidates[:len(candidates)-1]
				}

				visited = append(visited, heapItem{nID, nDist})
				if nDist > visitedMax {
					visitedMax = nDist
				}

				// Remove max if over ef. Among exactly tied farthest
				// entries the OLDEST is evicted (rankorder: newest-first is
				// what survives a cut).
				if len(visited) > ef {
					maxIdx := 0
					for i := 1; i < len(visited); i++ {
						if visited[i].dist > visited[maxIdx].dist ||
							(visited[i].dist == visited[maxIdx].dist && rankorder.Before(visited[maxIdx].id, visited[i].id)) {
							maxIdx = i
						}
					}
					visited[maxIdx] = visited[len(visited)-1]
					visited = visited[:len(visited)-1]
					// Must rescan to find new max after removal.
					visitedMax = 0
					for _, v := range visited {
						if v.dist > visitedMax {
							visitedMax = v.dist
						}
					}
				}
			}
		}
	}

	// Sort visited by distance and take top-k (D1: sort.Slice replaces insertion sort).
	// Exact distance ties break newest-first (rankorder.Before) — recall's one
	// tie rule, applied at this cut because it runs before the final sort.
	sort.Slice(visited, func(i, j int) bool {
		if visited[i].dist != visited[j].dist {
			return visited[i].dist < visited[j].dist
		}
		return rankorder.Before(visited[i].id, visited[j].id)
	})

	results := make([]ScoredID, 0, k)
	for _, v := range visited {
		if len(results) >= k {
			break
		}
		// Skip tombstoned nodes (hard-deleted engrams).
		if _, tombstoned := idx.deleted.Load(v.id); tombstoned {
			continue
		}
		results = append(results, ScoredID{
			ID:    v.id,
			Score: 1.0 - v.dist,
		})
	}
	return results
}

// Insert adds a vector to the HNSW index.
// This is safe for concurrent use but should be called from the async indexing worker.
func (idx *Index) Insert(id [16]byte, vector []float32) {
	idx.insert(id, vector, true)
}

// insert is Insert; copyVec false lets a repair's scratch graph share the
// served graph's (immutable) vectors instead of duplicating every one.
func (idx *Index) insert(id [16]byte, vector []float32, copyVec bool) {
	level := idx.randomLevel()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	cachedVec := vector
	if copyVec {
		cachedVec = make([]float32, len(vector))
		copy(cachedVec, vector)
	}
	node := &HNSWNode{
		id:     id,
		vec:    cachedVec,
		norm:   vecNorm(cachedVec),
		layers: make([][][16]byte, level+1),
	}
	idx.nodes[id] = node

	if len(idx.nodes) == 1 {
		idx.entryPoint = id
		idx.maxLevel = level
		idx.persistAsync(id, node)
		return
	}

	// Traverse from the EXISTING entry point and link the new node into the
	// graph BEFORE any entry-point promotion. Promoting first would make
	// ep == id below: searchLayer then sees only the new node itself, the node
	// links to nothing (or to itself), and every promotion orphans the entire
	// prior graph — fragmenting the index a little more on each maxLevel raise.
	ep := idx.entryPoint
	oldMaxLevel := idx.maxLevel
	var epVec []float32
	if epNode := idx.nodes[ep]; epNode != nil {
		epVec = epNode.vec
	}

	// Phase 1: greedy descent from oldMaxLevel to level+1 (only if epVec exists)
	if epVec != nil {
		for l := oldMaxLevel; l > level; l-- {
			epVec = idx.greedyDescend(ep, epVec, vector, l, &ep)
		}
	}

	// Neighbors whose layer lists we mutate must be re-persisted, or the
	// back-edges exist only in memory and every restart sheds them — leaving
	// the on-disk graph forward-only and barely navigable after reload.
	mutated := make(map[[16]byte]*HNSWNode)

	// Phase 2: insert at each layer from min(level, oldMaxLevel) down to 0
	for l := min(level, oldMaxLevel); l >= 0; l-- {
		found := idx.searchLayer(ep, vector, idx.efC(), l)
		maxConn := maxConnections(l)
		var chosen [][16]byte
		if idx.legacy {
			if len(found) > maxConn {
				found = found[:maxConn]
			}
			chosen = make([][16]byte, len(found))
			for i, nb := range found {
				chosen[i] = nb.id
			}
		} else {
			ids := make([][16]byte, len(found))
			for i, nb := range found {
				ids[i] = nb.id
			}
			chosen = idx.selectNeighbors(node, ids, maxConn, [16]byte{})
		}
		node.layers[l] = chosen

		// Add bidirectional connections
		for _, nbID := range chosen {
			nbNode := idx.nodes[nbID]
			if nbNode == nil {
				continue
			}
			nbNode.mu.Lock()
			for len(nbNode.layers) <= l {
				nbNode.layers = append(nbNode.layers, nil)
			}
			nbNode.layers[l] = append(nbNode.layers[l], id)
			if len(nbNode.layers[l]) > maxConn {
				// Prune with the same diversity rule as the node's own pick
				// (select.go) — keeping the maxConn NEAREST here is what closed
				// islands of more than M0 mutual nearest neighbours — and always
				// retain the just-appended edge. The new node's only path into
				// the graph is a back-edge from one of these neighbors; in a
				// dense region every neighbor's list is already full of
				// mutually-closer nodes, so a prune that could drop the fresh
				// edge would drop it at every neighbor and the node would be
				// born with zero in-edges — permanently invisible to graph
				// search, since future inserts can only discover neighbors by
				// traversing the graph (#620).
				if idx.legacy {
					nbNode.layers[l] = idx.pruneNeighbors(nbNode.vec, nbNode.layers[l], maxConn, id)
				} else {
					nbNode.layers[l] = idx.selectNeighbors(nbNode, nbNode.layers[l], maxConn, id)
				}
			}
			nbNode.mu.Unlock()
			mutated[nbID] = nbNode
		}

		// The next layer's search starts from the nearest candidate found,
		// whichever neighbours were kept.
		if len(found) > 0 {
			ep = found[0].id
		}
	}

	// Promote to entry point only after the node is linked into the graph.
	// The entry point is the SMALLEST id at the top level — the same rule
	// LoadFromPebble applies — so a reloaded graph starts every search where
	// the live graph did (the legacy arm keeps "first to reach the level").
	if level > idx.maxLevel || (!idx.legacy && level == idx.maxLevel && string(id[:]) < string(idx.entryPoint[:])) {
		idx.entryPoint = id
		idx.maxLevel = level
	}

	idx.persistAsync(id, node)
	for nbID, nbNode := range mutated {
		idx.persistAsync(nbID, nbNode)
	}
}

// persistAsync writes a node's lists in the background (tracked by
// persistWg). An index without a database — a rebuild's scratch graph — keeps
// its lists in memory only; they reach Pebble in one batch at the swap.
func (idx *Index) persistAsync(id [16]byte, node *HNSWNode) {
	if idx.db == nil {
		return
	}
	idx.persistWg.Add(1)
	go idx.persistNode(id, node)
}

// pruneNeighbors is the LEGACY back-link prune (the trial arm's control; see
// select.go for the shipped rule). It keeps the keep nearest neighbor ids to baseVec, measured by
// cosine distance. Neighbors whose node or vector is missing sort last so they
// are pruned first. The protect id, when present in ids, is always retained
// (it takes one of the keep slots): connectivity of a just-linked node beats
// the marginal distance optimality of the edge it displaces. Pass the zero
// value for no protection — engram ids are ULIDs, which are never zero.
// Caller must hold the owning node's mutex; neighbor vectors are immutable
// after insert, so reading them without their locks is safe.
func (idx *Index) pruneNeighbors(baseVec []float32, ids [][16]byte, keep int, protect [16]byte) [][16]byte {
	if len(ids) <= keep {
		return ids
	}
	type nd struct {
		id   [16]byte
		dist float64
	}
	scored := make([]nd, 0, len(ids))
	for _, nid := range ids {
		if nid == protect {
			scored = append(scored, nd{nid, -1.0}) // always sorts first
			continue
		}
		n := idx.nodes[nid]
		if n == nil || len(n.vec) == 0 || len(baseVec) == 0 {
			scored = append(scored, nd{nid, 2.0}) // missing vector: prune first
			continue
		}
		scored = append(scored, nd{nid, 1.0 - float64(CosineSimilarity(baseVec, n.vec))})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].dist < scored[j].dist })
	out := make([][16]byte, keep)
	for i := 0; i < keep; i++ {
		out[i] = scored[i].id
	}
	return out
}

func (idx *Index) greedyDescend(ep [16]byte, epVec, query []float32, l int, newEP *[16]byte) []float32 {
	epDist := 1.0 - float64(CosineSimilarity(query, epVec))
	changed := true
	for changed {
		changed = false
		node := idx.nodes[ep]
		if node == nil {
			break
		}
		for _, nID := range node.getLayer(l) {
			nNode := idx.nodes[nID]
			if nNode == nil || len(nNode.vec) == 0 {
				continue
			}
			nDist := 1.0 - float64(CosineSimilarity(query, nNode.vec))
			if nDist < epDist {
				ep = nID
				epDist = nDist
				epVec = nNode.vec
				*newEP = ep
				changed = true
			}
		}
	}
	return epVec
}

func (idx *Index) searchLayer(ep [16]byte, query []float32, ef, l int) []candidate {
	epNode := idx.nodes[ep]
	if epNode == nil || len(epNode.vec) == 0 {
		return nil
	}
	epDist := 1.0 - float64(CosineSimilarity(query, epNode.vec))

	candidates := []candidate{{ep, epDist}}
	visited := []candidate{{ep, epDist}}
	seen := map[[16]byte]bool{ep: true}

	// Track visited max distance to avoid rescanning on every iteration (D4).
	visitedMax := epDist

	for len(candidates) > 0 {
		minIdx := 0
		for i := 1; i < len(candidates); i++ {
			if candidates[i].dist < candidates[minIdx].dist {
				minIdx = i
			}
		}
		curr := candidates[minIdx]
		candidates[minIdx] = candidates[len(candidates)-1]
		candidates = candidates[:len(candidates)-1]

		if curr.dist > visitedMax {
			break
		}

		node := idx.nodes[curr.id]
		if node == nil {
			continue
		}

		for _, nID := range node.getLayer(l) {
			if seen[nID] {
				continue
			}
			seen[nID] = true

			nNode := idx.nodes[nID]
			if nNode == nil || len(nNode.vec) == 0 {
				continue
			}
			nDist := 1.0 - float64(CosineSimilarity(query, nNode.vec))

			if nDist < visitedMax || len(visited) < ef {
				candidates = append(candidates, candidate{nID, nDist})
				visited = append(visited, candidate{nID, nDist})
				if nDist > visitedMax {
					visitedMax = nDist
				}
				if len(visited) > ef {
					maxIdx := 0
					for i := 1; i < len(visited); i++ {
						if visited[i].dist > visited[maxIdx].dist {
							maxIdx = i
						}
					}
					visited[maxIdx] = visited[len(visited)-1]
					visited = visited[:len(visited)-1]
					// Must rescan to find new max after removal.
					visitedMax = 0
					for _, v := range visited {
						if v.dist > visitedMax {
							visitedMax = v.dist
						}
					}
				}
			}
		}
	}

	// Sort by distance ascending (D1: sort.Slice replaces insertion sort).
	sort.Slice(visited, func(i, j int) bool { return visited[i].dist < visited[j].dist })
	return visited
}

// StoreVector persists a vector to Pebble for later retrieval.
func (idx *Index) StoreVector(id [16]byte, vec []float32) error {
	key := keys.HNSWNodeKey(idx.ws, id, 0xFF)
	return idx.db.Set(key, encodeVector(vec), pebble.NoSync)
}

// DeleteVector removes a previously stored vector from Pebble.
// Used to clean up orphaned vectors when graph insertion fails after storage succeeds.
func (idx *Index) DeleteVector(id [16]byte) error {
	key := keys.HNSWNodeKey(idx.ws, id, 0xFF)
	return idx.db.Delete(key, pebble.NoSync)
}

func encodeVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.BigEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func decodeVector(buf []byte) []float32 {
	if len(buf)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.BigEndian.Uint32(buf[i*4:]))
	}
	return vec
}

// Close waits for all in-flight persistNode goroutines to finish.
// Call before closing the underlying Pebble DB to prevent "pebble: closed" panics.
func (idx *Index) Close() {
	idx.persistWg.Wait()
}

// persistNode writes a node's neighbor lists to Pebble.
func (idx *Index) persistNode(id [16]byte, node *HNSWNode) {
	defer idx.persistWg.Done()
	node.mu.RLock()
	defer node.mu.RUnlock()

	for l, neighbors := range node.layers {
		key := keys.HNSWNodeKey(idx.ws, id, uint8(l))
		val := encodeNeighbors(neighbors)
		if err := idx.db.Set(key, val, pebble.NoSync); err != nil {
			slog.Warn("hnsw: failed to persist node", "error", err)
		}
	}
}

func encodeNeighbors(neighbors [][16]byte) []byte {
	buf := make([]byte, 2+len(neighbors)*16)
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(neighbors)))
	for i, nb := range neighbors {
		copy(buf[2+i*16:], nb[:])
	}
	return buf
}

func decodeNeighbors(buf []byte) [][16]byte {
	if len(buf) < 2 {
		return nil
	}
	count := int(binary.BigEndian.Uint16(buf[0:2]))
	buf = buf[2:]
	if len(buf) < count*16 {
		return nil
	}
	result := make([][16]byte, count)
	for i := range result {
		copy(result[i][:], buf[i*16:])
	}
	return result
}

// canonicalEntry returns the smallest id at the highest level among nodes
// that carry a vector; ok is false when no node has both a vector and a list.
func canonicalEntry(nodes map[[16]byte]*HNSWNode) (entry [16]byte, level int, ok bool) {
	level = -1
	for id, n := range nodes {
		if len(n.vec) == 0 || len(n.layers) == 0 {
			continue
		}
		l := len(n.layers) - 1
		if !ok || l > level || (l == level && string(id[:]) < string(entry[:])) {
			entry, level, ok = id, l, true
		}
	}
	return entry, level, ok
}

// bfsReachable counts the nodes reachable from the entry point over layer-0
// edges. A healthy graph reaches ~all nodes; a small count on a populated graph
// indicates a disconnected (e.g. self-looped) entry point.
func bfsReachable(nodes map[[16]byte]*HNSWNode, entry [16]byte) int {
	return len(bfsReachableSet(nodes, entry))
}

// bfsReachableSet returns the set of node ids reachable from the entry point
// over layer-0 edges.
func bfsReachableSet(nodes map[[16]byte]*HNSWNode, entry [16]byte) map[[16]byte]bool {
	if nodes[entry] == nil {
		return nil
	}
	seen := map[[16]byte]bool{entry: true}
	queue := [][16]byte{entry}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		n := nodes[cur]
		if n == nil || len(n.layers) == 0 {
			continue
		}
		for _, nb := range n.layers[0] {
			if !seen[nb] && nodes[nb] != nil {
				seen[nb] = true
				queue = append(queue, nb)
			}
		}
	}
	return seen
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
