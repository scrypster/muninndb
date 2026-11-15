package hnsw

// One test per guard in the background repair (repair.go, registry.go): each
// fails when its guard alone is removed.

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// repairAt starts the repair of an old-rule vault and blocks it at stage.
func repairAt(t *testing.T, stage string, ws [8]byte) (*Registry, *pebble.DB, *corpus, *stageGate) {
	t.Helper()
	c := oldRuleFixture(7)
	db := memPebble(t)
	t.Cleanup(func() { db.Close() })
	persistCorpus(t, c, db, ws, true)
	gate := newStageGate(stage)
	reg := NewRegistry(db)
	reg.repairHook = gate.hook
	_ = reg.VaultVectors(ws)
	gate.wait(t)
	return reg, db, c, gate
}

// awaitCancelled waits until idx's running repair has been cancelled.
func awaitCancelled(t *testing.T, idx *Index) {
	t.Helper()
	idx.repair.mu.Lock()
	ctx := idx.repair.ctx
	idx.repair.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(watchdog):
		t.Fatal("the repair was never cancelled")
	}
}

func wipeVault(t *testing.T, db *pebble.DB, ws [8]byte) {
	t.Helper()
	lo := append([]byte{0x07}, ws[:]...)
	wsPlus, _ := keys.IncrementWSPrefix(ws)
	if err := db.DeleteRange(lo, append([]byte{0x07}, wsPlus[:]...), pebble.Sync); err != nil {
		t.Fatal(err)
	}
}

// TestGuard_ResetVaultCancelsAndWaits: ResetVault stops a repair that is
// about to commit and returns only once it has stopped; nothing is written.
func TestGuard_ResetVaultCancelsAndWaits(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GRESET01")
	reg, db, _, gate := repairAt(t, "precommit", ws)
	idx := reg.get(ws)
	before := graphKeys(t, db, ws)
	done := make(chan struct{})
	go func() { reg.ResetVault(ws); close(done) }()
	awaitCancelled(t, idx)
	close(gate.release)
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatal("ResetVault did not return")
	}
	idx.waitRepair()
	if s := idx.RebuildStatus(); s == nil || s.State != "cancelled" {
		t.Errorf("status = %+v, want cancelled", s)
	}
	if m, e, c := sameGraphKeys(before, graphKeys(t, db, ws)); m+e+c != 0 {
		t.Errorf("a reset repair wrote to the vault: %d missing, %d extra, %d changed keys", m, e, c)
	}
	reg.Close()
}

// TestGuard_ClearCannotInterleaveWithCommit: the clear protocol (BeginClear,
// delete, EndClear) stops the repair before the delete, so the cleared vault
// stays empty — the repair is waited for, not counted early.
func TestGuard_ClearCannotInterleaveWithCommit(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GCLEAR01")
	reg, db, c, gate := repairAt(t, "precommit", ws)
	idx := reg.get(ws)
	began := make(chan struct{})
	go func() { reg.BeginClear(ws); close(began) }()
	awaitCancelled(t, idx)
	close(gate.release)
	select {
	case <-began:
	case <-time.After(watchdog):
		t.Fatal("BeginClear did not return")
	}
	wipeVault(t, db, ws)
	// A search during the clear must not cache or repair the vault.
	_, _ = reg.Search(context.Background(), ws, c.vecs[0], 5)
	reg.EndClear(ws)
	idx.waitRepair()
	reg.WaitRepairs()
	if n := len(graphKeys(t, db, ws)); n != 0 {
		t.Errorf("%d graph keys written back into a cleared vault", n)
	}
	reg.Close()
}

// TestGuard_CommitRefusedWhenVaultWiped: a wipe that bypasses the clear
// protocol is still caught at the commit — the vault's vectors are gone, so
// the old lists are not written back.
func TestGuard_CommitRefusedWhenVaultWiped(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GWIPE001")
	reg, db, _, gate := repairAt(t, "precommit", ws)
	idx := reg.get(ws)
	wipeVault(t, db, ws)
	close(gate.release)
	idx.waitRepair()
	if n := len(graphKeys(t, db, ws)); n != 0 {
		t.Errorf("%d graph keys written into a wiped vault", n)
	}
	if s := idx.RebuildStatus(); s == nil || s.State != "failed" {
		t.Errorf("status = %+v, want failed", s)
	}
	reg.Close()
}

