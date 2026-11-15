package hnsw

import (
	"math"
	"sort"
)

// Neighbour selection. ONE rule, used both when a node picks its own
// neighbours at insert and when a neighbour's list overflows and is pruned.
//
// The rule is the diversity heuristic of Malkov & Yashunin (2018, Algorithm 4)
// with the paper's keepPrunedConnections back-fill:
//
//  1. Walk the candidates nearest first. Keep a candidate c only if it is
//     nearer to the base than to every neighbour already kept; otherwise c is
//     reachable through that neighbour and is set aside.
//  2. If fewer than `keep` survive, top the list up with the set-aside
//     candidates, nearest first, so lists stay full.
//
// Why both halves, and both sites: keeping the `keep` NEAREST at either site
// lets any group of more than M0 mutual nearest neighbours end with no edge
// leaving the group — a closed island a search can enter and never leave.
// Measured on synthetic corpora (decision record, "closed islands"): diversity
// at the node's own pick only left ~40-member topics at recall@10 0.52, and
// diversity without the back-fill thinned layer-0 lists to 16–20 and dropped
// recall to 0.90.
//
// One addition to the paper, for exact duplicates: a candidate whose vector is
// bit-identical to a kept neighbour is set aside too. With identical vectors
// every distance in step 1 ties, the strict test keeps them all, and 33
// identical vectors would close exactly as nearest-only selection does.
//
// All distances here are computed by nodeCos (cached norms), base-to-candidate
// included, so the comparisons in step 1 are between numbers produced by the
// same arithmetic — a duplicate of the base and the base itself compare equal.

// vecNorm is the L2 norm nodeCos divides by, cached on each node when its
// vector is set (vectors are immutable after that).
func vecNorm(v []float32) float32 {
	var s float32
	for _, x := range v {
		s += x * x
	}
	return float32(math.Sqrt(float64(s)))
}

// nodeCos is cosine similarity using cached norms. Symmetric bit for bit:
// nodeCos(a, b) == nodeCos(b, a). Mismatched lengths or a zero vector score 0,
// as CosineSimilarity does.
func nodeCos(a, b *HNSWNode) float32 {
	x, y := a.vec, b.vec
	if len(x) != len(y) || len(x) == 0 || a.norm == 0 || b.norm == 0 {
		return 0
	}
	var dot float32
	i := 0
	for ; i+3 < len(x); i += 4 {
		dot += x[i]*y[i] + x[i+1]*y[i+1] + x[i+2]*y[i+2] + x[i+3]*y[i+3]
	}
	for ; i < len(x); i++ {
		dot += x[i] * y[i]
	}
	return dot / (a.norm * b.norm)
}

