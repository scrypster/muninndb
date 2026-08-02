package engine

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// COG-29 — contradiction honesty (#764).
//
// When two results in a recall response are joined by an UNRESOLVED DECLARED
// `contradicts` edge, recall must not present either as the answer. Both are
// returned, demoted, in a defensible order, with a first-class per-row
// annotation, and the response carries a `conflict` block naming the pair and
// the resolution actions.
//
// The phase is DEMOTE-ONLY (it can never lift a row above a genuinely better
// unrelated match), NEVER removes a row, NEVER abstains, and WRITES NOTHING.
//
// Why not top-level abstention, which is what one evaluator literally asked
// for: #747/#754 is the hard-learned lesson in this exact area — suppressing
// BOTH sides of a declared contradiction destroys the true memory exactly as
// hard as the false one, and the agent that did the right thing by declaring
// the conflict loses access to both facts and to the conflict itself.
// Abstention also has a wire contract ("Empty iff Abstained is false",
// mbp/types.go) that two admission-worthy candidates do not satisfy. The
// complaint underneath the ask — "an agent commonly consumes only the first
// result, so this is confidently misleading" — is about presenting one side as
// THE ANSWER, not about returning the data. The demote, the always-on per-row
// annotation and the response-level block remove "the answer" while keeping
// both facts.
const (
	// contradictionDemote is the relative demote applied to every member of a
	// conflict cluster. It is a pairwise honesty nudge, not a tuned relevance
	// constant: it exists so a disputed fact does not sit at the score an
	// undisputed one would have earned.
	//
	// It is the ONLY score change this phase makes. An earlier draft also
	// carried an absolute 0.95 ceiling; it was removed because recall's wire
	// Score is an absolute activation, structurally well below 0.6 on every
	// corpus measured, so the ceiling never once bound — it shipped a
	// permanently-false `score_capped` flag and a vacuous test assertion. The
	// honest statement of the guarantee is the relative one: a disputed row is
	// returned BELOW the score it earned, and it is annotated on every surface.
	contradictionDemote = 0.10

	// contradictionMargin is added to MaxResults to decide how many top
	// (already score-sorted) rows to examine. Same rationale and same value as
	// supersessionMargin: rows below this cannot survive truncation, so paying
	// association reads for them is wasted hot-path I/O.
	contradictionMargin = supersessionMargin

	// contradictionAssocScan / contradictionReverseScan bound the forward and
	// reverse association reads per examined row. Generous enough that a
	// heavily-referenced engram's contradicts edge is not missed behind other
	// edges — a miss would silently present a disputed fact as the answer,
	// the exact failure this phase exists to prevent.
	contradictionAssocScan   = supersessionReverseScan
	contradictionReverseScan = supersessionReverseScan

	// contradictionMaxCluster caps how many mutually-conflicting rows are
	// treated as one atomic cluster. Beyond it the members are still demoted
	// and annotated, but cluster_truncated is set rather than the response
	// silently pretending the cluster was fully enumerated.
	contradictionMaxCluster = 8
)

// Sides of a declared contradiction, from one row's point of view.
const (
	contradictionSideAsserted   = "asserted"   // this row is the edge's SOURCE
	contradictionSideChallenged = "challenged" // this row is the edge's TARGET
)

// Bases for the intra-cluster ordering ladder. Presentation order only — never
// a verdict about which memory is true.
const (
	contradictionBasisValidFrom    = "newer_valid_from"
	contradictionBasisAsserting    = "asserting_side"
	contradictionBasisULIDTiebreak = "ulid_tiebreak"
)

// contradictionWarning is the response-level instruction. It names all three
// resolution actions in words, because the accepted residual of this phase is
// that a declared-and-abandoned conflict demotes both facts until someone
// resolves it — recoverable, but only if the caller is told how.
const contradictionWarning = "Two or more returned memories are declared to contradict each other and the conflict is unresolved. Neither is presented as the answer: both are returned, with their scores demoted below what they earned. Resolve it — muninn_evolve the memory that should survive, muninn_forget(not_true_since=…) the side that stopped being true, or muninn_link(relation=\"supersedes\") to declare which one wins."

