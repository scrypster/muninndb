package hnsw

import (
	"context"
	"math"
	"sort"

	"github.com/scrypster/muninndb/internal/rankorder"
)

// LiveSearchStats reports how much extra work SearchLive did beyond an
// ordinary beam search, so tests and measurements can see the bound hold.
type LiveSearchStats struct {
	// RetiredSeen counts retired nodes whose distance was computed.
	RetiredSeen int
	// RetiredExpanded counts retired nodes whose neighbour lists were followed
	// (used as routing nodes). Never exceeds RetiredBudget.
	RetiredExpanded int
	// RetiredBudget is the expansion budget this search was given.
	RetiredBudget int
	// ExactScan reports that the graph walk could not produce a full beam of
	// live hits and the search fell back to one exact pass over the index.
	ExactScan bool
	// Distances counts the query-to-node distances computed, descent and
	// exact pass included — the search's cost in the unit an exact scan of N
	// nodes costs N.
	Distances int
}

// SearchLive is Search for a vault whose index holds RETIRED vectors — soft-
// deleted or superseded engrams whose vectors stay in the graph on purpose
// (HNSW has no delete, and COG-28 needs an old version's vector so a query
// worded like it can still reach the current head).
//
// Search counts every node it meets toward its beam of ef results, so a dense
// cluster of retired versions near a query fills the beam and the live
// memories behind it never become candidates: a 50-version chain took 44 of
// 45 slots in a 2,000-memory vault and recall returned 0 of 20 relevant live
// memories. SearchLive changes exactly one thing: a node for which
// retired(id) is true (or that is tombstoned) is a ROUTING node — its
// neighbours are explored as usual — but it never occupies a result slot. The
// beam therefore widens until it holds ef LIVE hits or the graph is exhausted.
//
// Retired nodes are not discarded either. The nearest shadowBudget of them
// (tombstoned nodes excepted — a hard-deleted engram is gone) are returned
// separately as shadows: evidence the recall pipeline may resolve to a chain
// head, on a budget that is NOT counted against the live k.
//
// BOUNDS ON EXTRA WORK, both derived from the index rather than tuned:
//
//   - Retired expansions are capped at max(ef, N/M0), N = nodes in the
//     index. Each expansion costs at most M0 distance evaluations, so the
//     routing detour through retired nodes costs at most about one exact pass
//     over the index (N/M0 × M0 = N) — the price at which an exact scan
//     becomes the cheaper way to finish.
//   - If the walk routed through ef or more retired nodes, spent the retired
//     budget, or exhausted the reachable graph with fewer than ef live hits,
//     while nodes remain unvisited, the search makes ONE exact pass over the
//     unvisited nodes. A query landing in a
//     closed island of near-identical retired vectors (on graphs built before
//     the diverse selection rule, select.go; repaired at load) can otherwise
//     reach nothing live at all, and
//     a walk that ran out of budget inside a big retired region returns a
//     beam of distant live hits. Worst case for a whole search: ≈ 2N distance
//     evaluations plus the ordinary beam, i.e. never more than about two exact
//     scans of the vault; it is paid only by queries that spend the budget.
//
// With nothing retired encountered this walks the same path as Search;
// callers with an empty retired set should call Search itself (the engine
// does), so a vault with no retired versions is untouched by construction.
func (idx *Index) SearchLive(ctx context.Context, query []float32, k int, retired func([16]byte) bool, shadowBudget int) (live, shadows []ScoredID, stats LiveSearchStats, err error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.nodes) == 0 || k <= 0 {
		return nil, nil, stats, nil
	}
	ep := idx.entryPoint
	epNode := idx.nodes[ep]
	if epNode == nil || len(epNode.vec) == 0 {
		return nil, nil, stats, idx.entryError()
	}
	epDist := 1.0 - float64(CosineSimilarity(query, epNode.vec))
	stats.Distances++

	// Phase 1: greedy descent through the upper layers. Retired nodes route
	// here exactly as in Search — the descent only chooses where layer 0
	// starts, it fills no result slot.
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
				stats.Distances++
				if nDist < epDist {
					ep = nID
					epDist = nDist
					node = idx.nodes[ep]
					changed = true
				}
			}
		}
	}

	ef := idx.efS()
	if k > ef {
		ef = k
	}
	budget := len(idx.nodes) / M0
	if budget < ef {
		budget = ef
	}
	stats.RetiredBudget = budget

	tombstoned := func(id [16]byte) bool {
		_, t := idx.deleted.Load(id)
		return t
	}
	skip := func(id [16]byte) bool {
		return tombstoned(id) || (retired != nil && retired(id))
	}

	type item struct {
		id   [16]byte
		dist float64
	}
	// worse reports whether a ranks below b in the result order: farther, or
	// at an exact tie OLDER (rankorder: newest-first survives a cut — the same
	// rule Search applies at its eviction and its final sort).
	worse := func(a, b item) bool {
		if a.dist != b.dist {
			return a.dist > b.dist
		}
		return rankorder.Before(b.id, a.id)
	}

	// W: live results, at most ef. wMax is its farthest distance once full.
	var w []item
	wMax := math.Inf(1)
	recomputeWMax := func() {
		if len(w) < ef {
			wMax = math.Inf(1)
			return
		}
		wMax = 0
		for _, v := range w {
			if v.dist > wMax {
				wMax = v.dist
			}
		}
	}
	offerLive := func(it item) {
		w = append(w, it)
		if len(w) > ef {
			worst := 0
			for i := 1; i < len(w); i++ {
				if worse(w[i], w[worst]) {
					worst = i
				}
			}
			w[worst] = w[len(w)-1]
			w = w[:len(w)-1]
		}
		recomputeWMax()
	}
	// Shadow heap: nearest shadowBudget retired (non-tombstoned) nodes.
	var sh []item
	offerShadow := func(it item) {
		stats.RetiredSeen++
		if shadowBudget <= 0 || tombstoned(it.id) {
			return
		}
		sh = append(sh, it)
		if len(sh) > shadowBudget {
			worst := 0
			for i := 1; i < len(sh); i++ {
				if worse(sh[i], sh[worst]) {
					worst = i
				}
			}
			sh[worst] = sh[len(sh)-1]
			sh = sh[:len(sh)-1]
		}
	}

	candidates := []item{{id: ep, dist: epDist}}
	seen := map[[16]byte]bool{ep: true}
	if skip(ep) {
		offerShadow(item{ep, epDist})
	} else {
		offerLive(item{ep, epDist})
	}

	stoppedByDistance := false
	budgetSpent := false
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

		if len(w) >= ef && curr.dist > wMax {
			stoppedByDistance = true
			break
		}
		if skip(curr.id) {
			if stats.RetiredExpanded >= budget {
				budgetSpent = true
				continue
			}
			stats.RetiredExpanded++
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
			stats.Distances++
			isSkip := skip(nID)
			if isSkip {
				offerShadow(item{nID, nDist})
			}
			if len(w) < ef || nDist < wMax {
				candidates = append(candidates, item{nID, nDist})
				// Same bound on the frontier as Search: drop the farthest
				// candidate once it exceeds 2·ef.
				if len(candidates) > ef*2 {
					maxC := 0
					for i := 1; i < len(candidates); i++ {
						if candidates[i].dist > candidates[maxC].dist {
							maxC = i
						}
					}
					candidates[maxC] = candidates[len(candidates)-1]
					candidates = candidates[:len(candidates)-1]
				}
				if !isSkip {
					offerLive(item{nID, nDist})
				}
			}
		}
	}

	// Exact fallback, one pass over the unvisited nodes, when any of
	//   - the walk routed through at least ef retired nodes (a full beam's
	//     worth): the query landed in a retired region, and a beam filled from
	//     around it is made of distant live hits — the frontier cap (2·ef)
	//     drops candidates while the walk is inside the region, so it stops
	//     "by distance" on the wrong neighbourhood. Measured on a 12k index
	//     with ten 500-version chains, chain-aimed recall@10 against exact
	//     search over live vectors: 0.44 firing only on a short beam, 0.52
	//     adding budget exhaustion, 1.00 with this trigger (off-topic
	//     queries on that 42%-retired index: 0.84 → 0.92; the pass fired on
	//     81 of 200 of them, ~1.4 ms each);
	//   - the retired-routing budget ran out; or
	//   - the walk exhausted what it could reach with fewer than ef live hits
	//     (a closed island of near-duplicates).
	// Cost: one distance per unvisited node (≤ N), paid only by queries that
	// spent a beam on retired routing.
	//
	// Re-measured on the diverse graph (decision record, "closed islands"):
	// at 12k / dim 384 the beam-of-retired trigger bought no recall (25% and
	// 90% retired; ten 500-version chains) and forced the exact pass on every
	// query of a 90%-retired vault; on the 64-dim chain fixture
	// (TestSearchLive_ChainAimedRecallAgainstExact) it still lifts recall@10
	// from 0.956 to 1.000. Kept until its removal is its own evidenced change.
	beamOfRetired := stats.RetiredExpanded >= ef && !idx.noRetiredBeamTrigger
	if (budgetSpent || beamOfRetired || (!stoppedByDistance && len(w) < ef)) && len(seen) < len(idx.nodes) {
		stats.ExactScan = true
		n := 0
		for id, node := range idx.nodes {
			if seen[id] || node == nil || len(node.vec) == 0 {
				continue
			}
			n++
			if n%1024 == 0 && ctx.Err() != nil {
				return nil, nil, stats, ctx.Err()
			}
			d := 1.0 - float64(CosineSimilarity(query, node.vec))
			stats.Distances++
			if skip(id) {
				offerShadow(item{id, d})
			} else {
				offerLive(item{id, d})
			}
		}
	}

	sort.Slice(w, func(i, j int) bool { return worse(w[j], w[i]) })
	sort.Slice(sh, func(i, j int) bool { return worse(sh[j], sh[i]) })
	if len(w) > k {
		w = w[:k]
	}
	live = make([]ScoredID, len(w))
	for i, v := range w {
		live[i] = ScoredID{ID: v.id, Score: 1.0 - v.dist}
	}
	if len(sh) > 0 {
		shadows = make([]ScoredID, len(sh))
		for i, v := range sh {
			shadows[i] = ScoredID{ID: v.id, Score: 1.0 - v.dist}
		}
	}
	return live, shadows, stats, nil
}
