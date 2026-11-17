package replication

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// A follower whose position is past the end of its leader's log (STO-27): the
// part of the refusal that needs no checksum. The leader refuses to admit it,
// the follower refuses to follow, and the follower does not change its mind
// when the leader's log later grows past it. join_match_test.go covers the
// case positions cannot show.

func headPtr(v uint64) *uint64 { return &v }

// newAheadTestHandler returns a join handler for a Cortex at the given epoch
// whose log head is head.
func newAheadTestHandler(t *testing.T, epoch, head uint64) *JoinHandler {
	t.Helper()
	// One in-memory store: these cases need no disk, and two on-disk stores
	// per case cost about 0.3 s each under -race.
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.Advance(epoch); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	log := NewReplicationLog(db)
	if _, err := log.AdvanceTo(head); err != nil {
		t.Fatalf("seed log head: %v", err)
	}
	return NewJoinHandler("node-cortex", "", es, log, NewConnManager("node-cortex"))
}

// TestJoinHandler_RefusesOnlyALobeAheadOfItsLogHead walks the boundary on the
// Cortex side: at the head and behind it are admitted, one past it is not.
func TestJoinHandler_RefusesOnlyALobeAheadOfItsLogHead(t *testing.T) {
	cases := []struct {
		name        string
		head        uint64
		lastApplied uint64
		refused     bool
	}{
		{name: "lobe exactly at the head", head: 40, lastApplied: 40, refused: false},
		{name: "lobe behind the head", head: 40, lastApplied: 33, refused: false},
		{name: "lobe that has applied nothing", head: 40, lastApplied: 0, refused: false},
		{name: "lobe one entry ahead", head: 40, lastApplied: 41, refused: true},
		{name: "lobe far ahead", head: 40, lastApplied: 36557, refused: true},
		{name: "empty log, lobe that has applied nothing", head: 0, lastApplied: 0, refused: false},
		{name: "empty log, lobe that has applied anything", head: 0, lastApplied: 1, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAheadTestHandler(t, 2, tc.head)
			resp := h.HandleJoinRequest(mbp.JoinRequest{
				NodeID: "node-lobe", Addr: "127.0.0.1:1", LastApplied: tc.lastApplied,
				ProtocolVersion: mbp.CurrentProtocolVersion,
			}, nil)

			if resp.Accepted == tc.refused {
				t.Fatalf("Accepted = %v for a lobe at %d against head %d (reason %q)",
					resp.Accepted, tc.lastApplied, tc.head, resp.RejectReason)
			}
			// Either way the Cortex states its head, and states it exactly:
			// an empty log reports 0, it does not report nothing.
			if resp.LeaderHeadSeq == nil {
				t.Fatalf("response carries no LeaderHeadSeq; a head of %d must be reported", tc.head)
			}
			if *resp.LeaderHeadSeq != tc.head {
				t.Errorf("LeaderHeadSeq = %d, want %d", *resp.LeaderHeadSeq, tc.head)
			}
			_, member := memberByID(h, "node-lobe")
			if tc.refused {
				if !strings.HasPrefix(resp.RejectReason, aheadRejectPrefix) {
					t.Errorf("RejectReason = %q, want it to start with %q", resp.RejectReason, aheadRejectPrefix)
				}
				if resp.Epoch != 2 || resp.CortexID != "node-cortex" {
					t.Errorf("refusal names epoch %d cortex %q, want 2 and node-cortex", resp.Epoch, resp.CortexID)
				}
				if member {
					t.Error("a refused lobe was recorded as a member")
				}
			} else if !member {
				t.Error("an admitted lobe was not recorded as a member")
			}
		})
	}
}

func memberByID(h *JoinHandler, id string) (NodeInfo, bool) {
	for _, m := range h.Members() {
		if m.NodeID == id {
			return m, true
		}
	}
	return NodeInfo{}, false
}