// contradictionEdge is one declared contradicts edge between two engrams,
// carrying which side asserted it and when.
type contradictionEdge struct {
	src       storage.ULID // the asserting side (source of the 0x03 edge)
	dst       storage.ULID
	declared  time.Time // zero = unknown; never invented
	srcInSet  bool
	dstInSet  bool
	partnerOK bool // the out-of-set endpoint is live and nameable
}

func (ce contradictionEdge) other(id storage.ULID) storage.ULID {
	if ce.src == id {
		return ce.dst
	}
	return ce.src
}

// vaultMayHaveContradictions is COG-29's fast-path gate. The phase must be
// free on the overwhelming majority of vaults, which have no contradictions at
// all. Three probes, cheapest first:
//
//  1. The in-process per-vault flag. It covers the window between an explicit
//     muninn_link(contradicts) in THIS process and the batch worker writing the
//     0x0A marker (≤ one worker interval after #764 D1), so a single-binary
//     deployment — the default, and the evaluators' case — honors a declaration
//     on the very next query.
//
//  2. A bounded 0x0A seek, answered by one iterator First(). This is the steady
//     state, and it is the only probe that sees an edge declared by ANOTHER
//     process, so it must keep running on every query.
//
//  3. Once per vault per process, a bounded scan for declared-but-unflagged
//     contradicts edges. This exists because probes 1 and 2 TOGETHER have a
//     permanent hole: the in-process flag lives only in memory and the 0x0A
//     marker is written only when the batch worker flushes, so a process that
//     restarts between muninn_link(contradicts) and that flush loses the flag
//     while the marker was never written — and nothing else ever re-probes.
//     Recall would then silently stop honoring a durable, correctly-declared
//     contradiction FOREVER. The scan's result is cached per vault, so a
//     contradiction-free vault pays it exactly once, not once per recall.
//
// Named residual: the once-per-process scan is capped
// (storage.DefaultDeclaredContradictionScanCap). On a vault whose forward
// associations exceed that cap the scan is incomplete, and this gate then
// degrades toward DOING the work rather than toward silence — the phase is
// read-only and bounded, so the cost of being wrong that way is I/O, while the
// cost of being wrong the other way is a disputed fact presented as the answer.
func (e *Engine) vaultMayHaveContradictions(ctx context.Context, ws [8]byte) bool {
	if _, declared := e.contradictionsDeclared.Load(ws); declared {
		return true
	}
	has, err := e.store.HasContradictionMarkers(ctx, ws)
	if err != nil {
		// Degrade toward DOING the work: a read error must not silently turn
		// contradiction honesty off. The phase itself is read-only, so the
		// worst case is wasted I/O on one query.
		slog.Warn("recall: contradiction marker probe failed, running the phase anyway", "err", err)
		return true
	}
	if has {
		return true
	}
	return e.declaredContradictionsProbe(ctx, ws)
}

// declaredContradictionsProbe runs the once-per-process declared-edge scan
// described in vaultMayHaveContradictions, memoising its verdict per vault.
func (e *Engine) declaredContradictionsProbe(ctx context.Context, ws [8]byte) bool {
	if _, done := e.contradictionProbeClean.Load(ws); done {
		return false
	}
	res, err := e.store.DeclaredContradictions(ctx, ws, 0)
	if err != nil {
		slog.Warn("recall: contradiction declared-edge probe failed, running the phase anyway", "err", err)
		return true
	}
	if len(res.Records) > 0 {
		// Promote to the sticky flag: the vault demonstrably has a declared
		// contradiction, so never pay this scan again.
		e.contradictionsDeclared.Store(ws, struct{}{})
		return true
	}
	if !res.Complete {
		slog.Warn("recall: contradiction declared-edge probe hit its scan cap; running COG-29 on every query for this vault",
			"scanned", res.Scanned)
		e.contradictionsDeclared.Store(ws, struct{}{})
		return true
	}
	e.contradictionProbeClean.Store(ws, struct{}{})
	return false
}

