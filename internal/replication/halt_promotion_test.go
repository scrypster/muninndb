package replication

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestPromotionDuringARefusedJoinLeavesNoRecord is the interleaving the second
// review found. A follower's join is in flight to a leader that will refuse
// it. Before the answer arrives this node takes the leader role. Then the
// answer lands. Its verdict was on a follower's history, and this node's
// history is the cluster's now: it must not be recorded.
//
// Before the fix the record was removed in a separate step after the role
// flip, and the late answer wrote it back. The leader then refused to apply
// anything (ErrApplierHalted), a restart brought it up refused although its
// history was the cluster's, and the next leader, holding that same history,
// refused it.
func TestPromotionDuringARefusedJoinLeavesNoRecord(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repLog := NewReplicationLog(db)
	applier := NewApplier(db)
	for seq := uint64(1); seq <= 20; seq++ {
		if err := applier.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatal(err)
		}
	}
	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClusterCoordinator(&config.ClusterConfig{Enabled: true, NodeID: "node-f", BindAddr: "127.0.0.1:1",
		Seeds: []string{"127.0.0.1:2"}, ClusterSecret: "invented-test-cluster-secret", Role: "replica",
		HeartbeatMS: 50, SDOWNBeats: 1_000_000, PruneIntervalSec: 3600}, repLog, applier, es)

	cConn, sConn := net.Pipe()
	asked := make(chan struct{})
	answer := make(chan struct{})
	go func() {
		defer sConn.Close()
		readJoinRequest(t, sConn)
		close(asked)
		<-answer
		sendJoinResponse(t, sConn, mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 4,
			LeaderHeadSeq: headPtr(30), Diverged: true, RejectReason: aheadRejectPrefix + ": invented"})
	}()
	joined := make(chan error, 1)
	go func() {
		_, err := c.joinClient.joinConn(context.Background(), cConn)
		joined <- err
	}()
	<-asked

	// This node wins an election while the answer is in flight.
	c.election.mu.Lock()
	c.takeLeaderRole(5)
	c.election.mu.Unlock()
	close(answer)
	<-joined

	if !c.IsLeader() {
		t.Fatal("precondition: the node does not lead")
	}
	if rec, onDisk := haltOnDisk(db); onDisk {
		t.Fatalf("the leader holds a refusal record from the term before its own: refused by %s in epoch %d at %d", rec.LeaderID, rec.Epoch, rec.LastApplied)
	}
	if _, halted := applier.Halt(); halted {
		t.Fatal("the leader's applier holds a refusal in memory")
	}
	if err := applier.Apply(otherLeadersEntry(21, "old-leader")); errors.Is(err, ErrApplierHalted) {
		t.Fatal("the leader's applier refuses to apply")
	}

	// The next leader followed this node's term, so it holds the same history.
	// It must admit this node.
	m := newMatchLeader(t)
	m.follow(0, 20, "old-leader")
	m.follow(20, 30, "node-f")
	m.promote()
	m.write(5)
	_, s, _ := applier.LastAppliedMark()
	if _, refused, _ := m.h.refuseIfAhead(mbp.JoinRequest{NodeID: "node-f", LastApplied: 21, LastAppliedSum: binary.BigEndian.AppendUint32(nil, s)}); refused {
		t.Error("a node whose history is the cluster's is refused by the next leader")
	}

	// Once it steps down, a refusal from a LATER term is recorded again. One
	// from its own term or an earlier one is not: it was answered before this
	// node led (TestOldTermRefusalAfterPromoteAndDemoteIsNotRecorded).
	c.handleDemotion(causeClaim)
	later := inventedHalt(21)
	later.Epoch = 6
	if err := applier.SetHalt(later); err != nil {
		t.Errorf("a refusal from epoch 6 is not recorded after the node, which led epoch 5, stepped down: %v", err)
	}
}

