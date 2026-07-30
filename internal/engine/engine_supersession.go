package engine

import (
	"bytes"
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// supersessionEpsilon is the score gap placed between a superseded engram and
	// the current (head) engram that replaces it. It is a pairwise ordering nudge,
	// not a tuned relevance constant: the head inherits the MAX earned score in its
	// supersedes chain (the score belongs to the TOPIC, and the head is the correct
	// answer for it), and a superseded fact sits at min(its own earned score,
	// head−ε) — DEMOTE-ONLY: supersession can only lower a stale fact's rank, never
	// lift a barely-matching stale near-duplicate above a genuinely-relevant
	// unrelated result.
	supersessionEpsilon = 0.01

	// supersessionMaxDepth caps the supersedes-chain walk. A chain longer than
	// this, a cycle, or an ambiguous (multi-superseder) node is treated as
	// unresolvable and left un-demoted rather than guessed at — degrade loudly (a
	// WARN), never silently pick an arbitrary head.
	supersessionMaxDepth = 8

	// supersessionReverseScan bounds the reverse-association scan per chain hop. It
	// must be generous enough that a heavily-referenced engram's RelSupersedes edge
	// is not missed behind other reverse edges (a miss would silently leave recall
	// leading with the stale fact — the exact thing this phase prevents).
	supersessionReverseScan = 256

	// supersessionMargin is added to MaxResults to decide how many top (already
	// score-sorted) candidates to examine. Candidates below this cannot survive
	// truncation, so scanning them (a Pebble reverse-assoc iterator each) is wasted
	// I/O on the hot recall path. The margin absorbs the ε reshuffles at the boundary.
	supersessionMargin = 16
)