// TestJoinHandler_AheadLobeIsAdmittedWhenASnapshotWillReplaceItsState: a
// handler that snapshots every joiner overwrites whatever the joiner held, so
// being ahead is not a reason to refuse it. (No production handler is built
// this way today; the rule must still be right when one is.)
func TestJoinHandler_AheadLobeIsAdmittedWhenASnapshotWillReplaceItsState(t *testing.T) {
	db := newTestDB(t)
	es := newTestEpochStore(t)
	if _, err := es.Advance(2); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	h := NewJoinHandlerWithDB("node-cortex", "", es, NewReplicationLog(db), db, NewConnManager("node-cortex"))

	resp := h.HandleJoinRequest(mbp.JoinRequest{
		NodeID: "node-lobe", LastApplied: 36557, ProtocolVersion: mbp.CurrentProtocolVersion,
	}, nil)
	if !resp.Accepted || !resp.NeedsSnapshot {
		t.Fatalf("Accepted=%v NeedsSnapshot=%v reason=%q, want an accepted join with a snapshot",
			resp.Accepted, resp.NeedsSnapshot, resp.RejectReason)
	}
}

// serveJoin hands one JoinRequest to coord the way ServeConn does and returns
// what came back on the wire.
func serveJoin(t *testing.T, coord *ClusterCoordinator, req mbp.JoinRequest) (resp mbp.JoinResponse, adopted bool, lobeEnd net.Conn) {
	t.Helper()
	payload, err := msgpack.Marshal(req)
	if err != nil {
		t.Fatalf("marshal JoinRequest: %v", err)
	}
	cortexEnd, lobeEnd := net.Pipe()
	t.Cleanup(func() { cortexEnd.Close(); lobeEnd.Close() })

	type answer struct {
		resp mbp.JoinResponse
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		frame, err := mbp.ReadFrame(lobeEnd)
		if err != nil {
			got <- answer{err: err}
			return
		}
		var a answer
		a.err = msgpack.Unmarshal(frame.Payload, &a.resp)
		got <- a
	}()

	_, adopted, err = coord.HandleIncomingJoin(cortexEnd, payload)
	if err != nil {
		t.Fatalf("HandleIncomingJoin: %v", err)
	}
	a := <-got
	if a.err != nil {
		t.Fatalf("read JoinResponse: %v", a.err)
	}
	return a.resp, adopted, lobeEnd
}

// newLeaderForJoinTest returns a coordinator that is leader at epoch 2 with a
// log head of head.
func newLeaderForJoinTest(t *testing.T, head int) *ClusterCoordinator {
	t.Helper()
	coord, _ := newTestCoordinator(t, "primary")
	if _, err := coord.epochStore.Advance(2); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	simulatePromotion(coord, 2)
	fillLog(t, coord.repLog, head)
	return coord
}

// TestHandleIncomingJoin_AheadLobeIsRefusedBeforeItsConnectionIsTouched is the
// coordinator's half. The refusal has to leave the lobe's existing connection
// alone: that connection carries its heartbeats and its votes, and a replica
// that cannot replicate must still count toward quorum. Registering the join
// conn first — what an admitted join does — would close it.
func TestHandleIncomingJoin_AheadLobeIsRefusedBeforeItsConnectionIsTouched(t *testing.T) {
	coord := newLeaderForJoinTest(t, 10)

	// The lobe's standing peer connection, as discovery establishes it.
	standingCortexEnd, standingLobeEnd := net.Pipe()
	t.Cleanup(func() { standingCortexEnd.Close(); standingLobeEnd.Close() })
	standing, ok := coord.mgr.RegisterConnKind("node-ahead", "127.0.0.1:2", standingCortexEnd, kindHello, true)
	if !ok {
		t.Fatal("could not register the standing connection")
	}

	resp, adopted, _ := serveJoin(t, coord, mbp.JoinRequest{
		NodeID: "node-ahead", Addr: "127.0.0.1:2", LastApplied: 15,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	})

	if adopted {
		t.Error("the join connection of a refused lobe was adopted")
	}
	if resp.Accepted {
		t.Fatalf("a lobe at 15 was admitted by a Cortex whose log head is 10")
	}
	if !strings.HasPrefix(resp.RejectReason, aheadRejectPrefix) {
		t.Errorf("RejectReason = %q, want it to start with %q", resp.RejectReason, aheadRejectPrefix)
	}
	if resp.LeaderHeadSeq == nil || *resp.LeaderHeadSeq != 10 {
		t.Errorf("LeaderHeadSeq = %v, want 10", resp.LeaderHeadSeq)
	}
	if now, _ := coord.mgr.GetPeer("node-ahead"); now != standing || !standing.IsConnected() {
		t.Error("refusing the join replaced or closed the lobe's standing connection")
	}
	if _, member := memberByID(coord.joinHandler, "node-ahead"); member {
		t.Error("a refused lobe was recorded as a member")
	}
	coord.streamersMu.Lock()
	_, streaming := coord.streamers["node-ahead"]
	coord.streamersMu.Unlock()
	if streaming {
		t.Error("a replication stream was started to a refused lobe")
	}
}