// noteContradictionDeclared records that this process has written a
// RelContradicts edge in ws, so the COG-29 fast-path gate stops skipping the
// vault before the batch detector's marker exists.
//
// Deliberately never evicted. Eviction would have to prove that NO unresolved
// declared contradiction remains anywhere in the vault, which is a vault-wide
// scan — exactly the cost this gate exists to avoid — and getting it wrong
// turns honesty off silently, the failure class this whole increment is about.
// The entry is an empty struct keyed by an 8-byte prefix, one per vault this
// process has ever seen a declaration in, so the retained set is bounded by
// vault count and measured in bytes. The cost of the false positive is that
// COG-29 keeps running on a vault whose conflicts were all resolved; the phase
// is read-only, bounded, and returns no block when nothing is live.
func (e *Engine) noteContradictionDeclared(ws [8]byte) {
	e.contradictionsDeclared.Store(ws, struct{}{})
	e.contradictionProbeClean.Delete(ws)
}

// applyContradictionHonesty is COG-29. See the block comment above for the
// contract. It returns the (possibly reordered) results, the response-level
// conflict block (nil when there is no live conflict), and keepAtLeast — the
// minimum number of rows the caller's MaxResults truncation must keep so that
// no conflict cluster is cut in half.
//
// Runs after applyCurrencyAnnotation and before truncation. That slot is
// load-bearing on four counts: after the COG-19 validity gate, so a side
// already removed for being invalid or soft-deleted is neither resurrected nor
// referenced (this is what makes evolve/forget stop the theater); after
// supersession/substitution, because a declared RelSupersedes between the pair
// IS the resolution and by then the loser has already been demoted or removed;
// after currency, because COG-25 may reorder exact ties and COG-29's ordering
// must be the last word; and before truncation, so a demoted partner is not
// silently cut.
//
// Zero writes: forward and reverse association reads, engram and metadata
// reads, and lease reads through the shared visibility gate. Observe-safe
// (COG-11) by construction.
func (e *Engine) applyContradictionHonesty(
	ctx context.Context,
	ws [8]byte,
	results []activation.ScoredEngram,
	req *activation.ActivateRequest,
	gate *visibilityGate,
	now time.Time,
) (out []activation.ScoredEngram, conflict *mbp.ConflictBlock, keepAtLeast int) {
	if len(results) == 0 {
		return results, nil, 0
	}
	if !e.vaultMayHaveContradictions(ctx, ws) {
		return results, nil, 0
	}

	// Step 1 — examine window. Results are already score-descending.
	window := len(results)
	if req.MaxResults > 0 && window > req.MaxResults+contradictionMargin {
		window = req.MaxResults + contradictionMargin
	}
	idx := make(map[storage.ULID]int, window)
	ids := make([]storage.ULID, 0, window)
	for i := 0; i < window; i++ {
		idx[results[i].Engram.ID] = i
		ids = append(ids, results[i].Engram.ID)
	}

	edges := e.collectContradictionEdges(ctx, ws, ids, idx)
	if len(edges) == 0 {
		return results, nil, 0
	}

	// Step 3 — the unresolved test. Partner liveness is resolved once, in a
	// batch, over the distinct endpoints that are NOT already in the result
	// set (a row in the set cleared phase 6's admission by construction).
	partners := e.resolveContradictionPartners(ctx, ws, edges, idx, gate)

	// Endpoint liveness, applied to BOTH sides regardless of whether they are
	// in the result set. "In the set" is NOT sufficient: under
	// include_invalid (and under a lineage-admitting as-of view) a retired
	// endpoint is returned, and treating its presence as proof of liveness
	// would keep the theater running after exactly the resolution the warning
	// told the caller to perform. The rule is the same one
	// markResolvedContradictions applies to the report, so the two surfaces
	// cannot disagree about whether a conflict is live.
	endpointLive := func(id storage.ULID) bool {
		if i, inSet := idx[id]; inSet {
			return contradictionEndpointLive(results[i].Engram, req, now)
		}
		p, ok := partners[id]
		return ok && p.live
	}

	live := edges[:0:0]
	for _, ce := range edges {
		// Clause 4 — existed at the caller's instant. A conflict declared
		// AFTER the as-of instant is not part of the truth of that time. A
		// zero stamp is a legacy edge: treat it as always-existing and show
		// the conflict — never invent a time, and over-warn beats under-warn
		// on a correctness signal.
		if req.AsOf != nil && !ce.declared.IsZero() && ce.declared.After(*req.AsOf) {
			continue
		}
		// Clause 2 — both endpoints live under the caller's view.
		if !endpointLive(ce.src) || !endpointLive(ce.dst) {
			continue
		}
		// Clause 3 — not resolved by a declared supersession. "I declared
		// which one wins" IS a resolution; asserted beats asserted, the same
		// doctrine COG-25 uses.
		if e.currencyHasExplicitSupersedesEdge(ctx, ws, ce.src, ce.dst) {
			continue
		}
		live = append(live, ce)
	}
	if len(live) == 0 {
		return results, nil, 0
	}

	// Step 4 — cluster. Union-find over the examined window, so A⊥B, B⊥C is
	// one cluster. One-sided edges (partner live but not retrieved) do not
	// join anything; they annotate their single in-set row.
	parent := make(map[int]int, window)
	find := func(a int) int {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	add := func(i int) {
		if _, ok := parent[i]; !ok {
			parent[i] = i
		}
	}
	for _, ce := range live {
		si, sOK := idx[ce.src]
		di, dOK := idx[ce.dst]
		if sOK {
			add(si)
		}
		if dOK {
			add(di)
		}
		if sOK && dOK {
			ra, rb := find(si), find(di)
			if ra != rb {
				parent[ra] = rb
			}
		}
	}
	clusters := make(map[int][]int, len(parent))
	for i := range parent {
		r := find(i)
		clusters[r] = append(clusters[r], i)
	}

	// Copy before mutating: every score write and reorder below lands in the
	// backing array Run() returned, and the activation engine's async
	// log-drain goroutine may still be reading it — the same hazard
	// applySupersession documents. Paid only when a live conflict exists.
	out = append(make([]activation.ScoredEngram, 0, len(results)), results...)

	// Step 6 — demote. A single min() against the row's OWN earned score, so a
	// conflict can only ever push a row DOWN, never above a genuinely better
	// unrelated match. There is no absolute ceiling; see contradictionDemote.
	for i := range parent {
		own := out[i].Score
		next := own * (1 - contradictionDemote)
		if next > own {
			next = own
		}
		out[i].Score = next
	}

	// Step 5 — order within each cluster by the deterministic ladder, applied
	// as a PERMUTATION over the positions the cluster already occupies. The
	// score re-sort below is stable, so the ladder is only ever the last word
	// on an exact score tie between two cluster members — which is precisely
	// the evaluators' shape (two rivals that scored identically) and precisely
	// where nothing else can decide.
	for _, members := range clusters {
		slots := append([]int(nil), members...)
		sort.Ints(slots)
		sortContradictionCluster(out, members, live, idx)
		rows := make([]activation.ScoredEngram, len(members))
		for k, m := range members {
			rows[k] = out[m]
		}
		for k, slot := range slots {
			out[slot] = rows[k]
		}
	}
	// idx maps ULID -> position and the permutation above moved rows between
	// positions, so it must be rebuilt before anything else reads it.
	for i := 0; i < window; i++ {
		idx[out[i].Engram.ID] = i
	}

	// Annotate. Cluster membership is a set of positions and the permutation
	// above only shuffled rows WITHIN that set, so `clusters` is still correct.
	block := &mbp.ConflictBlock{Unresolved: true, Warning: contradictionWarning}
	memberCluster := make(map[int][]int, len(parent))
	for _, members := range clusters {
		truncated := len(members) > contradictionMaxCluster
		for _, i := range members {
			memberCluster[i] = members
			out[i].UnresolvedContradiction = buildRowConflict(out, i, members, live, idx, partners, truncated)
		}
	}
	block.Pairs = buildConflictPairs(out, live, idx, partners)

	// Step 7 — RE-SORT by score, stably, and let adjacency be whatever the
	// scores produce.
	//
	// This is the ONE rule. An earlier draft gathered each cluster to the rank
	// of its lowest-scoring member instead, to keep the pair adjacent without
	// lifting anything; that buries a dominant answer behind rows it outscores
	// by orders of magnitude (measured: a 0.859 conflicted row dropped from
	// rank 0 to rank 6 behind rows scoring 0.1–0.5) and returns a response that
	// is no longer score-descending — a worse lie than the one the phase exists
	// to fix. Sorting by the demoted score is both demote-only AND monotone:
	// near-tied rivals land adjacent for free, a dominant answer stays at rank
	// 0 (demoted and annotated), and nothing is ever lifted above a better
	// unrelated match because a row's score only ever went down.
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return out[order[x]].Score > out[order[y]].Score })
	reordered := make([]activation.ScoredEngram, len(out))
	pos := make(map[int]int, len(order))
	for newPos, oldIdx := range order {
		reordered[newPos] = out[oldIdx]
		pos[oldIdx] = newPos
	}
	out = reordered

	// Never remove. The phase has no delete path; `order` is a permutation of
	// [0,len(out)) by construction, so this is a tripwire, not a branch anyone
	// expects to take.
	if len(out) != len(results) {
		slog.Error("recall: COG-29 changed the result count — this is a bug", "before", len(results), "after", len(out))
	}

	// Cluster membership must survive truncation: if any cluster member is
	// inside the caller's cut, all of them are. Rebuilt on POST-SORT positions
	// — computing it against the pre-sort ranks would silently mis-report the
	// overflow. The overflow is bounded and REPORTED: returning one side of a
	// conflict alone is the failure this whole phase exists to remove, so the
	// truncation yields to it, but never silently.
	if req.MaxResults > 0 {
		sorted := make(map[int][]int, len(memberCluster))
		for oldIdx, members := range memberCluster {
			np := make([]int, 0, len(members))
			for _, m := range members {
				np = append(np, pos[m])
			}
			sort.Ints(np)
			sorted[pos[oldIdx]] = np
		}
		limit := req.MaxResults
		for p, members := range sorted {
			if p >= req.MaxResults {
				continue
			}
			for _, m := range members {
				if m+1 > limit {
					limit = m + 1
				}
			}
		}
		if maxLimit := req.MaxResults + contradictionMaxCluster - 1; limit > maxLimit {
			limit = maxLimit
		}
		if limit > len(out) {
			limit = len(out)
		}
		keepAtLeast = limit
		block.AdjacencyOverflow = limit - req.MaxResults
		if block.AdjacencyOverflow < 0 {
			block.AdjacencyOverflow = 0
		}
	}

	return out, block, keepAtLeast
}