// TestGuard_CancelledBeforeCommitWritesNothing: a cancellation that arrives
// while the repair waits for the write lock is honoured at the commit.
func TestGuard_CancelledBeforeCommitWritesNothing(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GCANCEL1")
	reg, db, _, gate := repairAt(t, "precommit", ws)
	idx := reg.get(ws)
	before := graphKeys(t, db, ws)
	idx.repair.mu.Lock()
	cancel := idx.repair.cancel
	idx.repair.mu.Unlock()
	cancel()
	close(gate.release)
	idx.waitRepair()
	if m, e, c := sameGraphKeys(before, graphKeys(t, db, ws)); m+e+c != 0 {
		t.Errorf("a cancelled repair committed: %d missing, %d extra, %d changed keys", m, e, c)
	}
	reg.Close()
}

// TestGuard_InFlightListWritesDrainBeforeCommit: list writes queued by an
// insert on the served graph must land before the batch; landing after, they
// would overwrite repaired lists with old ones.
func TestGuard_InFlightListWritesDrainBeforeCommit(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GDRAIN01")
	c := oldRuleFixture(7)
	db := memPebble(t)
	defer db.Close()
	persistCorpus(t, c, db, ws, true)
	built := newStageGate("built")
	release := make(chan struct{})
	released := false
	free := func() {
		if !released {
			released = true
			close(release)
		}
	}
	reg := NewRegistry(db)
	reg.repairHook = func(w [8]byte, stage string) {
		built.hook(w, stage)
		// Whichever comes first: the drain (persists must land before the
		// commit) or the commit itself (the drain is gone; persists land
		// after it, as they would in production).
		if stage == "drain-wait" || stage == "committed" {
			free()
		}
	}
	_ = reg.VaultVectors(ws)
	built.wait(t)
	idx := reg.get(ws)
	idx.persistHook = func() { <-release }
	r := newRand(5)
	for i := 0; i < 5; i++ {
		var id [16]byte
		r.Read(id[:])
		if err := reg.Insert(context.Background(), ws, id, cNear(r, c.vecs[i], 0.2)); err != nil {
			t.Fatal(err)
		}
	}
	close(built.release)
	reg.WaitRepairs()
	idx.Close()
	reg.Close()
	re := New(db, ws)
	if _, err := re.load(); err != nil {
		t.Fatal(err)
	}
	if d := sameStructure(idx, re); d != 0 {
		t.Errorf("persisted graph differs from the swapped-in graph at %d nodes: list writes from the old graph landed after the commit", d)
	}
}

// TestGuard_ReembedDuringRepairIsReplayed: an id re-inserted with a new
// vector while the repair builds must carry the NEW vector after the swap.
func TestGuard_ReembedDuringRepairIsReplayed(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GREEMB01")
	reg, _, c, gate := repairAt(t, "built", ws)
	r := newRand(8)
	id := c.ids[3]
	nv := cUnit(cGauss(r, c.dim, 1))
	if err := reg.Insert(context.Background(), ws, id, nv); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	reg.WaitRepairs()
	idx := reg.get(ws)
	idx.mu.RLock()
	got := idx.nodes[id].vec
	idx.mu.RUnlock()
	if !sameVector(got, nv) {
		t.Error("after the swap the re-embedded id carries its OLD vector")
	}
	res, _ := reg.Search(context.Background(), ws, nv, 1)
	if len(res) == 0 || res[0].ID != id {
		t.Error("the re-embedded id is not the top hit for its new vector")
	}
	reg.Close()
}