// applySupersession makes recall supersedes-aware: when a candidate is superseded
// by a newer engram (a manual RelSupersedes link whose predecessor is still
// active — evolve() soft-deletes its predecessor, so those never reach here), the
// current fact is promoted to the rank the topic earned and the stale fact is
// demoted. If the head is not already in the candidate pool it is INJECTED (the
// #607 candidate-pool precedent) — demotion alone would risk returning nothing
// about the topic when the query matched the stale phrasing but not the current
// one (a silently-truncated result). The stale fact is never removed.
//
// Two-phase and DEMOTE-ONLY so it is order-independent and can never displace a
// genuine unrelated match:
//   - Phase 1 resolves each candidate's chain head and computes, per head, the MAX
//     earned score over {the head's own score, every stale score pointing at it}.
//   - Phase 2 assigns each head its final score (injecting absent heads), then sets
//     each stale fact to min(its ORIGINAL earned score, head_final − ε). A stale
//     fact thus never rises above where it started; the head sits one ε above the
//     highest stale in its chain.
//
// This is the ranking half of supersedes-aware recall (the #1 sentient-feel
// finding). The always-on superseded_by/current_version annotation payload is a
// following increment; today the opt-in `annotate` path still surfaces it.
//
// Substitution is resolved UNDER THE CALLER'S VIEW and applied atomically per
// chain. The chain walk treats a node the caller's visibility contract refuses
// (meta filters, structured filter, trust, foreign live lease, valid-time —
// the shared visibilityGate) as traversable but unnameable: the effective head
// is the DEEPEST ADMITTED node on the chain, and the annotation names the
// NEAREST ADMITTED successor, so a hidden intermediate's ID can never leak
// through SupersededBy (#548's class — the ID is the existence) and a hidden
// tail cannot void a substitution that is still correct at a visible
// intermediate (under AsOf, the newest admitted node IS the head at the
// caller's instant). When NO node above the stale is admitted, the whole
// substitution abstains — no injection, no demotion, no annotation:
// demote-without-inject silently truncates the topic, annotating leaks the
// hidden ID, and under AsOf the demotion itself is wrong because at the
// caller's instant the "stale" fact WAS the truth. A node already in the
// result set cleared its own admission (phase 6, or a prior injector's gate)
// and is admitted by construction. injected reports how many absent heads
// were admitted and appended, so the caller can keep TotalFound honest.
//
// Runs post-scoring / post-entity-boost and PRE-truncation so injected heads
// are not cut. No store writes (reverse-assoc + engram + lease reads only;
// the results slice is re-scored and re-sorted in place); observe-safe.
// req.MaxResults bounds the work to the top survivors (0 = examine all). now
// is the shared injection clock — the caller passes the same instant to the
// final COG-19 gate so a validity boundary cannot fall between admission here
// and that last cut (which would un-atomize the substitution it just applied).
func (e *Engine) applySupersession(ctx context.Context, ws [8]byte, results []activation.ScoredEngram, req *activation.ActivateRequest, now time.Time) (out []activation.ScoredEngram, injected int) {
	if len(results) == 0 {
		return results, 0
	}
	maxResults := req.MaxResults

	// ULID → index in results (grows as heads are injected).
	seen := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seen[r.Engram.ID] = i
	}

	// Only examine the top survivors: results are already score-descending (entity
	// boost re-sorts), and anything below MaxResults+margin cannot survive the
	// truncation applied right after this phase.
	orig := len(results)
	if maxResults > 0 && orig > maxResults+supersessionMargin {
		orig = maxResults + supersessionMargin
	}

	// Snapshot original earned scores BEFORE any mutation, so Phase-2 demotion is
	// against the earned score, not a running (already-promoted) head score.
	type staleRef struct {
		idx       int
		earned    float64
		headID    storage.ULID
		immediate storage.ULID
	}
	var stales []staleRef
	headEngram := make(map[storage.ULID]*storage.Engram)
	headFinal := make(map[storage.ULID]float64)

	// Existence decisions, cached per chain node: several stales can share a
	// chain, and the gate's lease check reads the store. A node already in the
	// result pool cleared its own admission (phase 6, or the entity-boost gate
	// for boost injections, which runs before this phase) — nameable by
	// construction, no store read.
	gate := newVisibilityGate(req, now)
	nameableCache := make(map[storage.ULID]bool)
	nameable := func(eng *storage.Engram) bool {
		if _, inPool := seen[eng.ID]; inPool {
			return true
		}
		if a, decided := nameableCache[eng.ID]; decided {
			return a
		}
		a := gate.Nameable(ctx, e.store, ws, eng)
		nameableCache[eng.ID] = a
		return a
	}

	for i := 0; i < orig; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		// The walk resolves the effective head under the caller's view: only
		// nameable, view-known nodes can be returned, only a view-valid node
		// can be the head, and !superseded covers both "not superseded at
		// all" and "no substitutable successor" — the atomic abstention branch.
		head, immediate, superseded := e.resolveSupersessionHead(ctx, ws, results[i].Engram.ID, gate, nameable)
		if !superseded {
			continue
		}
		earned := results[i].Score
		stales = append(stales, staleRef{idx: i, earned: earned, headID: head.ID, immediate: immediate})
		headEngram[head.ID] = head
		if earned > headFinal[head.ID] {
			headFinal[head.ID] = earned
		}
	}
	if len(stales) == 0 {
		return results, 0
	}

	// Fold each head's OWN earned score (when it was already retrieved) into its
	// final: a head keeps its own high relevance and never drops below it.
	for headID := range headFinal {
		if idx, ok := seen[headID]; ok && results[idx].Score > headFinal[headID] {
			headFinal[headID] = results[idx].Score
		}
	}

	// Phase 2a: assign head scores; inject absent (gate-admitted) heads.
	for headID, final := range headFinal {
		if idx, ok := seen[headID]; ok {
			results[idx].Score = final
		} else {
			results = append(results, activation.ScoredEngram{Engram: headEngram[headID], Score: final})
			seen[headID] = len(results) - 1
			injected++
		}
	}

	// Phase 2b: demote each stale fact — but only ever downward — and record the
	// supersession annotation (immediate superseder + chain head) so recall can
	// surface "stale — current is X" without a second call or the annotate flag.
	for _, s := range stales {
		demoted := headFinal[s.headID] - supersessionEpsilon
		if s.earned < demoted {
			demoted = s.earned
		}
		results[s.idx].Score = demoted
		results[s.idx].SupersededBy = s.immediate
		results[s.idx].CurrentVersion = s.headID
	}

	// Deterministic order: stable sort with a ULID tiebreak so the manufactured
	// head−ε ties (and the MaxResults truncation boundary) are reproducible.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		a, b := results[i].Engram.ID, results[j].Engram.ID
		return bytes.Compare(a[:], b[:]) < 0
	})
	return results, injected
}

