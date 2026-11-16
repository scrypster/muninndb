package hnsw

// Migration: graphs persisted by the pre-fix (nearest-only) rule are detected
// at load, served as loaded, repaired in the background and swapped in once.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// watchdog is a FAILURE DETECTOR, never a timing assertion: it turns a hang
// (the defect) into a failure instead of a stuck test binary. Every wait it
// bounds is event-driven (a stage hook closing a channel, a context being
// cancelled), so it only has to be longer than any real repair. It was 20s,
// and a full repair of the 400-vector fixture under -race -cover on a 4-vCPU
// runner took 15-27s: the guard tests failed with "repair never reached stage"
// while the repair was still making progress. Two minutes still ends a real
// hang well inside the package's 300s timeout.
const watchdog = 2 * time.Minute

// oldRuleFixture is a corpus the old (nearest-only) rule islands on every
// seed and vault measured: 320 topic vectors plus one cluster of 80
// near-identical ones, dim 64 (old-rule graphs: 120–339 closed nodes; new
// rule: 0). Small, because the migration tests build it many times and CI
// runs them under -race.
func oldRuleFixture(seed int64) *corpus { return makeCorpus("dup80", 64, 400, seed) }

// persistCorpus stores and inserts c into ws on db under the given rule and
// waits for every list to be written.
func persistCorpus(t *testing.T, c *corpus, db *pebble.DB, ws [8]byte, legacy bool) *Index {
	t.Helper()
	return buildCorpus(t, c, db, ws, func(idx *Index) { idx.legacy = legacy })
}

// graphKeys returns every 0x07 layer key (not the 0xFF vectors) of ws and its
// value: the persisted graph, byte for byte.
func graphKeys(t *testing.T, db *pebble.DB, ws [8]byte) map[string][]byte {
	t.Helper()
	lo := append([]byte{0x07}, ws[:]...)
	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		t.Fatal(err)
	}
	it, err := db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: append([]byte{0x07}, wsPlus[:]...)})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	out := map[string][]byte{}
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) >= 26 && k[25] == 0xFF {
			continue
		}
		out[string(k)] = append([]byte(nil), it.Value()...)
	}
	return out
}

func sameGraphKeys(a, b map[string][]byte) (missing, extra, changed int) {
	for k, v := range a {
		w, ok := b[k]
		if !ok {
			missing++
		} else if !bytes.Equal(v, w) {
			changed++
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			extra++
		}
	}
	return
}

// sameStructure reports how many nodes' layer lists differ between a and b.
func sameStructure(a, b *Index) int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	b.mu.RLock()
	defer b.mu.RUnlock()
	differ := 0
	for id, an := range a.nodes {
		bn := b.nodes[id]
		if bn == nil || len(an.layers) != len(bn.layers) {
			differ++
			continue
		}
		for l := range an.layers {
			if !sameIDs(an.layers[l], bn.layers[l]) {
				differ++
				break
			}
		}
	}
	return differ + abs(len(a.nodes)-len(b.nodes))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// stageGate blocks a repair at one named stage until released, and reports
// when the repair reached it.
type stageGate struct {
	stage   string
	reached chan struct{}
	release chan struct{}
}

func newStageGate(stage string) *stageGate {
	return &stageGate{stage: stage, reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *stageGate) hook(_ [8]byte, stage string) {
	if stage == g.stage {
		close(g.reached)
		<-g.release
	}
}

func (g *stageGate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(watchdog):
		t.Fatal("repair never reached stage " + g.stage)
	}
}

func searchAll(t *testing.T, reg *Registry, ws [8]byte, qs []query, k int) [][][16]byte {
	t.Helper()
	out := make([][][16]byte, len(qs))
	for i, q := range qs {
		r, err := reg.Search(context.Background(), ws, q.q, k)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range r {
			out[i] = append(out[i], x.ID)
		}
	}
	return out
}

func countSame(a, b [][][16]byte) int {
	n := 0
	for i := range a {
		if sameIDs(a[i], b[i]) {
			n++
		}
	}
	return n
}