// TestHandleIncomingJoin_LobeAtTheHeadIsAdmitted is the control for the test
// above: the same join, one position lower, goes through the ordinary path.
func TestHandleIncomingJoin_LobeAtTheHeadIsAdmitted(t *testing.T) {
	coord := newLeaderForJoinTest(t, 10)

	resp, adopted, lobeEnd := serveJoin(t, coord, mbp.JoinRequest{
		NodeID: "node-level", Addr: "127.0.0.1:3", LastApplied: 10,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	})
	// The admitted lobe's stream starts at once; drain it so the streamer is
	// not left blocked on a pipe nobody reads.
	go func() {
		for {
			if _, err := mbp.ReadFrame(lobeEnd); err != nil {
				return
			}
		}
	}()

	if !adopted || !resp.Accepted {
		t.Fatalf("adopted=%v Accepted=%v reason=%q, want a lobe at the head admitted", adopted, resp.Accepted, resp.RejectReason)
	}
	if resp.LeaderHeadSeq == nil || *resp.LeaderHeadSeq != 10 {
		t.Errorf("LeaderHeadSeq = %v, want 10", resp.LeaderHeadSeq)
	}
	if _, member := memberByID(coord.joinHandler, "node-level"); !member {
		t.Error("an admitted lobe was not recorded as a member")
	}
}

// TestHandleIncomingJoin_AheadRefusalDoesNotAnswerAnUnauthenticatedJoiner: the
// refusal discloses the Cortex's log head, so it is given only to a joiner that
// has proved it knows the cluster secret. Anyone else gets the ordinary
// rejection, with no head in it.
func TestHandleIncomingJoin_AheadRefusalDoesNotAnswerAnUnauthenticatedJoiner(t *testing.T) {
	coord := newLeaderForJoinTest(t, 10)
	coord.joinHandler.clusterSecret = "invented-test-cluster-secret"

	resp, _, _ := serveJoin(t, coord, mbp.JoinRequest{
		NodeID: "node-stranger", Addr: "127.0.0.1:4", LastApplied: 15,
		SecretHash:      []byte("not the hmac"),
		ProtocolVersion: mbp.CurrentProtocolVersion,
	})
	if resp.Accepted {
		t.Fatal("a joiner with a wrong secret was admitted")
	}
	if resp.RejectReason != "invalid cluster secret" {
		t.Errorf("RejectReason = %q, want the ordinary secret rejection", resp.RejectReason)
	}
	if resp.LeaderHeadSeq != nil {
		t.Errorf("an unauthenticated joiner was told the log head (%d)", *resp.LeaderHeadSeq)
	}
}

// newAheadTestClient returns a join client whose applier has applied through
// lastApplied.
func newAheadTestClient(t *testing.T, lastApplied uint64) *JoinClient {
	t.Helper()
	// In memory, as newAheadTestHandler: nothing here needs a disk.
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if lastApplied > 0 {
		seedLastApplied(t, db, lastApplied)
	}
	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return NewJoinClient("node-lobe", "127.0.0.1:5", "", es, NewApplier(db), NewConnManager("node-lobe"))
}