// resolveSupersessionHead walks the RelSupersedes chain upward from startID to
// the effective head UNDER THE CALLER'S VIEW: the deepest chain node that is
// both nameable (existence-visible: lease, trust, filters — gate.Nameable)
// and valid for the caller's temporal view (gate.ValidForView). The two
// halves are deliberately different tests:
//
//   - A node that fails Nameable is hidden — TRAVERSABLE BUT UNNAMEABLE. The
//     walk continues through it, but it can be returned as neither head nor
//     immediate, so a hidden ID cannot surface in a result row or its
//     SupersededBy annotation (#548: the ID is the existence).
//   - A node that is nameable but fails ValidForView is expired lineage —
//     NAMEABLE BUT NOT CURRENT. Chain intermediates are validity-expired by
//     construction (supersession stamps ValidUntil on every superseded
//     engram), so they may be named as lineage, but only a view-valid node
//     can be the substitution's head. Under as-of, a node postdating the
//     caller's instant (gate.ViewFuture) does not exist yet in that view and
//     may not even be named.
//
// immediate is the NEAREST nameable, view-known successor of startID — the
// caller-visible answer to "what replaced this". A soft-deleted/archived
// superseder is different in kind from all of the above: it VOIDS the
// supersession at that hop for every caller (the walk stops), matching
// evolve-retraction semantics — deletion is a lifecycle fact, not a
// per-caller view.
//
// Returns (head, immediate, true) when a view-valid head exists; (nil, zero,
// false) when none does — never superseded, superseder soft-deleted (voided),
// no successor both nameable and valid for this view (the caller's atomic
// abstention), a hop with more than one active superseder (ambiguous — WARN,
// don't guess), or a cycle (WARN, leave un-demoted). The depth cap is
// different: it silently TRUNCATES the walk, so on a chain longer than
// supersessionMaxDepth the head is the deepest admitted node within the cap —
// possibly a non-terminal intermediate presented as current. Long chains are
// rare (evolve chains heal at startup; the cap exists for pathological
// manual graphs) and a cap-abstain would regress every long legacy chain.
//
// GetReverseAssociations(X) returns edges pointing TO X with the association's
// TargetID repurposed to hold the SOURCE — so for a RelSupersedes edge it is the
// engram that supersedes X (see annotation.go / storage/association.go).
func (e *Engine) resolveSupersessionHead(ctx context.Context, ws [8]byte, startID storage.ULID, gate *visibilityGate, nameable func(*storage.Engram) bool) (head *storage.Engram, immediate storage.ULID, superseded bool) {
	cur := startID
	visited := map[storage.ULID]bool{startID: true}

	for depth := 0; depth < supersessionMaxDepth; depth++ {
		rev, err := e.store.GetReverseAssociations(ctx, ws, cur, supersessionReverseScan)
		if err != nil {
			break
		}
		// Collect all distinct superseders at this hop; >1 is genuine ambiguity.
		var next storage.ULID
		found := 0
		for i := range rev {
			if rev[i].RelType == storage.RelSupersedes {
				if found == 0 || rev[i].TargetID != next {
					found++
					next = rev[i].TargetID
				}
			}
		}
		if found == 0 {
			break // cur has no superseder → the chain ends here
		}
		if found > 1 {
			slog.Warn("recall: engram has multiple superseders, leaving un-demoted", "id", cur.String())
			return nil, storage.ULID{}, false
		}
		if visited[next] {
			slog.Warn("recall: supersedes cycle detected, leaving un-demoted", "at", next.String())
			return nil, storage.ULID{}, false
		}
		visited[next] = true

		eng, err := e.store.GetEngram(ctx, ws, next)
		if err != nil || eng == nil {
			break // dangling edge → stop; the chain ends below it
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			break // superseder retracted → supersession voided at this hop, for every caller
		}
		cur = next
		if !nameable(eng) || gate.ViewFuture(eng) {
			continue // hidden or not-yet in this view: walk through, never name
		}
		if immediate == (storage.ULID{}) {
			immediate = next // nearest nameable, view-known successor
		}
		if gate.ValidForView(eng) {
			head = eng // deepest nameable node valid under the caller's view
		}
	}

	if head == nil {
		// No view-valid successor: not superseded, or nothing this caller's
		// view can substitute toward — either way, abstain whole.
		return nil, storage.ULID{}, false
	}
	return head, immediate, true
}