// pruneConflictBlock drops pairs whose endpoints all fell outside the caller's
// final cut, and drops the block entirely when nothing is left. The block
// describes what the caller RECEIVED; naming a conflict between two memories
// that are not in the response would be an unknown reported as known.
func pruneConflictBlock(block *mbp.ConflictBlock, final []activation.ScoredEngram) *mbp.ConflictBlock {
	if block == nil {
		return nil
	}
	present := make(map[string]bool, len(final))
	for i := range final {
		present[final[i].Engram.ID.String()] = true
	}
	kept := block.Pairs[:0:0]
	for _, p := range block.Pairs {
		if present[p.A] || present[p.B] {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	block.Pairs = kept
	return block
}

// collectContradictionEdges reads the declared contradicts edges touching the
// examined window, in both directions, deduplicated by canonical pair.
//
// DECLARED EDGES ONLY. No similarity-inferred conflict, ever — the same
// asserted/inferred boundary #758/#763 established and COG-25 restates: an
// inferred signal never gets authority, and this phase changes scores.
// Deliberately keyed on the 0x03/0x04 edges rather than the 0x0A marker: the
// marker's key is …|id(16) with a single-partner value, so one engram can
// record exactly ONE 0x0A partner and a second contradiction on the same
// engram overwrites the first (keyspace-registry §15).
func (e *Engine) collectContradictionEdges(ctx context.Context, ws [8]byte, ids []storage.ULID, idx map[storage.ULID]int) []contradictionEdge {
	seen := make(map[[32]byte]int)
	var edges []contradictionEdge
	record := func(src, dst storage.ULID, declared time.Time) {
		a, b := src, dst
		if bytes.Compare(a[:], b[:]) > 0 {
			a, b = b, a
		}
		var k [32]byte
		copy(k[:16], a[:])
		copy(k[16:], b[:])
		if pos, dup := seen[k]; dup {
			// Keep the EARLIEST known declaration: the conflict has existed
			// since the first assertion of it.
			if !declared.IsZero() && (edges[pos].declared.IsZero() || declared.Before(edges[pos].declared)) {
				edges[pos].declared = declared
			}
			return
		}
		_, srcIn := idx[src]
		_, dstIn := idx[dst]
		seen[k] = len(edges)
		edges = append(edges, contradictionEdge{src: src, dst: dst, declared: declared, srcInSet: srcIn, dstInSet: dstIn})
	}

	fwd, err := e.store.GetAssociations(ctx, ws, ids, contradictionAssocScan)
	if err != nil {
		slog.Warn("recall: COG-29 forward association read failed", "err", err)
	} else {
		for src, assocs := range fwd {
			for _, a := range assocs {
				if a.RelType == storage.RelContradicts {
					record(src, a.TargetID, a.CreatedAt)
				}
			}
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			break
		}
		// GetReverseAssociations repurposes TargetID to hold the SOURCE of the
		// edge pointing at id (see storage/association.go).
		rev, err := e.store.GetReverseAssociations(ctx, ws, id, contradictionReverseScan)
		if err != nil {
			slog.Warn("recall: COG-29 reverse association read failed", "id", id.String(), "err", err)
			continue
		}
		for _, a := range rev {
			if a.RelType == storage.RelContradicts {
				record(a.TargetID, id, a.CreatedAt)
			}
		}
	}
	return edges
}

// contradictionEndpointLive is the liveness half of the COG-29 unresolved test,
// and the SINGLE definition of it — GetContradictionReport applies the same
// rule (markResolvedContradictions) so recall and the report can never disagree
// about whether a conflict is still live.
//
// A soft-deleted, archived, or validity-elapsed endpoint is not a live
// conflict. That one clause is what makes evolve, forget (soft),
// forget(not_true_since) and hard-delete all stop the theater — the thing no
// operation in the product could do before #764.
//
// Under as_of the state test is deliberately skipped: lifecycle state is a
// TRANSACTION-time fact with no history to travel to, so a query about an
// earlier instant is answered on the valid-time axis alone. Asserting that a
// fact deleted today was already deleted a month ago would be inventing a
// history the store does not have.
func contradictionEndpointLive(eng *storage.Engram, req *activation.ActivateRequest, now time.Time) bool {
	if eng == nil {
		return false
	}
	if req.AsOf != nil {
		return eng.ValidAt(*req.AsOf)
	}
	if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
		return false
	}
	return !eng.IsExpired(now)
}

// contradictionPartner is a resolved out-of-set endpoint.
type contradictionPartner struct {
	live    bool
	concept string
}

// resolveContradictionPartners resolves the endpoints that are NOT in the
// result set: one batched GetEngrams over the distinct IDs, then the shared
// visibility gate.
//
// This single clause is what makes evolve, forget (soft), forget
// (not_true_since) and hard-delete all stop the theater. A partner that is
// soft-deleted, expired, or invisible to this caller is NOT a live conflict —
// and an unnameable partner is skipped entirely rather than named, so a
// lease-hidden or filtered engram's ID can never leak through a conflict
// annotation (#548: the ID is the existence).
func (e *Engine) resolveContradictionPartners(
	ctx context.Context, ws [8]byte, edges []contradictionEdge,
	idx map[storage.ULID]int, gate *visibilityGate,
) map[storage.ULID]contradictionPartner {
	want := make([]storage.ULID, 0, len(edges))
	seen := make(map[storage.ULID]bool, len(edges))
	for _, ce := range edges {
		for _, id := range [2]storage.ULID{ce.src, ce.dst} {
			if _, inSet := idx[id]; inSet || seen[id] {
				continue
			}
			seen[id] = true
			want = append(want, id)
		}
	}
	out := make(map[storage.ULID]contradictionPartner, len(want))
	if len(want) == 0 {
		return out
	}
	engrams, err := e.store.GetEngrams(ctx, ws, want)
	if err != nil {
		slog.Warn("recall: COG-29 partner resolution failed", "err", err)
		return out
	}
	for i, eng := range engrams {
		if i >= len(want) || eng == nil {
			continue
		}
		// Nameable is the existence half: an engram this caller may not even
		// know about must never be named inside another row's annotation
		// (#548 — the ID is the existence). Liveness is the shared rule.
		if !gate.Nameable(ctx, e.store, ws, eng) {
			continue
		}
		if !contradictionEndpointLive(eng, gate.req, gate.now) {
			continue
		}
		out[want[i]] = contradictionPartner{live: true, concept: eng.Concept}
	}
	return out
}

// sortContradictionCluster orders a cluster's members by the deterministic
// ladder — first rule that discriminates:
//
//  1. Newer EffectiveValidFrom first (the same currency signal COG-25 uses).
//  2. Newer declaration: the SOURCE of the contradicts edge is the asserting
//     side ("this new thing contradicts that old one"), so it goes first.
//  3. ULID descending — monotonic, so newer first, and the order is total and
//     reproducible.
//
// This is a PRESENTATION order, not a verdict. Neither side of an unresolved
// conflict is known to be right; that is exactly why both are returned.
func sortContradictionCluster(rows []activation.ScoredEngram, members []int, edges []contradictionEdge, idx map[storage.ULID]int) {
	asserts := make(map[storage.ULID]map[storage.ULID]bool, len(members))
	for _, ce := range edges {
		if asserts[ce.src] == nil {
			asserts[ce.src] = map[storage.ULID]bool{}
		}
		asserts[ce.src][ce.dst] = true
	}
	sort.SliceStable(members, func(x, y int) bool {
		a, b := rows[members[x]].Engram, rows[members[y]].Engram
		av, bv := a.EffectiveValidFrom(), b.EffectiveValidFrom()
		if !av.Equal(bv) {
			return av.After(bv)
		}
		if asserts[a.ID][b.ID] != asserts[b.ID][a.ID] {
			return asserts[a.ID][b.ID]
		}
		return bytes.Compare(a.ID[:], b.ID[:]) > 0
	})
}

// contradictionOrderBasis names which ladder rule discriminated a pair.
func contradictionOrderBasis(a, b *storage.Engram, asserted bool) string {
	if !a.EffectiveValidFrom().Equal(b.EffectiveValidFrom()) {
		return contradictionBasisValidFrom
	}
	if asserted {
		return contradictionBasisAsserting
	}
	return contradictionBasisULIDTiebreak
}

// buildRowConflict assembles the always-on per-row payload. Always-on for the
// same reason superseded_by is: an agent must never be handed a disputed fact
// without being told.
//
// A row can be an endpoint of several declared contradicts edges, and the
// annotation names exactly one partner. Selection is TOTAL and independent of
// map iteration order: prefer a partner that is itself in the result set (the
// caller can see both sides, so naming that one is strictly more useful), then
// the lowest partner ULID. An earlier draft took the first edge encountered
// while ranging a map, which made both `with` and `partner_in_results` flip
// between identical queries.
func buildRowConflict(
	rows []activation.ScoredEngram, i int, members []int,
	edges []contradictionEdge, idx map[storage.ULID]int,
	partners map[storage.ULID]contradictionPartner,
	clusterTruncated bool,
) *activation.ContradictionConflict {
	self := rows[i].Engram.ID
	best := -1
	var bestOther storage.ULID
	bestInSet := false
	for k := range edges {
		ce := edges[k]
		if ce.src != self && ce.dst != self {
			continue
		}
		other := ce.other(self)
		_, inSet := idx[other]
		better := best < 0 ||
			(inSet && !bestInSet) ||
			(inSet == bestInSet && bytes.Compare(other[:], bestOther[:]) < 0)
		if better {
			best, bestOther, bestInSet = k, other, inSet
		}
	}
	if best < 0 {
		return nil
	}

	ce := edges[best]
	side := contradictionSideChallenged
	if ce.src == self {
		side = contradictionSideAsserted
	}
	concept := ""
	if bestInSet {
		concept = rows[idx[bestOther]].Engram.Concept
	} else if p, ok := partners[bestOther]; ok {
		concept = p.concept
	}
	return &activation.ContradictionConflict{
		With:             bestOther,
		WithConcept:      concept,
		Side:             side,
		DeclaredAt:       ce.declared,
		PartnerInResults: bestInSet,
		// len(members) is authoritative: an in-set partner was unioned into
		// this row's cluster in step 4, so a cluster with an in-set partner
		// always has at least two members.
		ClusterSize:      len(members),
		ClusterTruncated: clusterTruncated,
	}
}

// buildConflictPairs assembles the response-level pair list in a deterministic
// order.
func buildConflictPairs(
	rows []activation.ScoredEngram, edges []contradictionEdge,
	idx map[storage.ULID]int, partners map[storage.ULID]contradictionPartner,
) []mbp.ConflictPairInfo {
	pairs := make([]mbp.ConflictPairInfo, 0, len(edges))
	for _, ce := range edges {
		si, sOK := idx[ce.src]
		di, dOK := idx[ce.dst]
		if !sOK && !dOK {
			continue
		}
		info := mbp.ConflictPairInfo{
			A: ce.src.String(), B: ce.dst.String(),
			PartnerInResults: sOK && dOK,
		}
		if !ce.declared.IsZero() {
			info.DeclaredAt = ce.declared.UTC().Format(time.RFC3339)
		}
		var srcEng, dstEng *storage.Engram
		if sOK {
			srcEng = rows[si].Engram
			info.AConcept = srcEng.Concept
		} else if p, ok := partners[ce.src]; ok {
			info.AConcept = p.concept
		}
		if dOK {
			dstEng = rows[di].Engram
			info.BConcept = dstEng.Concept
		} else if p, ok := partners[ce.dst]; ok {
			info.BConcept = p.concept
		}
		if srcEng != nil && dstEng != nil {
			// Preferred names which side this response ORDERED FIRST — a
			// presentation order, never a verdict on which memory is true.
			if srcEng.EffectiveValidFrom().Before(dstEng.EffectiveValidFrom()) {
				info.Preferred = "b"
			} else {
				info.Preferred = "a"
			}
			info.Basis = contradictionOrderBasis(srcEng, dstEng, true)
		}
		pairs = append(pairs, info)
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].A != pairs[j].A {
			return pairs[i].A < pairs[j].A
		}
		return pairs[i].B < pairs[j].B
	})
	return pairs
}