func meanRecall(c *corpus, qs []query, got [][][16]byte) float64 {
	sum := 0.0
	for i, q := range qs {
		want, _ := exactTopK(c, q.q, 10, nil)
		var g []ScoredID
		for _, id := range got[i] {
			g = append(g, ScoredID{ID: id})
		}
		sum += recallStrict(g, want)
	}
	return sum / float64(len(qs))
}

// TestMigration_OldRuleGraphServedThenReplacedOnce: a graph persisted by the
// old rule is served exactly as loaded — deterministically — while the repair
// builds; the repaired graph replaces it in one step, is persisted complete
// (no stale layers), and the next load needs no repair: results change once.
func TestMigration_OldRuleGraphServedThenReplacedOnce(t *testing.T) {
	// Hermetic (its own seeded corpus, store and index; no package state), so it
	// runs in parallel and its -race cost overlaps the package's other heavy tests.
	t.Parallel()
	for _, tc := range []struct {
		name string
		seed int64
		want repairKind
	}{
		{"seed-3", 3, repairFull},
		{"seed-7", 7, repairFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := oldRuleFixture(tc.seed)
			qs := makeQueries(c, 100, 21)
			db := memPebble(t)
			defer db.Close()
			var ws [8]byte
			copy(ws[:], "MIGRATE1")
			persistCorpus(t, c, db, ws, true)
			before := graphKeys(t, db, ws)

			// What the loaded (unrepaired) graph answers, from an index that
			// only loads.
			probe := New(db, ws)
			plan, err := probe.load()
			if err != nil {
				t.Fatal(err)
			}
			if plan.kind != tc.want {
				t.Fatalf("old-rule graph assessed %s (closed %d, unreachable %d of %d), want %s",
					plan.kind, plan.closed, plan.unreachable, plan.vectors, tc.want)
			}
			var oldAnswers [][][16]byte
			for _, q := range qs {
				oldAnswers = append(oldAnswers, topIDs(t, probe, q.q, 10))
			}

			gate := newStageGate("built")
			reg := NewRegistry(db)
			reg.repairHook = gate.hook
			first := searchAll(t, reg, ws, qs, 10)
			gate.wait(t)
			during := searchAll(t, reg, ws, qs, 10)
			if s := reg.RebuildStatus(ws); s == nil || s.State != "running" || s.Kind != tc.want.String() {
				t.Errorf("status during repair = %+v, want running %s", s, tc.want)
			}
			if n := countSame(oldAnswers, first); n != len(qs) {
				t.Errorf("before the swap, %d of %d answers equal the loaded graph's", n, len(qs))
			}
			if n := countSame(first, during); n != len(qs) {
				t.Errorf("while the repair ran, %d of %d repeated searches were identical", n, len(qs))
			}
			close(gate.release)
			reg.WaitRepairs()
			if s := reg.RebuildStatus(ws); s == nil || s.State != "done" {
				t.Fatalf("status after repair = %+v, want done", s)
			}
			idx := reg.get(ws)
			if n := closedAgainstEntry(idx); n != 0 {
				t.Errorf("%d closed nodes after the repair", n)
			}
			after := searchAll(t, reg, ws, qs, 10)
			rOld, rNew := meanRecall(c, qs, first), meanRecall(c, qs, after)
			t.Logf("recall@10 against exact: loaded graph %.3f, repaired %.3f", rOld, rNew)
			if rNew < 0.95 {
				t.Errorf("repaired graph recall@10 %.3f < 0.95", rNew)
			}
			if _, _, changed := sameGraphKeys(before, graphKeys(t, db, ws)); changed == 0 {
				t.Error("repair swapped in memory but persisted nothing")
			}
			reg.Close()

			// Next start: the persisted graph IS the repaired one.
			reg2 := NewRegistry(db)
			again := searchAll(t, reg2, ws, qs, 10)
			reg2.WaitRepairs()
			if s := reg2.RebuildStatus(ws); s != nil {
				t.Errorf("second load started a repair (%+v): the repair did not persist a healthy graph", s)
			}
			if d := sameStructure(idx, reg2.get(ws)); d != 0 {
				t.Errorf("reloaded graph differs from the swapped-in graph at %d nodes (stale or missing lists)", d)
			}
			if n := countSame(after, again); n != len(qs) {
				t.Errorf("after a restart %d of %d answers equal the repaired graph's", n, len(qs))
			}
			reg2.Close()
		})
	}
}

