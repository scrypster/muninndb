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

// Reasons a chain walk produced no substitutable head. Distinct values, not
// free text, so COG-28 can branch on them (design §5.5).
const (
	supersessionBlockNotSuperseded = "not_superseded"     // no RelSupersedes edge at all — not a block, just silence
	supersessionBlockAmbiguous     = "ambiguous"          // fork: >1 active superseder at a hop; refusing to choose
	supersessionBlockCycle         = "cycle"              // the chain loops
	supersessionBlockRetracted     = "retracted"          // the successor was soft-deleted/archived: supersession voided
	supersessionBlockNoCurrent     = "no_current_version" // successors exist, none is valid for this view
	supersessionBlockHidden        = "hidden"             // successors exist, none is nameable by this caller
)

// supersessionWalk is the result of resolveSupersessionHead. It replaced a
// three-value return so the walk can report WHY it refused (Reason) and
// WHETHER the depth cap cut it short (Truncated) — two facts the ranking
// caller may ignore but the COG-28 substitution caller must surface, because
// there the injected head is the only row the caller sees.
type supersessionWalk struct {
	// Head is the deepest chain node both nameable and valid for the caller's
	// view. nil iff OK is false.
	Head *storage.Engram
	// Immediate is the NEAREST nameable, view-known successor of the start
	// node — the caller-visible answer to "what replaced this".
	Immediate storage.ULID
	// OK reports that a view-valid head exists.
	OK bool
	// Truncated reports that the walk exhausted supersessionMaxDepth: Head is
	// the deepest admitted node WITHIN the cap and may not be the terminus.
	Truncated bool
	// Reason names the refusal when OK is false; empty when OK.
	Reason string
}

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
	return e.applySupersessionWithGate(ctx, ws, results, req, newVisibilityGate(req, now), make(map[storage.ULID]bool))
}

