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
	StartedAt        time.Time `json:"started_at"`
	DurationSeconds  float64   `json:"duration_seconds,omitempty"`
	Error            string    `json:"error,omitempty"`
}

// repairState is the per-index bookkeeping for at most one repair at a time.
type repairState struct {
	mu       sync.Mutex
	status   *RebuildStatus
	cancel   context.CancelFunc
	done     chan struct{}
	progress atomic.Int64
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
	if hook != nil {
		hook("built")
	}

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

	idx.mu.Lock()
	if ctx.Err() != nil {
		idx.mu.Unlock()
		return finish("cancelled", errRepairCancelled)
	}
	for _, n := range idx.notIn(tmp) {
		tmp.insert(n.id, n.vec, false)
		replayed++
	}
	// Writes queued by inserts on the served graph must land BEFORE the batch,
	// or a late one would overwrite a new list with an old one.
	idx.persistWg.Wait()
	if idx.db != nil {
		if err := idx.commitGraph(tmp); err != nil {
			idx.mu.Unlock()
			return finish("failed", err)
		}
	}
	idx.nodes = tmp.nodes
	idx.entryPoint = tmp.entryPoint
	idx.maxLevel = tmp.maxLevel
	idx.mu.Unlock()

	idx.mu.RLock()
	after := assessGraph(idx.nodes, idx.entryPoint, idx.legacy)
	idx.mu.RUnlock()
	ok := finish("done", nil)
	if after.closed > 0 || after.unreachable > 0 {
		slog.Warn("hnsw: repaired graph still has closed or unreachable nodes; the next load will repair it again",
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