// TestMigration_OldRuleBootLoopEnds: the trunk rebuilt an islanded vault with
// the same rule on every load, re-creating the islands each time. One repair
// must end it.
func TestMigration_OldRuleBootLoopEnds(t *testing.T) {
	c := oldRuleFixture(3)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "BOOTLOOP")
	persistCorpus(t, c, db, ws, true)
	for boot := 1; boot <= 3; boot++ {
		idx := New(db, ws)
		plan, err := idx.load()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("boot %d: %s (closed %d, unreachable %d of %d)", boot, plan.kind, plan.closed, plan.unreachable, plan.vectors)
		if boot == 1 && plan.kind == repairNone {
			t.Fatal("setup: the old-rule graph needs no repair")
		}
		if boot > 1 && (plan.kind != repairNone || plan.closed != 0 || plan.unreachable != 0) {
			t.Errorf("boot %d: %s repair, %d closed, %d unreachable: the repair did not end the islands",
				boot, plan.kind, plan.closed, plan.unreachable)
		}
		if plan.kind != repairNone {
			idx.runRepair(context.Background(), plan, nil)
		}
		idx.Close()
	}
}

// TestMigration_CancelledRepairWritesNothing: a repair stopped before its
// swap (vault reset, shutdown — or a crash, since nothing is written before
// the commit) leaves the persisted graph exactly as it was; the next start
// assesses it again and completes.
func TestMigration_CancelledRepairWritesNothing(t *testing.T) {
	c := oldRuleFixture(7)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "CANCEL01")
	persistCorpus(t, c, db, ws, true)
	before := graphKeys(t, db, ws)

	gate := newStageGate("built")
	reg := NewRegistry(db)
	reg.repairHook = gate.hook
	_ = reg.VaultVectors(ws)
	gate.wait(t)
	// Mid-repair, the scratch graph is fully built: nothing of it may be on
	// disk yet.
	if missing, extra, changed := sameGraphKeys(before, graphKeys(t, db, ws)); missing+extra+changed != 0 {
		t.Errorf("before the commit the persisted graph already differs: %d missing, %d extra, %d changed keys (a crash here would leave a mix)",
			missing, extra, changed)
	}
	// Cancel first — exactly what Close and ResetVault do before waiting —
	// and only then let the repair continue, so it meets the cancellation
	// deterministically instead of racing it to the commit.
	idx := reg.get(ws)
	idx.repair.mu.Lock()
	cancel := idx.repair.cancel
	idx.repair.mu.Unlock()
	cancel()
	close(gate.release)
	stopped := make(chan struct{})
	go func() { reg.Close(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(watchdog):
		t.Fatal("Close did not stop the repair")
	}
	if missing, extra, changed := sameGraphKeys(before, graphKeys(t, db, ws)); missing+extra+changed != 0 {
		t.Errorf("a cancelled repair changed the persisted graph: %d missing, %d extra, %d changed keys", missing, extra, changed)
	}
	if s := reg.RebuildStatus(ws); s == nil || s.State != "cancelled" {
		t.Errorf("status = %+v, want cancelled", s)
	}

	reg2 := NewRegistry(db)
	_ = reg2.VaultVectors(ws)
	reg2.WaitRepairs()
	if s := reg2.RebuildStatus(ws); s == nil || s.State != "done" {
		t.Fatalf("restarted repair status = %+v, want done", s)
	}
	if n := closedAgainstEntry(reg2.get(ws)); n != 0 {
		t.Errorf("%d closed nodes after the restarted repair", n)
	}
	reg2.Close()
}

