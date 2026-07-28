package engine

import (
	"context"
	"log/slog"
	"sort"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// supersessionEpsilon is the score gap placed between a superseded engram and
	// the current (head) engram that replaces it. It is a pairwise ordering nudge,
	// not a tuned relevance constant: the head inherits the superseded engram's
	// earned score (the score belongs to the TOPIC, and the head is the correct
	// answer for that topic), and the superseded engram sits exactly one epsilon
	// below it. Small enough that ordering among all non-superseded results — and
	// among distinct superseded pairs — is unchanged.
	supersessionEpsilon = 0.01

	// supersessionMaxDepth caps the supersedes-chain walk. A chain longer than
	// this (or a cycle) is treated as unresolvable and left un-demoted rather than
	// guessed at — degrade loudly (a WARN), never silently pick an arbitrary head.
	supersessionMaxDepth = 8
)

// applySupersession makes recall supersedes-aware: when a candidate is superseded
// by a newer engram (a manual RelSupersedes link whose predecessor is still
// active — evolve() soft-deletes its predecessor, so those never reach here), the
// current fact is promoted to the rank the stale fact earned and the stale fact is
// demoted immediately below it. If the head is not already in the candidate pool,
// it is INJECTED (the #607 candidate-pool precedent) — demotion alone would risk
// returning nothing about the topic when the query matched the stale phrasing but
// not the current one (a silently-truncated result). Never removes the stale fact:
// it stays visible, one slot down, so a wrong link costs one rank position, not
// data loss, and is self-revealing rather than hiding a true fact.
//
// This is the ranking half of supersedes-aware recall (the #1 sentient-feel
// finding). The always-on `superseded_by`/`current_version` annotation payload is
// a following increment; today the opt-in `annotate` path still surfaces it.
//
// Runs post-scoring / post-entity-boost and PRE-truncation so injected heads are
// not cut. Mirrors applyEntityBoost's shape.
func (e *Engine) applySupersession(ctx context.Context, ws [8]byte, results []activation.ScoredEngram) []activation.ScoredEngram {
	if len(results) == 0 {
		return results
	}

	// ULID → index in results (grows as heads are injected).
	seen := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seen[r.Engram.ID] = i
	}

	// Only the ORIGINAL candidates are examined for supersession; an injected head
	// is by construction the top of its chain, so it never needs re-processing.
	orig := len(results)
	for i := 0; i < orig; i++ {
		if err := ctx.Err(); err != nil {
			break
		}
		head, superseded := e.resolveSupersessionHead(ctx, ws, results[i].Engram.ID)
		if !superseded {
			continue
		}
		staleScore := results[i].Score
		if idx, ok := seen[head.ID]; ok {
			// Head already retrieved: it takes the max of its own and the stale
			// score (the topic's earned relevance), stale sits epsilon below.
			if staleScore > results[idx].Score {
				results[idx].Score = staleScore
			}
			results[i].Score = results[idx].Score - supersessionEpsilon
		} else {
			// Head not in the pool — inject it at the stale fact's score so the
			// current answer survives truncation; demote the stale fact below it.
			results = append(results, activation.ScoredEngram{Engram: head, Score: staleScore})
			seen[head.ID] = len(results) - 1
			results[i].Score = staleScore - supersessionEpsilon
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

// resolveSupersessionHead walks the RelSupersedes chain upward from startID to the
// current head: the newest ACTIVE engram that supersedes it and is itself
// superseded by nothing active. Returns (head, true) when startID is superseded by
// an active engram; (nil, false) when it is not superseded, when the only
// superseder is soft-deleted/archived (a voided supersession), or when the chain
// is a cycle / exceeds the depth cap (logged, left un-demoted).
//
// GetReverseAssociations(X) returns edges pointing TO X with the association's
// TargetID repurposed to hold the SOURCE — so for a RelSupersedes edge it is the
// engram that supersedes X (see annotation.go).
func (e *Engine) resolveSupersessionHead(ctx context.Context, ws [8]byte, startID storage.ULID) (*storage.Engram, bool) {
	cur := startID
	visited := map[storage.ULID]bool{startID: true}
	var head *storage.Engram

	for depth := 0; depth < supersessionMaxDepth; depth++ {
		rev, err := e.store.GetReverseAssociations(ctx, ws, cur, 16)
		if err != nil {
			break
		}
		var next *storage.ULID
		for i := range rev {
			if rev[i].RelType == storage.RelSupersedes {
				s := rev[i].TargetID // the superseding engram
				next = &s
				break
			}
		}
		if next == nil {
			break // cur has no superseder → cur is the head (or startID itself)
		}
		if visited[*next] {
			slog.Warn("recall: supersedes cycle detected, leaving un-demoted", "at", next.String())
			return nil, false
		}
		visited[*next] = true

		eng, err := e.store.GetEngram(ctx, ws, *next)
		if err != nil || eng == nil {
			break // dangling edge → stop; cur is the effective head
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			break // superseder gone → supersession voided; stop at cur
		}
		head = eng
		cur = *next
	}

	if head == nil {
		return nil, false // startID is not superseded by any active engram
	}
	return head, true
}
