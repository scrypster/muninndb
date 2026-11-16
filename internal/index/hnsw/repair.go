package hnsw

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// RebuildStatus reports a vault's graph repair (load.go explains when one
// runs). Exposed through admin observability.
type RebuildStatus struct {
	// State: queued (waiting for another vault's repair to finish), running,
	// done, cancelled (vault reset or shutdown; the next load starts over),
	// failed (the swap's write failed; the loaded graph is still served).
	State            string    `json:"state"`
	Kind             string    `json:"kind"` // full | partial
	Vectors          int       `json:"vectors"`
	ClosedNodes      int       `json:"closed_nodes"`      // at load
	UnreachableNodes int       `json:"unreachable_nodes"` // at load
	Done             int       `json:"done"`              // vectors re-inserted so far
	Total            int       `json:"total"`             // vectors to re-insert
	ReplayedInserts  int       `json:"replayed_inserts"`  // inserts that landed during the repair
	EscapeEdges      int       `json:"escape_edges"`      // edges added to make the result navigable (see ensureNavigable)
	StartedAt        time.Time `json:"started_at"`
	DurationSeconds  float64   `json:"duration_seconds,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// repairState is the per-index bookkeeping for at most one repair at a time.
type repairState struct {
	mu       sync.Mutex
	status   *RebuildStatus
	cancel   context.CancelFunc
	ctx      context.Context // the running repair's context (tests observe cancellation)
	done     chan struct{}
	progress atomic.Int64
	// replayedUnderLock counts inserts the final, write-locked replay pass had
	// to apply (test observation: the earlier passes should leave it ~0).
	replayedUnderLock atomic.Int64
}

// RebuildStatus returns this index's repair status, or nil when no repair
// has been needed since it was loaded.
func (idx *Index) RebuildStatus() *RebuildStatus {
	idx.repair.mu.Lock()
	defer idx.repair.mu.Unlock()
	if idx.repair.status == nil {
		return nil
	}
	s := *idx.repair.status
	if s.State == "running" {
		s.Done = int(idx.repair.progress.Load())
		s.DurationSeconds = time.Since(s.StartedAt).Seconds()
	}
	return &s
}

func (idx *Index) setRepairStatus(f func(s *RebuildStatus)) {
	idx.repair.mu.Lock()
	defer idx.repair.mu.Unlock()
	if idx.repair.status == nil {
		idx.repair.status = &RebuildStatus{}
	}
	f(idx.repair.status)
}

// stopRepair cancels this index's repair, if one is queued or running, and
// waits for it to stop. A cancelled repair writes nothing.
func (idx *Index) stopRepair() {
	idx.repair.mu.Lock()
	cancel, done := idx.repair.cancel, idx.repair.done
	idx.repair.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// waitRepair waits for this index's repair, if any, to finish on its own.
func (idx *Index) waitRepair() {
	idx.repair.mu.Lock()
	done := idx.repair.done
	idx.repair.mu.Unlock()
	if done != nil {
		<-done
	}
}

var errRepairCancelled = errors.New("hnsw: repair cancelled")

// runRepair builds the repaired graph on a scratch index, then replaces the
// served graph with it in one step and persists it in one atomic batch.
//
// While it builds, searches and inserts use the loaded graph exactly as
// before; an insert that lands meanwhile is replayed into the scratch graph
// (id order) before the swap. The swap holds the index's write lock, drains
// in-flight neighbour-list writes, then commits EVERY node's lists — and the
// deletion of layers a node no longer has — in one synced Pebble batch. A
// crash before the commit leaves the loaded graph on disk (the scratch graph
// has no database: nothing is written while it builds), so the next load
// assesses it again and starts over; a crash after leaves the new graph. Never
// a mix. The rebuild is deterministic for a given set of vectors (id order,
// levels from the vault-seeded RNG), so starting over yields the same graph.
//
// hook, when non-nil, is called at named stages (test seam). Returns whether
// the swap happened.
func (idx *Index) runRepair(ctx context.Context, plan repairPlan, hook func(stage string)) bool {
	start := time.Now()
	total := plan.vectors
	if plan.kind == repairPartial {
		total = len(plan.targets)
	}
	idx.repair.progress.Store(0)
	idx.setRepairStatus(func(s *RebuildStatus) {
		*s = RebuildStatus{State: "running", Kind: plan.kind.String(), Vectors: plan.vectors,
			ClosedNodes: plan.closed, UnreachableNodes: plan.unreachable, Total: total, StartedAt: start}
	})
	slog.Info("hnsw: repairing vector graph in the background (searches use the loaded graph until it is replaced)",
		"vault", idx.ws, "kind", plan.kind.String(), "vectors", plan.vectors,
		"closed_nodes", plan.closed, "unreachable_nodes", plan.unreachable, "to_insert", total)

	finish := func(state string, err error) bool {
		d := time.Since(start)
		idx.setRepairStatus(func(s *RebuildStatus) {
			s.State, s.DurationSeconds, s.Done = state, d.Seconds(), int(idx.repair.progress.Load())
			if err != nil {
				s.Error = err.Error()
			}
		})
		if state != "done" {
			slog.Warn("hnsw: graph repair did not complete; the loaded graph is still served (the next load retries)",
				"vault", idx.ws, "state", state, "error", err, "after", d)
		}
		return state == "done"
	}

	tmp, err := idx.buildRepaired(ctx, plan)
	if err != nil {
		return finish("cancelled", err)
	}
	if hook == nil {
		hook = func(string) {}
	}
	hook("built")

	// Catch up with inserts that landed while the scratch graph was built —
	// outside the write lock first, so searches are not held up by them.
	replayed := 0
	for round := 0; round < 3; round++ {
		idx.mu.RLock()
		pending := idx.notIn(tmp)
		idx.mu.RUnlock()
		if len(pending) == 0 {
			break
		}
		for _, n := range pending {
			tmp.insert(n.id, n.vec, false)
		}
		replayed += len(pending)
	}
	hook("replayed")

	idx.mu.Lock()
	hook("precommit")
	if ctx.Err() != nil {
		idx.mu.Unlock()
		return finish("cancelled", errRepairCancelled)
	}
	for _, n := range idx.notIn(tmp) {
		tmp.insert(n.id, n.vec, false)
		replayed++
		idx.repair.replayedUnderLock.Add(1)
	}
	escapes := tmp.ensureNavigable()
	// Writes queued by inserts on the served graph must land BEFORE the batch,
	// or a late one would overwrite a new list with an old one.
	idx.drainPersists(hook)
	if idx.db != nil {
		if err := idx.commitGraph(tmp); err != nil {
			idx.mu.Unlock()
			return finish("failed", err)
		}
	}
	hook("committed")
	idx.nodes = tmp.nodes
	idx.entryPoint = tmp.entryPoint
	idx.maxLevel = tmp.maxLevel
	idx.dups = tmp.dups
	idx.mu.Unlock()
	idx.setRepairStatus(func(s *RebuildStatus) { s.ReplayedInserts, s.EscapeEdges = replayed, escapes })
	if escapes > 0 {
		slog.Warn("hnsw: repaired graph needed escape edges to be fully navigable (nodes that could not reach the entry point, or be reached from it, after re-insertion)",
			"vault", idx.ws, "escape_edges", escapes)
	}

	idx.mu.RLock()
	after := assessGraph(idx.nodes, idx.entryPoint, idx.legacy)
	idx.mu.RUnlock()
	ok := finish("done", nil)
	if !idx.legacy && (after.closed > 0 || after.unreachable > 0) {
		slog.Error("hnsw: repaired graph still has closed or unreachable nodes after escape edges (a defect); the next load will repair it again",
			"vault", idx.ws, "closed_nodes", after.closed, "unreachable_nodes", after.unreachable)
	}
	slog.Info("hnsw: background graph repair finished; searches now use the repaired graph",
		"vault", idx.ws, "kind", plan.kind.String(), "vectors", after.vectors,
		"inserted", idx.repair.progress.Load(), "replayed_inserts", replayed,
		"closed_nodes_after", after.closed, "unreachable_after", after.unreachable,
		"duration", time.Since(start))
	return ok
}

// buildRepaired builds the scratch graph for plan from a snapshot of the
// served one. The snapshot shares vectors (immutable) but never lists.
func (idx *Index) buildRepaired(ctx context.Context, plan repairPlan) (*Index, error) {
	idx.mu.RLock()
	snap := make(map[[16]byte]*HNSWNode, len(idx.nodes))
	for id, n := range idx.nodes {
		snap[id] = n
	}
	entry, maxLevel := idx.entryPoint, idx.maxLevel
	idx.mu.RUnlock()

	tmp := &Index{
		nodes:          make(map[[16]byte]*HNSWNode, len(snap)),
		ws:             idx.ws,
		rng:            rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(idx.ws[:])))),
		efConstruction: idx.efConstruction,
		efSearch:       idx.efSearch,
		legacy:         idx.legacy,
	}
	step := func(i int) error {
		if i%256 == 0 && ctx.Err() != nil {
			return errRepairCancelled
		}
		idx.repair.progress.Add(1)
		return nil
	}
	switch plan.kind {
	case repairFull:
		ids := make([][16]byte, 0, len(snap))
		for id, n := range snap {
			if len(n.vec) > 0 {
				ids = append(ids, id)
			}
		}
		sortIDs(ids)
		for i, id := range ids {
			if err := step(i); err != nil {
				return nil, err
			}
			tmp.insert(id, snap[id].vec, false)
		}
	case repairPartial:
		for id, n := range snap {
			c := &HNSWNode{id: id, vec: n.vec, norm: n.norm}
			n.mu.RLock()
			c.layers = make([][][16]byte, len(n.layers))
			for l, lst := range n.layers {
				c.layers[l] = append([][16]byte(nil), lst...)
			}
			n.mu.RUnlock()
			tmp.nodes[id] = c
		}
		tmp.entryPoint, tmp.maxLevel = entry, maxLevel
		if !tmp.legacy {
			tmp.dups = buildDupGroups(tmp.nodes)
		}
		for i, id := range plan.targets {
			if err := step(i); err != nil {
				return nil, err
			}
			old := tmp.nodes[id]
			if old == nil || len(old.vec) == 0 || id == tmp.entryPoint {
				continue
			}
			delete(tmp.nodes, id)
			tmp.insert(id, old.vec, false)
		}
	}
	return tmp, nil
}

// drainPersists waits for in-flight neighbour-list writes. Caller holds
// idx.mu (write), so no new one can start.
func (idx *Index) drainPersists(hook func(string)) {
	hook("drain-wait")
	idx.persistWg.Wait()
}

// ensureNavigable is a repair's POSTCONDITION, enforced rather than hoped
// for: every node with a vector can reach the entry point and be reached
// from it over layer-0 edges. The next load assesses exactly these two
// properties, so a repaired graph can never be repaired again for the same
// reason — the repair loop is impossible by construction, decided from the
// graph itself with nothing persisted. Neither property has been seen to
// fail after a rebuild once identical vectors are ringed; this is the guard
// if one ever does.
//
//   - A closed node gets an out-edge to the nearest node that can reach the
//     entry (found by a layer-0 search from the entry; the entry itself if
//     none), replacing its last — back-filled, least diverse — slot when the
//     list is full. Removing an out-edge from a node that now reaches the
//     entry directly cannot strand anything.
//   - An unreachable node gets an in-edge from the nearest reachable node
//     with a free slot, or one whose last slot points at a node with another
//     in-edge.
//
// Returns the number of edges added. Caller has exclusive use of idx (a
// scratch index).
func (idx *Index) ensureNavigable() int {
	if idx.legacy || len(idx.nodes) < 2 {
		return 0
	}
	added := 0
	for pass := 0; pass < 3; pass++ {
		closed := closedNodes(idx.nodes, idx.entryPoint)
		fwd := bfsReachableSet(idx.nodes, idx.entryPoint)
		var unreach [][16]byte
		for id, n := range idx.nodes {
			if len(n.vec) > 0 && !fwd[id] {
				unreach = append(unreach, id)
			}
		}
		if len(closed) == 0 && len(unreach) == 0 {
			return added
		}
		ids := make([][16]byte, 0, len(closed))
		for id := range closed {
			ids = append(ids, id)
		}
		for _, c := range sortIDs(ids) {
			n := idx.nodes[c]
			target := idx.entryPoint
			for _, cand := range idx.searchLayer(idx.entryPoint, n.vec, idx.efC(), 0) {
				if cand.id != c && !closed[cand.id] {
					target = cand.id
					break
				}
			}
			if len(n.layers) == 0 {
				n.layers = [][][16]byte{nil}
			}
			if len(n.layers[0]) < M0 {
				n.layers[0] = append(n.layers[0], target)
			} else {
				n.layers[0][len(n.layers[0])-1] = target
			}
			added++
		}
		indeg := map[[16]byte]int{}
		for _, n := range idx.nodes {
			if len(n.layers) > 0 {
				for _, nb := range n.layers[0] {
					indeg[nb]++
				}
			}
		}
		for _, u := range sortIDs(unreach) {
			un := idx.nodes[u]
			for _, cand := range idx.searchLayer(idx.entryPoint, un.vec, idx.efC(), 0) {
				r := idx.nodes[cand.id]
				if cand.id == u || r == nil || !fwd[cand.id] {
					continue
				}
				if len(r.layers) == 0 {
					r.layers = [][][16]byte{nil}
				}
				l0 := r.layers[0]
				if len(l0) < M0 {
					r.layers[0] = append(l0, u)
				} else if last := l0[len(l0)-1]; indeg[last] >= 2 {
					indeg[last]--
					l0[len(l0)-1] = u
				} else {
					continue
				}
				indeg[u]++
				added++
				break
			}
		}
	}
	return added
}

type pendingVec struct {
	id  [16]byte
	vec []float32
}

// notIn lists the served graph's vectors the scratch graph does not hold —
// inserted, or re-inserted with a new vector, since the snapshot — in id
// order. Caller holds idx.mu.
func (idx *Index) notIn(tmp *Index) []pendingVec {
	var out []pendingVec
	for id, n := range idx.nodes {
		if len(n.vec) == 0 {
			continue
		}
		t := tmp.nodes[id]
		if t == nil || len(t.vec) != len(n.vec) || &t.vec[0] != &n.vec[0] {
			out = append(out, pendingVec{id, n.vec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].id[:]) < string(out[j].id[:]) })
	return out
}

// commitGraph writes every list of tmp, and deletes every layer key a node
// held in the served graph but no longer has, in one synced batch. It first
// confirms the vault's vectors are still on disk: a vault cleared while the
// repair ran must not have its lists written back. Caller holds idx.mu
// (write) and has drained persistWg.
func (idx *Index) commitGraph(tmp *Index) error {
	if _, closer, err := idx.db.Get(keys.HNSWNodeKey(idx.ws, tmp.entryPoint, 0xFF)); err != nil {
		return fmt.Errorf("hnsw: repair not committed, entry vector unreadable (vault cleared?): %w", err)
	} else {
		_ = closer.Close()
	}
	b := idx.db.NewBatch()
	defer b.Close()
	for id, n := range tmp.nodes {
		for l, lst := range n.layers {
			if err := b.Set(keys.HNSWNodeKey(idx.ws, id, uint8(l)), encodeNeighbors(lst), nil); err != nil {
				return err
			}
		}
		if old := idx.nodes[id]; old != nil {
			for l := len(n.layers); l < len(old.layers); l++ {
				if err := b.Delete(keys.HNSWNodeKey(idx.ws, id, uint8(l)), nil); err != nil {
					return err
				}
			}
		}
	}
	return b.Commit(pebble.Sync)
}
