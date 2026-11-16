package hnsw

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Loading a persisted graph, and repairing one that cannot be served as is.
//
// Persisted neighbour lists (0x07, layers 0..L) can be unfit to serve for
// three reasons, all detected structurally from the lists themselves — the
// on-disk format carries no version and needs none:
//
//   - the entry point reaches fewer than half the vectors (a degenerate apex
//     observed in production: an entry whose every neighbour was itself);
//   - some vectors are unreachable from the entry (#620 orphans: every
//     in-edge pruned away after they were persisted), or have no layer-0 list
//     at all (their lists were never persisted);
//   - some linked vectors cannot REACH the entry: they sit in a closed island,
//     a set of nodes with no edge leaving it, which a search that enters can
//     never leave. Graphs built by the pre-fix nearest-only selection have
//     them on any clustered data.
//
// THRESHOLDS (measured on synthetic corpora, decision record "closed
// islands"). A single closed node triggers a FULL rebuild: graphs built by
// select.go's rule had 0 closed nodes in every build measured (10 shapes,
// 400–16,500 vectors, dims 64–384); pre-fix graphs of clustered data had at
// least 79 of 2,000 and typically 46–93% of all nodes. Re-linking only
// the closed nodes was tried and does not converge — the pre-fix lists around
// them close again (measured: 79 closed → 82 after the re-link, every boot) —
// so an island is always repaired by rebuilding. A full rebuild also runs when
// the entry reaches fewer than half the vectors. Otherwise unreachable or
// unlinked vectors are re-linked by re-inserting just those (id order):
// proportional to the damage. Neither repair can repeat on the next boot:
// its result is made to satisfy exactly what this assessment checks — no
// closed and no unreachable vector — by ensureNavigable (repair.go), which
// adds escape edges if re-insertion alone left any. (Before identical vectors
// were ringed, 40–200 bit-identical copies did leave a rebuilt graph broken
// and it was repaired again on every boot.) Layer keys with no vector are
// leftovers, ignored and deleted here, never adopted as nodes.
//
// Either repair runs on a SCRATCH copy, never on the graph being served:
// searches keep using the loaded graph, unchanged, until the repaired graph
// replaces it in one step (repair.go). That swap is the only moment results
// change.

// repairKind is what a loaded graph needs before it is fit to serve.
type repairKind int

const (
	repairNone repairKind = iota
	repairPartial
	repairFull
)

func (k repairKind) String() string {
	switch k {
	case repairPartial:
		return "partial"
	case repairFull:
		return "full"
	}
	return "none"
}

// repairPlan is the outcome of assessing a graph.
type repairPlan struct {
	kind        repairKind
	vectors     int        // nodes carrying a vector
	unreachable int        // vectors the entry cannot reach (forward)
	closed      int        // vectors that cannot reach the entry (closed islands)
	targets     [][16]byte // partial: the vectors to re-link, id order
	nodes       int        // all nodes, vector or not
	reachable   int        // nodes reachable from the entry (all, as logged before)
	legacyRules bool       // assessed with the trial arm's pre-fix triggers
	entry       [16]byte   // entry point of the assessed graph
}

// assessGraph decides what nodes (with this entry) needs. Pure: reads the
// lists, mutates nothing.
func assessGraph(nodes map[[16]byte]*HNSWNode, entry [16]byte, legacy bool) repairPlan {
	p := repairPlan{nodes: len(nodes), legacyRules: legacy, entry: entry}
	for _, n := range nodes {
		if len(n.vec) > 0 {
			p.vectors++
		}
	}
	fwd := bfsReachableSet(nodes, entry)
	p.reachable = len(fwd)
	if p.vectors <= 1 {
		return p
	}
	var unreachable [][16]byte
	for id, n := range nodes {
		if len(n.vec) > 0 && !fwd[id] {
			unreachable = append(unreachable, id)
		}
	}
	p.unreachable = len(unreachable)
	if legacy {
		// The pre-fix load checks, for the trial arm: forward reach only.
		switch {
		case p.reachable*2 < p.vectors:
			p.kind = repairFull
		case len(unreachable) > 0:
			p.kind, p.targets = repairPartial, sortIDs(unreachable)
		}
		return p
	}
	// A node with a vector but no layer-0 list is unlinked: its lists were
	// never persisted. It is re-linked like an orphan, and is not counted as
	// an island member (it would otherwise make every edge into it look like
	// a way into an island).
	for id, n := range nodes {
		if len(n.vec) > 0 && fwd[id] && (len(n.layers) == 0 || len(n.layers[0]) == 0) && id != entry {
			unreachable = append(unreachable, id)
		}
	}
	p.unreachable = len(unreachable)
	closedSet := closedNodes(nodes, entry)
	p.closed = len(closedSet)
	vecReach := p.vectors - p.unreachable
	switch {
	case vecReach*2 < p.vectors || p.closed > 0:
		p.kind = repairFull
	case p.unreachable > 0:
		p.kind, p.targets = repairPartial, sortIDs(unreachable)
	}
	return p
}

func sortIDs(ids [][16]byte) [][16]byte {
	sort.Slice(ids, func(i, j int) bool { return string(ids[i][:]) < string(ids[j][:]) })
	return ids
}