// TestMigration_InsertsDuringRepairSurvive: vectors inserted while the
// repair builds are searchable at once (old graph) and after the swap, and
// are persisted with the repaired graph.
func TestMigration_InsertsDuringRepairSurvive(t *testing.T) {
	c := oldRuleFixture(7)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "INSERT01")
	persistCorpus(t, c, db, ws, true)

	gate := newStageGate("built")
	reg := NewRegistry(db)
	reg.repairHook = gate.hook
	_ = reg.VaultVectors(ws)
	gate.wait(t)

	r := newRand(99)
	type nv struct {
		id  [16]byte
		vec []float32
	}
	var added []nv
	for i := 0; i < 20; i++ {
		var id [16]byte
		r.Read(id[:])
		v := cNear(r, c.vecs[r.Intn(len(c.vecs))], 0.2)
		if err := reg.Insert(context.Background(), ws, id, v); err != nil {
			t.Fatal(err)
		}
		added = append(added, nv{id, v})
	}
	findable := func(reg *Registry, label string) {
		t.Helper()
		miss := 0
		for _, a := range added {
			res, err := reg.Search(context.Background(), ws, a.vec, 10)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, x := range res {
				found = found || x.ID == a.id
			}
			if !found {
				miss++
			}
		}
		if miss > 0 {
			t.Errorf("%s: %d of %d vectors inserted during the repair not findable by their own vector", label, miss, len(added))
		}
	}
	close(gate.release)
	reg.WaitRepairs()
	findable(reg, "after the swap")
	if n := reg.VaultVectors(ws); n != len(c.vecs)+len(added) {
		t.Errorf("index holds %d vectors after the swap, want %d", n, len(c.vecs)+len(added))
	}
	reg.Close()
	reg2 := NewRegistry(db)
	findable(reg2, "after a restart")
	reg2.WaitRepairs()
	if s := reg2.RebuildStatus(ws); s != nil {
		t.Errorf("restart after the swap started a repair: %+v", s)
	}
	reg2.Close()
}

// TestMigration_RepairDoesNotHoldRegistryLock: while one vault's repair runs,
// another vault's first access (load) and searches proceed.
func TestMigration_RepairDoesNotHoldRegistryLock(t *testing.T) {
	db := memPebble(t)
	defer db.Close()
	var wsOld, wsOther [8]byte
	copy(wsOld[:], "LOCKOLD1")
	copy(wsOther[:], "LOCKNEW1")
	persistCorpus(t, oldRuleFixture(7), db, wsOld, true)
	other := makeCorpus("uniform", 32, 200, 8)
	persistCorpus(t, other, db, wsOther, false)

	gate := newStageGate("built")
	reg := NewRegistry(db)
	reg.repairHook = gate.hook
	go func() { _ = reg.VaultVectors(wsOld) }()
	gate.wait(t)
	got := make(chan int, 1)
	go func() {
		res, _ := reg.Search(context.Background(), wsOther, other.vecs[0], 5)
		got <- len(res)
	}()
	select {
	case n := <-got:
		if n == 0 {
			t.Error("search on the other vault returned nothing")
		}
	case <-time.After(watchdog):
		t.Error("another vault's first search waited for this vault's repair (registry lock held)")
	}
	close(gate.release)
	reg.WaitRepairs()
	reg.Close()
}

// TestFormat_ListsReadableByBothRules: the on-disk format is unchanged; only
// list contents differ. A graph written by the new rule loads under the old
// rule's load checks with no repair and identical structure; a graph written
// by the old rule loads under the new code as a valid (if islanded) graph,
// identical to what was written, before any repair.
func TestFormat_ListsReadableByBothRules(t *testing.T) {
	c := oldRuleFixture(13)
	for _, written := range []string{"new", "old"} {
		t.Run("written-by-"+written, func(t *testing.T) {
			db := memPebble(t)
			defer db.Close()
			var ws [8]byte
			copy(ws[:], "FORMAT01")
			live := persistCorpus(t, c, db, ws, written == "old")
			reader := New(db, ws)
			reader.legacy = written == "new" // read with the OTHER rule
			plan, err := reader.load()
			if err != nil {
				t.Fatal(err)
			}
			if d := sameStructure(live, reader); d != 0 {
				t.Errorf("loaded lists differ from the written ones at %d nodes", d)
			}
			if written == "new" && plan.kind != repairNone {
				t.Errorf("old-rule load checks want a %s repair of a new-rule graph", plan.kind)
			}
			if written == "new" {
				miss := 0
				for i := 0; i < len(c.vecs); i += 10 {
					found := false
					for _, id := range topIDs(t, reader, c.vecs[i], 10) {
						found = found || id == c.ids[i]
					}
					if !found {
						miss++
					}
				}
				if miss > 0 {
					t.Errorf("old-rule reader: %d of %d sampled vectors not findable by their own vector", miss, len(c.vecs)/10)
				}
			}
			reader.Close()
		})
	}
}