// applySupersessionWithGate is applySupersession with the visibility gate and
// its per-node decision cache supplied by the caller. activateCore shares one
// gate and one cache across BOTH post-pipeline chain phases (COG-28
// substitution, then this one), so a broad query over a vault with many evolve
// chains pays each node's lease read once instead of twice. The gate carries
// the injection clock, so passing it also guarantees the two phases resolve
// visibility at the same instant.
func (e *Engine) applySupersessionWithGate(ctx context.Context, ws [8]byte, results []activation.ScoredEngram, req *activation.ActivateRequest, gate *visibilityGate, nameableCache map[storage.ULID]bool) (out []activation.ScoredEngram, injected int) {
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
		// Truncation is ignored HERE (and only here): on this path the stale row
		// stays visible and annotated beside the head, so a non-terminal
		// intermediate presented as current is still accompanied by the record
		// it replaced. The COG-28 substitution path, where the injected head is
		// the ONLY row the caller sees, propagates it loudly instead.
		walk := e.resolveSupersessionHead(ctx, ws, results[i].Engram.ID, gate, nameable)
		if !walk.OK {
			continue
		}
		head, immediate := walk.Head, walk.Immediate
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

	// Copy before mutating: every write below (head scores, demotions, the
	// re-sort) lands in the backing array Run() returned, and the activation
	// engine's async log-drain goroutine may still be reading that array's
	// Score fields — the same hazard the final COG-19 gate in activateCore
	// already avoids by filtering into a NEW slice. Reproduced under -race with
	// a link-supersedes chain read under include_invalid, which needs neither
	// evolve nor a soft-delete. Paid only when a substitution actually
	// happens: the no-stale path above returns the caller's slice untouched.
	// Capacity covers the heads Phase 2a may append.
	results = append(make([]activation.ScoredEngram, 0, len(results)+len(headFinal)), results...)

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
// Returns OK=true with Head set when a view-valid head exists; OK=false when
// none does — never superseded, superseder soft-deleted (voided), no successor
// both nameable and valid for this view (the caller's atomic abstention), a hop
// with more than one active superseder (ambiguous — WARN, don't guess), or a
// cycle (WARN, leave un-demoted). Reason names which of those it was, so the
// COG-28 substitution caller can report an actionable abstention instead of a
// generic empty response; the ranking caller ignores it.
//
// The depth cap is different: it TRUNCATES the walk, so on a chain longer than
// supersessionMaxDepth the head is the deepest admitted node within the cap —
// possibly a non-terminal intermediate presented as current. Long chains are
// rare (evolve chains heal at startup; the cap exists for pathological manual
// graphs) and a cap-abstain would regress every long legacy chain, so the walk
// still substitutes — but it now REPORTS the truncation (Truncated) rather than
// swallowing it. That was tolerable while the stale row was always visible
// beside the head; on the substitution path the injected head is the only row
// the caller sees, and presenting a non-terminal intermediate as "current"
// without saying so is silently-wrong.
//
// GetReverseAssociations(X) returns edges pointing TO X with the association's
// TargetID repurposed to hold the SOURCE — so for a RelSupersedes edge it is the
// engram that supersedes X (see annotation.go / storage/association.go).
func (e *Engine) resolveSupersessionHead(ctx context.Context, ws [8]byte, startID storage.ULID, gate *visibilityGate, nameable func(*storage.Engram) bool) supersessionWalk {
	cur := startID
	visited := map[storage.ULID]bool{startID: true}

	var head *storage.Engram
	var immediate storage.ULID
	sawSuccessor, sawRetracted, sawHidden, truncated := false, false, false, false
	depth := 0
	for ; depth < supersessionMaxDepth; depth++ {
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
			return supersessionWalk{Reason: supersessionBlockAmbiguous}
		}
		if visited[next] {
			slog.Warn("recall: supersedes cycle detected, leaving un-demoted", "at", next.String())
			return supersessionWalk{Reason: supersessionBlockCycle}
		}
		visited[next] = true
		sawSuccessor = true

		eng, err := e.store.GetEngram(ctx, ws, next)
		if err != nil || eng == nil {
			break // dangling edge → stop; the chain ends below it
		}
		// RETRACTION vs. FURTHER SUPERSESSION. Both leave a hop's superseder
		// soft-deleted, and telling them apart is load-bearing: a retracted
		// successor voids the supersession for every caller, while a successor
		// that was itself superseded is an ordinary chain INTERMEDIATE that the
		// walk must pass through to reach the head.
		//
		// The discriminator is the same closed-ValidUntil signature the rest of
		// COG-28 uses, and it is exact rather than heuristic: supersession
		// (evolve and Link(relation=supersedes)) soft-deletes AND stamps in one
		// atomic batch, a plain muninn_forget soft-deletes and leaves the stamp
		// OPEN, and forget(not_true_since) stamps WITHOUT soft-deleting. One
		// non-supersession sequence shares the signature — forget(not_true_since)
		// followed later by a plain forget — but it is benign here: with no
		// RelSupersedes edge the walk never fires on such a node, and mid-chain
		// it is traversed-but-unnameable (no_current_version, not retracted).
		//
		// Before this, an EVOLVE chain longer than one hop could never resolve:
		// every intermediate an evolve leaves behind is soft-deleted, so the
		// walk read the first one as "retracted" and voided the whole chain.
		// A→B→C returned nothing at all, which is #763 again with an extra hop.
		// StateArchived still stops the walk unconditionally (a storage-tier
		// fact whose payload is not guaranteed resident).
		if eng.State == storage.StateArchived ||
			(eng.State == storage.StateSoftDeleted && eng.ValidUntil.IsZero()) {
			sawRetracted = true
			break // superseder retracted → supersession voided at this hop, for every caller
		}
		cur = next
		if !nameable(eng) || gate.ViewFuture(eng) {
			// Hidden from this caller, not yet in this view, or — the ordinary
			// evolve case — an intermediate that is itself superseded and so
			// fails the default lifecycle cut. Traversable, never nameable.
			// Only a genuine CALLER-visibility refusal counts as "hidden" for
			// the abstention reason; a superseded intermediate is expected
			// structure, not a permission problem. (Under as_of the lifecycle
			// cut admits such an intermediate, and it may legitimately BE the
			// head at the caller's instant — the branch below decides.)
			if eng.State == storage.StateActive {
				sawHidden = true
			}
			continue
		}
		if immediate == (storage.ULID{}) {
			immediate = next // nearest nameable, view-known successor
		}
		if gate.ValidForView(eng) {
			head = eng // deepest nameable node valid under the caller's view
		}
	}
	// The walk ran the cap out without reaching a terminus. Deliberately
	// OVER-reports by one: a chain whose terminus sits at exactly
	// supersessionMaxDepth hops is flagged truncated because the loop never
	// gets the iteration that would prove it terminal. Over-warning that a
	// "current" answer might not be the last word is the safe direction.
	if depth >= supersessionMaxDepth {
		truncated = true
	}

	if head == nil {
		// No view-valid successor: not superseded, or nothing this caller's
		// view can substitute toward — either way, abstain whole.
		switch {
		case !sawSuccessor:
			return supersessionWalk{Reason: supersessionBlockNotSuperseded}
		case sawRetracted:
			return supersessionWalk{Reason: supersessionBlockRetracted}
		case sawHidden:
			return supersessionWalk{Reason: supersessionBlockHidden, Truncated: truncated}
		default:
			return supersessionWalk{Reason: supersessionBlockNoCurrent, Truncated: truncated}
		}
	}
	return supersessionWalk{Head: head, Immediate: immediate, OK: true, Truncated: truncated}
}