// TestJoinClient_RefusesACortexWhoseHeadIsBehindIt walks the same boundary on
// the follower's side, from the head the Cortex reports.
func TestJoinClient_RefusesACortexWhoseHeadIsBehindIt(t *testing.T) {
	const lastApplied = 20
	cases := []struct {
		name      string
		resp      mbp.JoinResponse
		wantAhead bool
		wantErr   bool
	}{
		{
			name: "head equal to this node's position",
			resp: mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(20)},
		},
		{
			name: "head past this node's position",
			resp: mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(25)},
		},
		{
			name:      "accepted, but the head is one short of this node",
			resp:      mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(19)},
			wantAhead: true, wantErr: true,
		},
		{
			name: "the Cortex refused and said why",
			resp: mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(19),
				RejectReason: aheadRejectPrefix + ": invented"},
			wantAhead: true, wantErr: true,
		},
		{
			name:      "a head of zero is a head, not an absent one",
			resp:      mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(0)},
			wantAhead: true, wantErr: true,
		},
		{
			name: "no head reported: a Cortex from before the field existed is joined as before",
			resp: mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2},
		},
		{
			name:    "an ordinary rejection stays an ordinary rejection",
			resp:    mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 2, RejectReason: "not cortex"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newAheadTestClient(t, lastApplied)
			conn, reqCh := mockCortexJoin(t, tc.resp)
			_, err := client.joinConn(context.Background(), conn)
			if req := <-reqCh; req.LastApplied != lastApplied {
				t.Fatalf("JoinRequest.LastApplied = %d, want %d", req.LastApplied, lastApplied)
			}

			if (err != nil) != tc.wantErr {
				t.Fatalf("joinConn error = %v, wantErr %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrFollowerAhead); got != tc.wantAhead {
				t.Fatalf("errors.Is(err, ErrFollowerAhead) = %v, want %v (err: %v)", got, tc.wantAhead, err)
			}
			if tc.wantAhead {
				// An operator has to be able to act on the message alone.
				for _, want := range []string{"20", "node-b", "docs/cluster-operations.md"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error does not mention %q: %v", want, err)
					}
				}
				if _, registered := client.mgr.GetPeer("node-b"); registered {
					t.Error("the refused Cortex was registered as a peer")
				}
			}
		})
	}
}

// TestAheadOfLeader_SnapshotAndUnknownHeadAreNotAhead: the two cases where a
// low or missing head proves nothing.
func TestAheadOfLeader_SnapshotAndUnknownHeadAreNotAhead(t *testing.T) {
	if _, ahead := aheadOfLeader(mbp.JoinResponse{LeaderHeadSeq: headPtr(1), NeedsSnapshot: true}, 36557); ahead {
		t.Error("a node about to be overwritten by a snapshot was called ahead")
	}
	if _, ahead := aheadOfLeader(mbp.JoinResponse{}, 36557); ahead {
		t.Error("a node was called ahead of a Cortex that did not report its head")
	}
	if head, ahead := aheadOfLeader(mbp.JoinResponse{LeaderHeadSeq: headPtr(7)}, 8); !ahead || head != 7 {
		t.Errorf("aheadOfLeader(head 7, applied 8) = (%d, %v), want (7, true)", head, ahead)
	}
}