// TestMigration_OrphansRelinkedInBackground: a healthy (new-rule) graph with a
// few nodes whose in-edges were lost is repaired by re-inserting just those,
// in the background, and served meanwhile.
func TestMigration_OrphansRelinkedInBackground(t *testing.T) {
	c := makeCorpus("uniform", 16, 60, 620)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "ORPHANBG")
	persistCorpus(t, c, db, ws, false)
	probe := New(db, ws)
	if _, err := probe.load(); err != nil {
		t.Fatal(err)
	}
	victims := map[[16]byte]bool{}
	for _, id := range c.ids {
		if n := probe.nodes[id]; len(victims) < 3 && id != probe.entryPoint && n != nil && len(n.layers) == 1 {
			victims[id] = true
		}
	}
	stripInEdges(t, db, ws, victims)

	reg := NewRegistry(db)
	_ = reg.VaultVectors(ws)
	reg.WaitRepairs()
	s := reg.RebuildStatus(ws)
	if s == nil || s.Kind != "partial" || s.State != "done" || s.Total != 3 {
		t.Fatalf("status = %+v, want a done partial repair of 3", s)
	}
	idx := reg.get(ws)
	if got := bfsReachable(idx.nodes, idx.entryPoint); got != len(c.vecs) {
		t.Errorf("reachable %d of %d after the re-link", got, len(c.vecs))
	}
	reg.Close()
	reg2 := NewRegistry(db)
	_ = reg2.VaultVectors(ws)
	reg2.WaitRepairs()
	if s := reg2.RebuildStatus(ws); s != nil {
		t.Errorf("restart after the re-link started another repair: %+v", s)
	}
	reg2.Close()
}

