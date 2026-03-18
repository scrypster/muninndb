package hnsw

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"sync"

	"github.com/cockroachdb/pebble"
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
}

// Dim returns the vector dimension used by this index.
// Returns 0 if the index is empty (no vectors inserted yet).
func (idx *Index) Dim() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for _, node := range idx.nodes {
		return len(node.vec)
	}
	return 0
}

// Tombstone marks a node as deleted so it is skipped in future Search results.
// The node's memory is reclaimed on the next full index rebuild.
func (idx *Index) Tombstone(id [16]byte) {
	idx.deleted.Store(id, struct{}{})
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
		nodes: make(map[[16]byte]*HNSWNode),
		db:    db,
		ws:    ws,
		rng:   rand.New(rand.NewSource(seed)),
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
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.nodes) == 0 {
		return nil, nil
	}

	ep := idx.entryPoint
	epNode := idx.nodes[ep]
	if epNode == nil || len(epNode.vec) == 0 {
		return nil, nil
	}
	epVec := epNode.vec
	epDist := 1.0 - float64(CosineSimilarity(query, epVec))

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

				// Remove max if over ef
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

	// Sort visited by distance and take top-k (D1: sort.Slice replaces insertion sort).
	sort.Slice(visited, func(i, j int) bool { return visited[i].dist < visited[j].dist })

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
	return results, nil
}

// Insert adds a vector to the HNSW index.
// This is safe for concurrent use but should be called from the async indexing worker.
func (idx *Index) Insert(id [16]byte, vector []float32) {
	level := idx.randomLevel()

	idx.mu.Lock()
	defer idx.mu.Unlock()

	cachedVec := make([]float32, len(vector))
	copy(cachedVec, vector)
	node := &HNSWNode{
		id:     id,
		vec:    cachedVec,
		layers: make([][][16]byte, level+1),
	}
	idx.nodes[id] = node

	if len(idx.nodes) == 1 {
		idx.entryPoint = id
		idx.maxLevel = level
		idx.persistWg.Add(1)
		go idx.persistNode(id, node)
		return
	}

	if level > idx.maxLevel {
		idx.entryPoint = id
		idx.maxLevel = level
	}

	ep := idx.entryPoint
	var epVec []float32
	if epNode := idx.nodes[ep]; epNode != nil {
		epVec = epNode.vec
	}

	// Phase 1: greedy descent from maxLevel to level+1 (only if epVec exists)
	if epVec != nil {
		for l := idx.maxLevel; l > level; l-- {
			epVec = idx.greedyDescend(ep, epVec, vector, l, &ep)
		}
	}

	// Phase 2: insert at each layer from min(level, maxLevel) down to 0
	for l := min(level, idx.maxLevel); l >= 0; l-- {
		neighbors := idx.searchLayer(ep, vector, idx.efC(), l)
		M := M
		if l == 0 {
			M = M0
		}
		if len(neighbors) > M {
			neighbors = neighbors[:M]
		}

		node.layers[l] = make([][16]byte, len(neighbors))
		for i, nb := range neighbors {
			node.layers[l][i] = nb.id
		}

		// Add bidirectional connections
		for _, nb := range neighbors {
			nbNode := idx.nodes[nb.id]
			if nbNode == nil {
				continue
			}
			nbNode.mu.Lock()
			for len(nbNode.layers) <= l {
				nbNode.layers = append(nbNode.layers, nil)
			}
			nbNode.layers[l] = append(nbNode.layers[l], id)
			maxConn := maxConnections(l)
			if len(nbNode.layers[l]) > maxConn {
				// Prune: keep strongest connections
				// Simple pruning: truncate (production would use heuristic select)
				nbNode.layers[l] = nbNode.layers[l][:maxConn]
			}
			nbNode.mu.Unlock()
		}

		if len(neighbors) > 0 {
			ep = neighbors[0].id
		}
	}

	idx.persistWg.Add(1)
	go idx.persistNode(id, node)
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

// LoadFromPebble reads all HNSW nodes from Pebble into memory.
// Loads into temporary structures first and only applies on success to maintain consistency.
func (idx *Index) LoadFromPebble() error {
	lowerBound := []byte{0x07}
	upperBound := []byte{0x08}

	iter, err := idx.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	// Load into temporary structures
	tempNodes := make(map[[16]byte]*HNSWNode)
	tempMaxLevel := 0
	tempEntryPoint := [16]byte{}

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < 26 {
			continue
		}

		var id [16]byte
		copy(id[:], key[9:25])

		// Vector slot (0xFF): load into node.vec
		if key[25] == 0xFF {
			if _, ok := tempNodes[id]; !ok {
				tempNodes[id] = &HNSWNode{id: id}
			}
			tempNodes[id].vec = decodeVector(iter.Value())
			continue
		}

		layer := int(key[25])

		if _, ok := tempNodes[id]; !ok {
			tempNodes[id] = &HNSWNode{id: id}
		}
		node := tempNodes[id]
		node.setLayer(layer, decodeNeighbors(iter.Value()))

		if layer > tempMaxLevel {
			tempMaxLevel = layer
			tempEntryPoint = id
		} else if tempEntryPoint == ([16]byte{}) {
			// Ensure at least one node is always set as the entry point even
			// when all nodes are at layer 0 (where layer == maxLevel == 0).
			tempEntryPoint = id
		}
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("hnsw: LoadFromPebble incomplete read: %w", err)
	}

	// Only apply to index if load completed successfully
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.nodes = tempNodes
	idx.maxLevel = tempMaxLevel
	idx.entryPoint = tempEntryPoint

	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