// TestAheadFollower_WouldSkipTheLeadersEntries is the premise the refusal
// rests on, shown directly: stream a leader's log to a follower that is three
// entries ahead of where the leader was promoted, and the follower drops the
// leader's first three writes without an error from anywhere.
func TestAheadFollower_WouldSkipTheLeadersEntries(t *testing.T) {
	const promotedAt, ahead, newEntries = 40, 3, 5

	leader, _ := newTestCoordinator(t, "replica")
	asFollower(leader)
	applyThrough(t, leader, promotedAt-1, promotedAt)
	simulatePromotion(leader, 2)

	// At promotion the Cortex can see it: the follower is past its head.
	if _, refused, _ := leader.joinHandler.refuseIfAhead(mbp.JoinRequest{NodeID: "node-ahead", LastApplied: promotedAt + ahead}); !refused {
		t.Fatalf("a lobe at %d was not refused by a Cortex promoted at %d", promotedAt+ahead, promotedAt)
	}

	appendFn := LocalAppendFunc(leader, leader.repLog)
	for i := 1; i <= newEntries; i++ {
		if err := appendFn(uint8(OpSet), []byte{'n', byte('0' + i)}, []byte("invented")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// Five writes later it no longer can, by position alone.
	if _, refused, _ := leader.joinHandler.refuseIfAhead(mbp.JoinRequest{NodeID: "node-ahead", LastApplied: promotedAt + ahead}); refused {
		t.Fatal("precondition: once the Cortex has written past the lobe, the position check passes")
	}

	follower, db := newTestCoordinator(t, "replica")
	asFollower(follower)
	applyThrough(t, follower, promotedAt+ahead-1, promotedAt+ahead)
	applied := streamTo(t, leader.repLog, follower.applier)

	if len(applied) != newEntries-ahead {
		t.Fatalf("an ahead follower applied %v; expected it to skip the leader's first %d entries and apply the last %d",
			applied, ahead, newEntries-ahead)
	}
	for i := 1; i <= ahead; i++ {
		if _, closer, err := db.Get([]byte{'n', byte('0' + i)}); err == nil {
			closer.Close()
			t.Errorf("entry %d of the new leader reached a follower that was ahead of it", i)
		} else if !errors.Is(err, pebble.ErrNotFound) {
			t.Fatalf("read: %v", err)
		}
	}
}

// TestHaltedFollower_ARefusalDoesNotMoveTheEpochOrTheLeader: a join answer is
// not authenticated. The cluster secret proves the joining node to the leader,
// not the leader to the joining node, so whoever answers the dial can say
// anything in it. A refusal is still recorded (the stated residual: a forged
// refusal stops this node until it is re-seeded, which an endpoint that can
// answer the dial could already do worse by feeding it a forged stream). But
// it must not move what the election layer believes: not the epoch, which
// nothing bounds and which a later election starts from, and not who leads.
// Those come from the leader's claim only. The stand-in here knows no secret,
// and the node it refuses has one.
func TestHaltedFollower_ARefusalDoesNotMoveTheEpochOrTheLeader(t *testing.T) {
	// A leader that turns every join away: its log ends at 3.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	const forgedEpoch = uint64(1) << 62
	var joins atomic.Int32
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
					return // a discovery hello; this stand-in is not a peer
				}
				joins.Add(1)
				payload, err := msgpack.Marshal(mbp.JoinResponse{
					Accepted: false, CortexID: "node-forger", Epoch: forgedEpoch,
					RejectReason:  aheadRejectPrefix + ": invented",
					LeaderHeadSeq: headPtr(3),
				})
				if err != nil {
					return
				}
				_ = mbp.WriteFrame(conn, &mbp.Frame{
					Version: 0x01, Type: mbp.TypeJoinResponse,
					PayloadLength: uint32(len(payload)), Payload: payload,
				})
			}()
		}
	}()

	coord, _ := newTestCoordinator(t, "replica")
	coord.cfg.Seeds = []string{ln.Addr().String()}
	coord.cfg.ClusterSecret = "invented-test-cluster-secret"
	coord.joinClient.clusterSecret = coord.cfg.ClusterSecret
	coord.haltRetry = 20 * time.Millisecond
	applyThrough(t, coord, 4, 5) // this follower has applied through 5

	if got := coord.election.CurrentLeader(); got != "" {
		t.Fatalf("precondition: the follower already knows a leader (%q)", got)
	}
	epochBefore := coord.CurrentEpoch()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// A second join means the follow loop has finished with the first refusal
	// (the node does not know this "leader" from a claim, so it asks again
	// after one retry interval): whatever it was going to take from that
	// answer, it has taken. A node that took the answer as a claim never asks
	// again, so stop waiting as soon as either has happened.
	waitUntil(t, func() bool {
		_, halted := coord.ReplicationHalt()
		tookIt := coord.election.CurrentLeader() != "" || coord.CurrentEpoch() != epochBefore
		return halted && (joins.Load() >= 2 || tookIt)
	}, func() string { return "the follower was not refused and asked again" })

	if got := coord.election.CurrentLeader(); got != "" {
		t.Errorf("leader known to the election layer = %q after an unauthenticated refusal, want none", got)
	}
	if got := coord.CurrentEpoch(); got != epochBefore {
		t.Errorf("epoch = %d after an unauthenticated refusal, want it unchanged at %d", got, epochBefore)
	}
	if coord.Role() != RoleReplica {
		t.Errorf("role = %v, want replica: a halted follower is still a follower", coord.Role())
	}
	if got := coord.applier.LastApplied(); got != 5 {
		t.Errorf("applied position = %d, want 5 (unchanged)", got)
	}
}
