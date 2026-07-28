package engine

import (
	"bytes"
	"context"
	"log/slog"
	"sort"

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
// Runs post-scoring / post-entity-boost and PRE-truncation so injected heads are
// not cut. Pure read path (reverse-assoc + engram reads only); observe-safe.
// maxResults bounds the work to the top survivors (0 = examine all).
func (e *Engine) applySupersession(ctx context.Context, ws [8]byte, results []activation.ScoredEngram, maxResults int) []activation.ScoredEngram {
	if len(results) == 0 {
		return results
	}

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

	for i := 0; i < orig; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		head, immediate, superseded := e.resolveSupersessionHead(ctx, ws, results[i].Engram.ID)
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
		return results
	}

	// Fold each head's OWN earned score (when it was already retrieved) into its
	// final: a head keeps its own high relevance and never drops below it.
	for headID := range headFinal {
		if idx, ok := seen[headID]; ok && results[idx].Score > headFinal[headID] {
			headFinal[headID] = results[idx].Score
		}
	}

	// Phase 2a: assign head scores; inject absent heads.
	for headID, final := range headFinal {
		if idx, ok := seen[headID]; ok {
			results[idx].Score = final
		} else {
			results = append(results, activation.ScoredEngram{Engram: headEngram[headID], Score: final})
			seen[headID] = len(results) - 1
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
	return results
}

// resolveSupersessionHead walks the RelSupersedes chain upward from startID to the
// current head: the newest ACTIVE engram that supersedes it and is itself
// superseded by nothing active. Returns (head, true) when startID is superseded by
// an active engram; (nil, false) when it is not superseded, when the only
// superseder is soft-deleted/archived (a voided supersession), when a hop has more
// than one active superseder (ambiguous — WARN, don't guess), or when the chain is
// a cycle / exceeds the depth cap (WARN, leave un-demoted).
//
// GetReverseAssociations(X) returns edges pointing TO X with the association's
// TargetID repurposed to hold the SOURCE — so for a RelSupersedes edge it is the
// engram that supersedes X (see annotation.go / storage/association.go).
func (e *Engine) resolveSupersessionHead(ctx context.Context, ws [8]byte, startID storage.ULID) (head *storage.Engram, immediate storage.ULID, superseded bool) {
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
			break // cur has no superseder → cur is the head (or startID itself)
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
			break // dangling edge → stop; cur is the effective head
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			break // superseder gone → supersession voided; stop at cur
		}
		if head == nil {
			immediate = next // first active superseder = the immediate one
		}
		head = eng
		cur = next
	}

	if head == nil {
		return nil, storage.ULID{}, false // startID is not superseded by any active engram
	}
	return head, immediate, true
}
