package hnsw

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// unitVec returns base + eps*noise, L2-normalized. With a small eps this
// produces a tight cluster on the unit sphere; with a large eps, a point
// well outside it.
func unitVec(rng *rand.Rand, base []float32, eps float32) []float32 {
	v := make([]float32, len(base))
	var norm float64
	for i := range v {
		v[i] = base[i] + eps*(rng.Float32()*2-1)
		norm += float64(v[i]) * float64(v[i])
	}
	inv := float32(1.0 / math.Sqrt(norm))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// inEdgeCount counts layer-0 edges across the graph that point at id.
func inEdgeCount(idx *Index, id [16]byte) int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	count := 0
	for nid, n := range idx.nodes {
		if nid == id {
			continue
		}
		for _, e := range n.getLayer(0) {
			if e == id {
				count++
			}
		}
	}
	return count
}

// TestInsert_DenseCluster_LateInsertsStayReachable reproduces #620's insert-time
// mechanism: when a new node's nearest neighbors all have full layer-0 lists of
// mutually-closer nodes, distance-only pruning drops the just-added back-edge at
// every neighbor. The node ends with zero in-edges, greedy search from the entry
// point can never reach it, and a memory that was just written is unfindable by
// its own embedding — silently, while FTS and status stay green.
//
// The contract asserted here is the user-visible one: a vector that was just
// inserted must be findable by searching for that same vector.
func TestInsert_DenseCluster_LateInsertsStayReachable(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ws [8]byte
	copy(ws[:], []byte("DENSE620"))
	idx := New(db, ws)
	defer idx.Close() // wait out async persists before the deferred db.Close
	rng := rand.New(rand.NewSource(620))
	ctx := context.Background()

	const dim = 32
	centroid := unitVec(rng, make([]float32, dim), 1.0) // random unit direction

	// A tight cluster, larger than layer-0 maxConn (M0=32), so every member's
	// layer-0 list is full of other members that are mutually far closer than
	// any late outsider.
	const clusterN = 40
	for i := 0; i < clusterN; i++ {
		var id [16]byte
		rng.Read(id[:])
		v := unitVec(rng, centroid, 0.01)
		if err := idx.StoreVector(id, v); err != nil {
			t.Fatal(err)
		}
		idx.Insert(id, v)
	}

	// Late arrivals: each in its own direction off the cluster — near enough
	// that their nearest neighbors are all cluster members, far enough that
	// they lose every distance-only prune contest at those members.
	const lateN = 20
	lateIDs := make([][16]byte, lateN)
	lateVecs := make([][]float32, lateN)
	for k := 0; k < lateN; k++ {
		var id [16]byte
		rng.Read(id[:])
		v := unitVec(rng, centroid, 0.35)
		lateIDs[k], lateVecs[k] = id, v
		if err := idx.StoreVector(id, v); err != nil {
			t.Fatal(err)
		}
		idx.Insert(id, v)

		// The just-written memory must be findable by its own embedding, now.
		res, err := idx.Search(ctx, v, 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range res {
			if r.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("late insert %d: just-inserted vector not findable by its own embedding (layer-0 in-edges=%d)",
				k, inEdgeCount(idx, id))
		}
	}
}

// stripInEdges removes every persisted layer reference to the victim ids from
// all other nodes, leaving the victims' own keys (vector + forward edges)
// intact. This produces on disk exactly the #620 end state: forward-only nodes
// with zero in-edges, invisible to graph search.
func stripInEdges(t *testing.T, db *pebble.DB, ws [8]byte, victims map[[16]byte]bool) {
	t.Helper()
	probe := New(db, ws)
	if err := probe.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	for id, n := range probe.nodes {
		if victims[id] {
			continue
		}
		for l := range n.layers {
			filtered := make([][16]byte, 0, len(n.layers[l]))
			changed := false
			for _, e := range n.layers[l] {
				if victims[e] {
					changed = true
					continue
				}
				filtered = append(filtered, e)
			}
			if changed {
				if err := db.Set(keys.HNSWNodeKey(ws, id, uint8(l)), encodeNeighbors(filtered), pebble.Sync); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// TestLoadFromPebble_FewOrphans_RepairedOnLoad reproduces #620's restart shape:
// a handful of unreachable nodes — far below the 50% full-rebuild threshold —
// must not stay orphaned across loads. Production logs show gaps like
// reachable_from_entry=1536 of nodes=1549 with rebuilt=false on every restart:
// the damage is permanent because nothing between "perfect" and "majority
// disconnected" ever repairs.
func TestLoadFromPebble_FewOrphans_RepairedOnLoad(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var ws [8]byte
	copy(ws[:], []byte("ORPHAN62"))
	const n, dim = 60, 16

	_, ids, vecs := buildPersistedGraph(t, db, ws, n, dim, 620)

	// Choose three level-0 victims (nodes with no persisted upper layers), so
	// stripping their in-edges makes them fully invisible to descent + beam.
	probe := New(db, ws)
	if err := probe.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	victims := map[[16]byte]bool{}
	victimVecs := map[[16]byte][]float32{}
	for i, id := range ids {
		if len(victims) == 3 {
			break
		}
		if id == probe.entryPoint {
			continue
		}
		if node := probe.nodes[id]; node != nil && len(node.layers) == 1 {
			victims[id] = true
			victimVecs[id] = vecs[i]
		}
	}
	if len(victims) != 3 {
		t.Fatalf("test setup: found %d level-0 victims, want 3", len(victims))
	}

	stripInEdges(t, db, ws, victims)

	// First reload: the orphans must come back reachable and findable.
	idx2 := New(db, ws)
	if err := idx2.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	if got := bfsReachable(idx2.nodes, idx2.entryPoint); got != n {
		t.Errorf("post-load reachable=%d/%d — small orphan set was not repaired", got, n)
	}
	ctx := context.Background()
	for id, v := range victimVecs {
		res, err := idx2.Search(ctx, v, 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range res {
			if r.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("orphaned node %x not findable by its own embedding after load", id[:4])
		}
	}

	// The repair must also persist: a second, fresh load starts healthy.
	idx2.Close()
	idx3 := New(db, ws)
	defer idx3.Close()
	if err := idx3.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	if got := bfsReachable(idx3.nodes, idx3.entryPoint); got != n {
		t.Errorf("second load reachable=%d/%d — repair was not persisted", got, n)
	}
}