// TestGuard_InsertJustBeforeTheLockIsReplayed: an insert landing after the
// unlocked replay passes and before the write lock is caught by the final,
// locked pass.
func TestGuard_InsertJustBeforeTheLockIsReplayed(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GLATE001")
	c := oldRuleFixture(7)
	db := memPebble(t)
	defer db.Close()
	persistCorpus(t, c, db, ws, true)
	var lateID [16]byte
	r := newRand(12)
	r.Read(lateID[:])
	lateVec := cNear(r, c.vecs[0], 0.2)
	reg := NewRegistry(db)
	reg.repairHook = func(w [8]byte, stage string) {
		if stage == "replayed" {
			if err := reg.Insert(context.Background(), w, lateID, lateVec); err != nil {
				t.Error(err)
			}
		}
	}
	_ = reg.VaultVectors(ws)
	reg.WaitRepairs()
	idx := reg.get(ws)
	idx.mu.RLock()
	n := idx.nodes[lateID]
	idx.mu.RUnlock()
	if n == nil {
		t.Fatal("a vector inserted just before the swap is missing from the swapped-in graph")
	}
	if idx.repair.replayedUnderLock.Load() != 1 {
		t.Errorf("locked replay pass applied %d inserts, want 1", idx.repair.replayedUnderLock.Load())
	}
	reg.Close()
}

// TestGuard_InsertsDuringBuildReplayedOutsideTheLock: inserts that land while
// the scratch graph builds are replayed BEFORE the write lock is taken, so
// searches are not held up by them.
func TestGuard_InsertsDuringBuildReplayedOutsideTheLock(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "GOUTSID1")
	reg, _, c, gate := repairAt(t, "built", ws)
	r := newRand(13)
	for i := 0; i < 20; i++ {
		var id [16]byte
		r.Read(id[:])
		if err := reg.Insert(context.Background(), ws, id, cNear(r, c.vecs[i], 0.2)); err != nil {
			t.Fatal(err)
		}
	}
	close(gate.release)
	reg.WaitRepairs()
	idx := reg.get(ws)
	if n := idx.repair.replayedUnderLock.Load(); n != 0 {
		t.Errorf("%d of 20 inserts were replayed under the write lock", n)
	}
	if s := idx.RebuildStatus(); s == nil || s.ReplayedInserts != 20 {
		t.Errorf("status = %+v, want 20 replayed inserts", s)
	}
	reg.Close()
}

// TestGuard_EnsureNavigableEndsIslandsAndOrphans: the repair's postcondition
// holds even for a graph the selection rule left broken — a closed island and
// an orphan, wired by hand — so the next load has no reason to repair again.
func TestGuard_EnsureNavigableEndsIslandsAndOrphans(t *testing.T) {
	c := makeCorpus("uniform", 16, 200, 21)
	var ws [8]byte
	copy(ws[:], "GNAVIG01")
	idx := buildCorpus(t, c, nil, ws, nil)
	island := map[[16]byte]bool{}
	for _, id := range c.ids[:12] {
		if id != idx.entryPoint {
			island[id] = true
		}
	}
	var members [][16]byte
	for id := range island {
		members = append(members, id)
	}
	for i, id := range members { // a ring with no way out
		idx.nodes[id].layers[0] = [][16]byte{members[(i+1)%len(members)]}
	}
	orphan := c.ids[150]
	for _, n := range idx.nodes { // nothing points at the orphan
		var keep [][16]byte
		for _, nb := range n.layers[0] {
			if nb != orphan {
				keep = append(keep, nb)
			}
		}
		n.layers[0] = keep
	}
	if p := assessGraph(idx.nodes, idx.entryPoint, false); p.closed == 0 || p.unreachable == 0 {
		t.Fatalf("setup: closed %d, unreachable %d", p.closed, p.unreachable)
	}
	added := idx.ensureNavigable()
	p := assessGraph(idx.nodes, idx.entryPoint, false)
	if p.closed != 0 || p.unreachable != 0 || p.kind != repairNone {
		t.Errorf("after ensureNavigable (%d edges): closed %d, unreachable %d, plan %s", added, p.closed, p.unreachable, p.kind)
	}
	for _, n := range idx.nodes {
		if len(n.layers[0]) > M0 {
			t.Fatalf("a list grew past M0: %d", len(n.layers[0]))
		}
	}
}