// TestOldTermRefusalAfterPromoteAndDemoteIsNotRecorded (the delta review's
// probe). A join answered in the term before this node was promoted can
// arrive after it has been promoted AND demoted again. Checking the role alone
// let it through: the record landed on a node whose history had moved on, and
// the next leader, holding that history, refused it. The record is refused by
// term: this node led epoch 5, so a verdict from epoch 4 is about a node that
// no longer exists.
func TestOldTermRefusalAfterPromoteAndDemoteIsNotRecorded(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := NewApplier(db)
	for seq := uint64(1); seq <= 20; seq++ {
		if err := a.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatal(err)
		}
	}
	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClusterCoordinator(&config.ClusterConfig{Enabled: true, NodeID: "node-f", BindAddr: "127.0.0.1:1",
		Seeds: []string{"127.0.0.1:2"}, ClusterSecret: "invented-test-cluster-secret", Role: "replica",
		HeartbeatMS: 50, SDOWNBeats: 1_000_000, PruneIntervalSec: 3600}, NewReplicationLog(db), a, es)
	cConn, sConn := net.Pipe()
	asked, answer := make(chan struct{}), make(chan struct{})
	go func() {
		defer sConn.Close()
		readJoinRequest(t, sConn)
		close(asked)
		<-answer
		sendJoinResponse(t, sConn, mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 4,
			LeaderHeadSeq: headPtr(30), Diverged: true, RejectReason: aheadRejectPrefix + ": invented"})
	}()
	done := make(chan error, 1)
	go func() { _, err := c.joinClient.joinConn(context.Background(), cConn); done <- err }()
	<-asked
	c.election.mu.Lock()
	c.takeLeaderRole(5)
	c.election.mu.Unlock()
	c.handleDemotion(causeClaim)
	close(answer)
	<-done
	if rec, onDisk := haltOnDisk(db); onDisk {
		t.Fatalf("a refusal from epoch %d, answered before this node led epoch 5, was recorded after it stepped down", rec.Epoch)
	}
	if _, halted := a.Halt(); halted {
		t.Fatal("the stale refusal is held in memory")
	}
}

// TestUpholdHalt_NeverWritesAPlaceholder (the delta review's probe). A read of
// the refusal key that fails at startup halts the node with a placeholder. If
// a leader then cannot confirm the node, the note of that (upholdHalt) must not
// write the placeholder to disk: a read that failed once would become a
// refusal that outlasts a restart on a healthy disk, and its remedy would send
// an operator to re-seed a healthy replica.
func TestUpholdHalt_NeverWritesAPlaceholder(t *testing.T) {
	for name, get := range map[string]func([]byte) ([]byte, io.Closer, error){
		"the read failed": func([]byte) ([]byte, io.Closer, error) {
			return nil, nil, errors.New("invented transient read failure")
		},
		"the value is unusable": func([]byte) ([]byte, io.Closer, error) { return []byte{0xff, 0x00}, io.NopCloser(nil), nil },
	} {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			a := NewApplier(db)
			a.halt = loadHalt(get)
			if a.halt == nil {
				t.Fatal("precondition: the load did not halt the node")
			}
			if _, ok, err := a.upholdHalt("node-x", 7); !ok || err != nil {
				t.Fatalf("upholdHalt = (ok %v, err %v), want the placeholder to stand", ok, err)
			}
			if _, onDisk := haltOnDisk(db); onDisk {
				t.Fatal("a placeholder for a record that could not be read was written to disk")
			}
		})
	}
}

// TestUpholdHalt_WritesNothingWhileLeading pins upholdHalt's leading guard.
// The record is removed as the node takes the role, so the guard matters only
// when that removal failed on disk and the record stayed: a late answer from
// another leader must not then write to it.
func TestUpholdHalt_WritesNothingWhileLeading(t *testing.T) {
	db := newTestDB(t)
	a := NewApplier(db)
	if err := a.SetHalt(inventedHalt(3)); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.leading = true // as after a takeLead whose delete failed
	a.mu.Unlock()
	if _, ok, _ := a.upholdHalt("node-x", 7); ok {
		t.Error("upholdHalt reported a record it may update while the node leads")
	}
	if rec, _ := haltOnDisk(db); rec.UpheldBy != "" {
		t.Errorf("upholdHalt wrote %q into the record while the node leads", rec.UpheldBy)
	}
}

