package engine

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/scrypster/muninndb/internal/storage"
)

// ErrEvolveTargetSuperseded is returned (wrapped) when evolve targets a
// memory that already has a successor. Evolving it would mint a second, rival
// successor — a version fork that recall's head resolution refuses to choose
// between (supersessionBlockAmbiguous). A production store was measured with
// 15 such forks.
var ErrEvolveTargetSuperseded = errors.New("evolve target is already superseded")

// evolveLock returns the stripe mutex for (vault, target id).
func (e *Engine) evolveLock(ws [8]byte, id storage.ULID) *sync.Mutex {
	h := fnv.New32a()
	h.Write(ws[:])
	h.Write(id[:])
	return &e.evolveLocks[h.Sum32()%upsertKeyStripes]
}

// distinctSuperseders returns the distinct engrams holding a RelSupersedes
// edge onto id, from an UNCAPPED relation-filtered scan (store.SupersedersOf):
// a capped mixed-relation read can be starved by other incoming links and let
// a second evolve fork.
func (e *Engine) distinctSuperseders(ctx context.Context, ws [8]byte, id storage.ULID) ([]storage.ULID, error) {
	return e.store.SupersedersOf(ctx, ws, id)
}

// refuseSupersededEvolveTarget returns nil when target has no successor (the
// ordinary evolve). Otherwise it walks the supersession chain to its head and
// returns an ErrEvolveTargetSuperseded naming it, so the caller can evolve the
// current version instead. Every abnormal chain — a forgotten or missing head,
// an existing fork, a cycle — is also an error: none of them may gain another
// successor silently.
//
// Why reject rather than transparently evolve the head: the caller wrote new
// content against the text of the version they hold. If a newer version
// exists, their edit may be stale relative to it; redirecting would overwrite
// that newer version with content written without seeing it. Recall's own
// head resolution (resolveSupersessionHead) is view-gated and abstains on
// ambiguity, which is the right shape for reading and the wrong one for a
// silent write redirect. One error, naming the head, is unambiguous.
//
// This is a writer's walk, deliberately NOT gated by recall visibility: the
// question is "does this memory have a successor in storage", not "may this
// caller see it". The cost for an ordinary target is one reverse-association
// read.
func (e *Engine) refuseSupersededEvolveTarget(ctx context.Context, ws [8]byte, target storage.ULID) error {
	succ, err := e.distinctSuperseders(ctx, ws, target)
	if err != nil {
		return fmt.Errorf("evolve: check for an existing successor: %w", err)
	}
	if len(succ) == 0 {
		return nil
	}
	refuse := func(detail string) error {
		return fmt.Errorf("%w: memory %s already has a successor, and evolving it again would fork its version history. %s",
			ErrEvolveTargetSuperseded, target.String(), detail)
	}
	visited := map[storage.ULID]bool{target: true}
	cur := target
	for depth := 0; depth < supersessionMaxDepth; depth++ {
		if len(succ) > 1 {
			ids := make([]string, len(succ))
			for i, s := range succ {
				ids[i] = s.String()
			}
			// A soft forget does NOT repair a fork: the forgotten branch keeps
			// its supersedes link and recall's head walk still sees two
			// successors (and abstains as ambiguous). Only a hard forget
			// removes the branch and its link. Pinned end to end by
			// TestEvolve_ForkRepairAdviceWorks.
			return refuse(fmt.Sprintf("Its history is already forked at %s (successors: %s). To repair it, permanently delete the branch you do not want with muninn_forget(id, hard=true) — a soft forget leaves its supersedes link and the fork in place — then evolve the surviving version.",
				cur.String(), strings.Join(ids, ", ")))
		}
		next := succ[0]
		if visited[next] {
			return refuse(fmt.Sprintf("Its supersession chain has a cycle at %s.", next.String()))
		}
		visited[next] = true
		eng, err := e.store.GetEngram(ctx, ws, next)
		if err != nil || eng == nil {
			return refuse(fmt.Sprintf("Its successor %s no longer exists; remember the new content as a new memory instead.", next.String()))
		}
		cur = next
		succ, err = e.distinctSuperseders(ctx, ws, cur)
		if err != nil {
			return fmt.Errorf("evolve: walk successor chain: %w", err)
		}
		if len(succ) > 0 {
			continue // an intermediate; the head is further up
		}
		if eng.State != storage.StateActive {
			return refuse(fmt.Sprintf("Its current version %s has been forgotten (state %s); restore %s with muninn_restore and evolve it, or remember the new content as a new memory.",
				cur.String(), eng.State.String(), cur.String()))
		}
		return refuse(fmt.Sprintf("Its current version is %s — evolve %s instead (read it first: your edit may predate it).", cur.String(), cur.String()))
	}
	return refuse(fmt.Sprintf("Its successor chain is longer than %d hops; the last version reached is %s.", supersessionMaxDepth, cur.String()))
}