// TestMeasure_MigrationCost measures, on a real-disk store of old-rule graphs
// (the realistic mix, dim 384): first access to the vault (load; the repair
// is not on this path), the background rebuild's duration and throughput,
// the extra heap it holds at its peak (the scratch graph, measured when it is
// complete, plus the commit batch), and the second start.
//
//	HNSW_HARNESS=1 [HNSW_STORE_DIR=dir] go test -tags localassets -run TestMeasure_MigrationCost -v ./internal/index/hnsw/
func TestMeasure_MigrationCost(t *testing.T) {
	if os.Getenv("HNSW_HARNESS") == "" {
		t.Skip("set HNSW_HARNESS=1")
	}
	dir := os.Getenv("HNSW_STORE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	var ws [8]byte
	copy(ws[:], "MEASURE1")
	for _, n := range []int{11500, 16500} {
		src := filepath.Join(dir, fmt.Sprintf("old-%d", n))
		if _, err := os.Stat(src); err != nil {
			db, err := pebble.Open(src, &pebble.Options{})
			if err != nil {
				t.Fatal(err)
			}
			persistCorpus(t, makeCorpus("mix", 384, n, 99), db, ws, true)
			db.Close()
		}
		work := filepath.Join(t.TempDir(), "work")
		if out, err := exec.Command("cp", "-R", src, work).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		db, err := pebble.Open(work, &pebble.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var m runtime.MemStats
		heap := func() int64 { runtime.GC(); runtime.ReadMemStats(&m); return int64(m.HeapAlloc) }

		h0 := heap()
		probe := New(db, ws)
		plan, err := probe.load()
		if err != nil {
			t.Fatal(err)
		}
		loaded := heap() - h0
		runtime.KeepAlive(probe)
		probe = nil

		h0 = heap()
		var atBuilt int64
		reg := NewRegistry(db)
		reg.repairHook = func(_ [8]byte, stage string) {
			if stage == "built" {
				atBuilt = heap()
			}
		}
		runtime.ReadMemStats(&m)
		alloc0 := m.TotalAlloc
		st := time.Now()
		_ = reg.VaultVectors(ws)
		first := time.Since(st)
		reg.WaitRepairs()
		runtime.ReadMemStats(&m)
		s := reg.RebuildStatus(ws)
		if s == nil || s.State != "done" {
			t.Fatalf("N=%d: status %+v", n, s)
		}
		idx := reg.get(ws)
		var batch int64
		idx.mu.RLock()
		for _, nd := range idx.nodes {
			for _, l := range nd.layers {
				batch += 26 + 2 + 16*int64(len(l))
			}
		}
		after := assessGraph(idx.nodes, idx.entryPoint, false)
		idx.mu.RUnlock()
		fmt.Printf("MIGRATE N=%d plan=%s closed_at_load=%d first_access=%.3fs rebuild=%.1fs (%.0f vectors/s) served_graph=%.1fMB scratch_graph_extra=%.1fMB batch=%.1fMB alloc_during=%.0fMB closed_after=%d\n",
			n, plan.kind, plan.closed, first.Seconds(), s.DurationSeconds, float64(s.Vectors)/s.DurationSeconds,
			float64(loaded)/1e6, float64(atBuilt-h0-loaded)/1e6, float64(batch)/1e6, float64(m.TotalAlloc-alloc0)/1e6, after.closed)
		reg.Close()

		for boot := 2; boot <= 3; boot++ {
			reg2 := NewRegistry(db)
			st = time.Now()
			_ = reg2.VaultVectors(ws)
			el := time.Since(st)
			reg2.WaitRepairs()
			fmt.Printf("MIGRATE N=%d boot=%d first_access=%.3fs repair=%v\n", n, boot, el.Seconds(), reg2.RebuildStatus(ws) != nil)
			reg2.Close()
		}
		db.Close()
	}
}

// TestRegistry_RebuildVaultOnDemand: an old-rule graph with no islands is
// left alone at load (nothing to repair) but keeps its nearest-only lists; an
// operator-requested rebuild replaces them, once, and the next start needs
// nothing.
func TestRegistry_RebuildVaultOnDemand(t *testing.T) {
	c := makeCorpus("uniform", 32, 300, 31)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "ONDEMAND")
	old := persistCorpus(t, c, db, ws, true)
	reg := NewRegistry(db)
	_ = reg.VaultVectors(ws)
	reg.WaitRepairs()
	if s := reg.RebuildStatus(ws); s != nil {
		t.Fatalf("setup: the old-rule graph was repaired at load: %+v", s)
	}
	if err := reg.RebuildVault(ws); err != nil {
		t.Fatal(err)
	}
	if err := reg.RebuildVault(ws); err != nil && err != ErrRebuildInProgress {
		t.Errorf("second request: %v", err)
	}
	reg.WaitRepairs()
	if s := reg.RebuildStatus(ws); s == nil || s.State != "done" || s.Kind != "full" {
		t.Fatalf("status after the requested rebuild = %+v", s)
	}
	if d := sameStructure(old, reg.get(ws)); d == 0 {
		t.Error("the requested rebuild left every list as the old rule wrote it")
	}
	rebuilt := reg.get(ws)
	reg.Close()
	reg2 := NewRegistry(db)
	_ = reg2.VaultVectors(ws)
	reg2.WaitRepairs()
	if s := reg2.RebuildStatus(ws); s != nil {
		t.Errorf("restart after the rebuild repaired again: %+v", s)
	}
	if d := sameStructure(rebuilt, reg2.get(ws)); d != 0 {
		t.Errorf("reloaded graph differs from the rebuilt one at %d nodes", d)
	}
	reg2.Close()
}