// closedNodes returns the LINKED nodes (a vector and a layer-0 list) from which no path of
// layer-0 edges leads to the entry point: a reverse breadth-first search from
// the entry over edges between nodes with vectors (a search cannot expand a
// node whose vector is missing, so such a node is not a way out). O(N + E).
func closedNodes(nodes map[[16]byte]*HNSWNode, entry [16]byte) map[[16]byte]bool {
	rev := make(map[[16]byte][][16]byte, len(nodes))
	for id, n := range nodes {
		if len(n.vec) == 0 || len(n.layers) == 0 {
			continue
		}
		for _, nb := range n.layers[0] {
			if m := nodes[nb]; m != nil && len(m.vec) > 0 {
				rev[nb] = append(rev[nb], id)
			}
		}
	}
	reaches := map[[16]byte]bool{}
	if e := nodes[entry]; e != nil && len(e.vec) > 0 {
		reaches[entry] = true
		q := [][16]byte{entry}
		for len(q) > 0 {
			cur := q[0]
			q = q[1:]
			for _, p := range rev[cur] {
				if !reaches[p] {
					reaches[p] = true
					q = append(q, p)
				}
			}
		}
	}
	out := map[[16]byte]bool{}
	for id, n := range nodes {
		if len(n.vec) > 0 && len(n.layers) > 0 && len(n.layers[0]) > 0 && !reaches[id] {
			out[id] = true
		}
	}
	return out
}

// LoadFromPebble reads this vault's graph from Pebble and, when it is unfit
// to serve, repairs it before returning — synchronously, for direct callers.
// The registry does not use it: it serves the loaded graph at once and
// repairs in the background (Registry.getOrCreate).
func (idx *Index) LoadFromPebble() error {
	plan, err := idx.load()
	if err != nil {
		return err
	}
	if plan.kind != repairNone {
		idx.runRepair(context.Background(), plan, nil)
	}
	return nil
}

// load reads the persisted graph into the index, replacing what it held, and
// assesses it. Loads into temporary structures first and applies them only on
// success.
func (idx *Index) load() (repairPlan, error) {
	if idx.loadErrHook != nil {
		return repairPlan{}, idx.loadErrHook()
	}

	start := time.Now()

	// Scope the iterator to this vault's keyspace. The 0x07 range is shared by
	// all vaults; bounding it to 0x07‖ws … 0x07‖(ws+1) means Pebble only visits
	// this vault's keys instead of scanning the whole 0x07 keyspace and skipping
	// foreign keys per-key. The in-loop ws check below remains as a safety net.
	lowerBound := append([]byte{0x07}, idx.ws[:]...)
	upperBound := []byte{0x08}
	if wsPlus, err := keys.IncrementWSPrefix(idx.ws); err == nil {
		upperBound = append([]byte{0x07}, wsPlus[:]...)
	}

	iter, err := idx.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return repairPlan{}, err
	}
	defer iter.Close()

	tempNodes := make(map[[16]byte]*HNSWNode)
	tempMaxLevel := 0
	tempEntryPoint := [16]byte{}

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < 26 {
			continue
		}

		// Only load nodes belonging to this index's vault. The 0x07 range is
		// shared by all vaults; without this filter every per-vault index loads
		// every vault's nodes — bloating memory and surfacing cross-vault IDs
		// that can never resolve in the querying vault.
		if [8]byte(key[1:9]) != idx.ws {
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
		return repairPlan{}, fmt.Errorf("hnsw: LoadFromPebble incomplete read: %w", err)
	}
	for _, node := range tempNodes {
		node.norm = vecNorm(node.vec)
	}
	leftovers := 0
	if !idx.legacy {
		// Layer keys with no vector are leftovers — written back into a
		// cleared vault, or left by an insert whose vector was rolled back.
		// They are never adopted (a vector-less entry point answers every
		// search with nothing) and are deleted.
		var b *pebble.Batch
		for id, node := range tempNodes {
			if len(node.vec) > 0 {
				continue
			}
			if b == nil {
				b = idx.db.NewBatch()
			}
			for l := range node.layers {
				_ = b.Delete(keys.HNSWNodeKey(idx.ws, id, uint8(l)), nil)
			}
			delete(tempNodes, id)
			leftovers++
		}
		if b != nil {
			if err := b.Commit(pebble.NoSync); err != nil {
				slog.Warn("hnsw: failed to delete vector-less leftover keys", "vault", idx.ws, "error", err)
			}
			_ = b.Close()
			slog.Warn("hnsw: ignored and deleted vector-less leftover graph keys", "vault", idx.ws, "nodes", leftovers)
		}
		// The entry point follows from the nodes alone (canonicalEntry): the
		// rule Insert applies live, so a reload starts every search where the
		// live graph did.
		tempEntryPoint, tempMaxLevel = [16]byte{}, 0
		if e, l, ok := canonicalEntry(tempNodes); ok {
			tempEntryPoint, tempMaxLevel = e, l
		}
	}

	// Derive the vault's dimension from the vector with the smallest ID —
	// deterministic across calls and restarts (map iteration is random), and
	// consistent with the storage layer's first-embedding-key derivation.
	tempDim := 0
	var dimID [16]byte
	for id, node := range tempNodes {
		if node.vec == nil {
			continue
		}
		if tempDim == 0 || string(id[:]) < string(dimID[:]) {
			tempDim = len(node.vec)
			dimID = id
		}
	}

	plan := assessGraph(tempNodes, tempEntryPoint, idx.legacy)

	var dups dupGroups
	if !idx.legacy {
		dups = buildDupGroups(tempNodes)
	}
	idx.mu.Lock()
	idx.nodes = tempNodes
	idx.maxLevel = tempMaxLevel
	idx.entryPoint = tempEntryPoint
	idx.dim = tempDim
	idx.dups = dups
	idx.mu.Unlock()

	slog.Info("hnsw: loaded graph from pebble",
		"vault", idx.ws,
		"nodes", plan.nodes,
		"vectors", plan.vectors,
		"duration", time.Since(start),
		"reachable_from_entry", plan.reachable,
		"closed_nodes", plan.closed,
		"repair", plan.kind.String(),
	)
	return plan, nil
}