// TestUpholdHalt_DoesNotWriteBackARecordThatWasRemoved: the update a refused
// node makes when another leader cannot confirm it (which term was asked) is
// one step under the applier's lock. A record removed in the meantime stays
// removed.
func TestUpholdHalt_DoesNotWriteBackARecordThatWasRemoved(t *testing.T) {
	db := newTestDB(t)
	a := NewApplier(db)
	if err := a.SetHalt(inventedHalt(3)); err != nil {
		t.Fatal(err)
	}
	if rec, ok, err := a.upholdHalt("node-x", 7); !ok || err != nil || rec.UpheldBy != "node-x" || rec.UpheldEpoch != 7 {
		t.Fatalf("upholdHalt = (%+v, %v, %v)", rec, ok, err)
	}
	if _, _, err := a.takeLead(1, func(uint64) {}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.upholdHalt("node-y", 8); ok {
		t.Error("upholdHalt wrote a record back after the node took the leader role")
	}
	if _, onDisk := haltOnDisk(db); onDisk {
		t.Error("a record is on disk after the node took the leader role")
	}
}

// TestLoadHalt_AReadThatFailsHalts: "the key existing is the refusal", and not
// being able to tell whether it exists is not the same as its being absent.
func TestLoadHalt_AReadThatFailsHalts(t *testing.T) {
	rec := loadHalt(func([]byte) ([]byte, io.Closer, error) { return nil, nil, errors.New("invented read failure") })
	if rec == nil {
		t.Fatal("a refusal record that could not be read was taken for none")
	}
	if rec2 := loadHalt(func([]byte) ([]byte, io.Closer, error) { return nil, nil, pebble.ErrNotFound }); rec2 != nil {
		t.Errorf("an absent record was read as %+v", rec2)
	}
}

// countingRefuser is a stand-in leader that refuses every join and counts
// them.
func countingRefuser(t *testing.T) (addr string, joins *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	joins = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				frame, err := mbp.ReadFrame(conn)
				if err != nil || frame.Type != mbp.TypeJoinRequest {
					return // a discovery hello: this stand-in is not a peer
				}
				joins.Add(1)
				payload, _ := msgpack.Marshal(mbp.JoinResponse{Accepted: false, CortexID: "node-leader", Epoch: 2,
					RejectReason: aheadRejectPrefix + ": invented", LeaderHeadSeq: headPtr(3), Diverged: true})
				_ = mbp.WriteFrame(conn, &mbp.Frame{Version: 0x01, Type: mbp.TypeJoinResponse, PayloadLength: uint32(len(payload)), Payload: payload})
			}()
		}
	}()
	return ln.Addr().String(), joins
}

// TestJoinWithRetry_ReturnsOnTheFirstRefusal: a refusal is not a failure to
// retry. The answer cannot improve, and a retry against an older leader that
// admits everything would go around the refusal. joinWithRetry returns it at
// once.
func TestJoinWithRetry_ReturnsOnTheFirstRefusal(t *testing.T) {
	addr, joins := countingRefuser(t)
	coord, _ := newTestCoordinator(t, "replica")
	applyThrough(t, coord, 0, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := coord.joinWithRetry(ctx, []string{addr, addr, addr}, "lobe")
	if !errors.Is(err, ErrFollowerAhead) {
		t.Fatalf("joinWithRetry returned %v, want ErrFollowerAhead", err)
	}
	if n := joins.Load(); n != 1 {
		t.Errorf("the refused node asked %d times, want once", n)
	}
}

// TestHaltedFollower_DoesNotReAskTheSameTerm: once refused by the leader it
// knows from that leader's claim, a follower waits for the leader or the epoch
// to change before it asks again. It must not ask the same leader in a loop.
// The claim is delivered as peer discovery delivers it in a running cluster;
// the refusal itself never tells the node who leads
// (TestHaltedFollower_ARefusalDoesNotMoveTheEpochOrTheLeader).
func TestHaltedFollower_DoesNotReAskTheSameTerm(t *testing.T) {
	addr, joins := countingRefuser(t)
	coord, _ := newTestCoordinator(t, "replica")
	coord.cfg.Seeds = []string{addr}
	coord.haltRetry = 20 * time.Millisecond
	applyThrough(t, coord, 0, 5)
	coord.election.HandleCortexClaim(mbp.CortexClaim{CortexID: "node-leader", Epoch: 2, FencingToken: 2})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, func() bool { _, h := coord.ReplicationHalt(); return h }, func() string { return "never refused" })

	// Fifty retry intervals in the same term.
	quiet := time.NewTimer(50 * coord.haltRetry)
	defer quiet.Stop()
	<-quiet.C
	if n := joins.Load(); n != 1 {
		t.Errorf("in the same term the refused node asked %d times, want once", n)
	}
}

// TestTakeLead_TheRecordIsGoneBeforeTheRoleFlips: anything that sees this node
// as the leader must find no refusal record on its disk. takeLead removes the
// record before it runs the function that flips the role.
func TestTakeLead_TheRecordIsGoneBeforeTheRoleFlips(t *testing.T) {
	db := newTestDB(t)
	a := NewApplier(db)
	if err := a.SetHalt(inventedHalt(3)); err != nil {
		t.Fatal(err)
	}
	var onDiskAtFlip bool
	was, removed, err := a.takeLead(1, func(uint64) {
		_, closer, gerr := db.Get(replHaltKey())
		if gerr == nil {
			closer.Close()
		}
		onDiskAtFlip = !errors.Is(gerr, pebble.ErrNotFound)
	})
	if err != nil || !removed || was.LeaderID != "node-cortex" {
		t.Fatalf("takeLead = (%+v, %v, %v)", was, removed, err)
	}
	if onDiskAtFlip {
		t.Error("the refusal record was still on disk when the role flipped")
	}
}