func sameVector(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type selCand struct {
	id   [16]byte
	node *HNSWNode
	dist float64
}

// IDENTICAL VECTORS. Among bit-identical vectors every distance ties, so any
// tie order sends every list to the same few copies and the rest get no
// in-edges (measured: 100 copies in a vault, 55 of them found at k = 100; a
// vault of nothing but 100 copies, 99 nodes closed). At layer 0 a group of
// identical vectors is therefore linked as a RING in id order: each member's
// list starts with its id-predecessor and id-successor in the group, and no
// other member of its own group competes for its remaining slots, which go to
// the ordinary selection over outsiders. Every member then has an in-edge
// from both ring neighbours (the group is strongly connected) and out-edges
// leaving the group — no member can be unreachable or closed — and a beam
// that enters the group walks it. The ring is a contents rule only: lists are
// the same format and size (≤ M0), so the on-disk shape is unchanged. Group
// membership is derived from the vectors themselves (dupGroups), at load and
// on insert; nothing about it is persisted.

// vecHash keys a vector's exact bit pattern for dupGroups (collisions are
// harmless: members are confirmed with sameVector).
func vecHash(v []float32) uint64 {
	h := uint64(14695981039346656037)
	for _, x := range v {
		b := math.Float32bits(x)
		for i := 0; i < 4; i++ {
			h ^= uint64(byte(b >> (8 * i)))
			h *= 1099511628211
		}
	}
	return h
}

// dupGroups indexes the ids of nodes by exact vector, so a node's identical
// peers are known without a search. Guarded by idx.mu.
type dupGroups map[uint64][][16]byte

func (g dupGroups) add(id [16]byte, v []float32) {
	if len(v) == 0 {
		return
	}
	h := vecHash(v)
	ids := g[h]
	i := sort.Search(len(ids), func(i int) bool { return string(ids[i][:]) >= string(id[:]) })
	if i < len(ids) && ids[i] == id {
		return
	}
	ids = append(ids, [16]byte{})
	copy(ids[i+1:], ids[i:])
	ids[i] = id
	g[h] = ids
}

func (g dupGroups) remove(id [16]byte, v []float32) {
	if len(v) == 0 {
		return
	}
	h := vecHash(v)
	ids := g[h]
	for i := range ids {
		if ids[i] == id {
			ids = append(ids[:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(g, h)
	} else {
		g[h] = ids
	}
}

// buildDupGroups indexes every node with a vector.
func buildDupGroups(nodes map[[16]byte]*HNSWNode) dupGroups {
	g := dupGroups{}
	for id, n := range nodes {
		g.add(id, n.vec)
	}
	return g
}

// identicalPeers returns base's bit-identical peers in id order, base
// included, or nil when base has none. Caller holds idx.mu.
func (idx *Index) identicalPeers(base *HNSWNode) [][16]byte {
	if idx.legacy || idx.dups == nil || len(base.vec) == 0 {
		return nil
	}
	ids := idx.dups[vecHash(base.vec)]
	if len(ids) < 2 {
		return nil
	}
	var out [][16]byte
	for _, id := range ids {
		if id == base.id {
			out = append(out, id)
			continue
		}
		if n := idx.nodes[id]; n != nil && sameVector(n.vec, base.vec) {
			out = append(out, id)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// ringOf returns base's ring neighbours (id-predecessor, id-successor, with
// wrap-around) among its identical peers.
func ringOf(peers [][16]byte, self [16]byte) [][16]byte {
	n := len(peers)
	i := 0
	for i < n && peers[i] != self {
		i++
	}
	if i == n || n < 2 {
		return nil
	}
	pred, succ := peers[(i-1+n)%n], peers[(i+1)%n]
	if pred == succ {
		return [][16]byte{succ}
	}
	return [][16]byte{pred, succ}
}

// selectNeighbors returns at most keep ids from ids for base's list at one
// layer. protect, when present in ids, is kept first and counts as a kept
// neighbour (#620: a just-linked node's only in-edge must survive the prune it
// triggered). base's own id is never selected. Ids whose node or vector is
// missing are never selected: a search cannot expand them, so a slot spent on
// one is a slot taken from a navigable neighbour. Caller holds idx.mu (write).
func (idx *Index) selectNeighbors(base *HNSWNode, ids [][16]byte, keep int, protect [16]byte, layer int) [][16]byte {
	var ring [][16]byte
	var peer map[[16]byte]bool
	if layer == 0 {
		if peers := idx.identicalPeers(base); peers != nil {
			ring = ringOf(peers, base.id)
			peer = make(map[[16]byte]bool, len(peers))
			for _, p := range peers {
				peer[p] = true
			}
		}
	}
	cands := make([]selCand, 0, len(ids))
	var prot *selCand
	for _, id := range ids {
		if id == base.id || peer[id] {
			continue
		}
		n := idx.nodes[id]
		if id == protect {
			prot = &selCand{id: id, node: n}
			continue
		}
		if n == nil || len(n.vec) == 0 {
			continue
		}
		cands = append(cands, selCand{id: id, node: n, dist: 1 - float64(nodeCos(base, n))})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].dist != cands[j].dist {
			return cands[i].dist < cands[j].dist
		}
		return string(cands[i].id[:]) < string(cands[j].id[:])
	})

	out := make([][16]byte, 0, keep)
	kept := make([]*HNSWNode, 0, keep)
	for _, id := range ring {
		if len(out) < keep {
			out = append(out, id)
		}
	}
	if prot != nil && len(out) < keep {
		out = append(out, prot.id)
		if prot.node != nil && len(prot.node.vec) > 0 {
			kept = append(kept, prot.node)
		}
	}
	var setAside [][16]byte
	for _, c := range cands {
		if len(out) >= keep {
			break
		}
		diverse := true
		for _, s := range kept {
			if 1-float64(nodeCos(c.node, s)) < c.dist || sameVector(c.node.vec, s.vec) {
				diverse = false
				break
			}
		}
		if diverse {
			out = append(out, c.id)
			kept = append(kept, c.node)
		} else {
			setAside = append(setAside, c.id)
		}
	}
	for _, id := range setAside {
		if len(out) >= keep {
			break
		}
		out = append(out, id)
	}
	return out
}
