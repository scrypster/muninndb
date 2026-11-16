package replication

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/vmihailenco/msgpack/v5"
)

// logSubscribers reports how many NetworkStreamer.Stream calls are live on a
// log: Stream subscribes on entry and unsubscribes (deferred) on return.
func logSubscribers(l *ReplicationLog) int {
	l.subsMu.Lock()
	defer l.subsMu.Unlock()
	return len(l.subs)
}

// stopInsideBubble is deferred by the synctest tests below so that a run which
// FAILS still ends cleanly. Stop cancels whatever streamers exist — one left
// alive when the bubble's root returns is reported as a deadlock in place of
// the failure — and, called here, it waits on the coordinator's WaitGroup from
// inside the bubble that last used it. newTestCoordinator's cleanup calls Stop
// again from outside; on go1.26.11 a WaitGroup.Wait from outside a bubble
// faults (nil bubble in internal/synctest.isAssociated) if the counter last
// reached zero inside one with nobody waiting.
func stopInsideBubble(c *ClusterCoordinator) { _ = c.Stop() }

// seedLogEntries appends n entries so a streamer has a backlog to read.
func seedLogEntries(t *testing.T, l *ReplicationLog, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := l.Append(OpSet, []byte{'k', byte(i)}, []byte("v")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// TestCoordinatorStop_WaitsForStreamerPastItsCancelCheck pins the ordering the
// shutdown sequence depends on: the caller of Stop closes the store next
// (cmd/muninn/server.go), so Stop must not return while a streamer goroutine
// can still read the replication log.
//
// Cancelling a streamer does not stop the read it has already committed to.
// Stream checks its context and then calls ReplicationLog.ReadSince; a cancel
// that lands between the two is seen only on the next loop. Before the fix Stop
// cancelled and returned, the store was closed, and the streamer's read hit a
// closed Pebble — "panic: pebble: closed" from NewIter when the close won, and
// "pebble: inconsistent reference count" from the iterator's Close when the
// close landed mid-read. Both were observed; the first is how this surfaced, as
// an intermittent CI failure of the whole package.
//
// The streamer is held at exactly that point by the beforeRead seam, and
// synctest.Wait makes "Stop is blocked" an observation rather than a timeout:
// it returns only once every goroutine in the bubble is durably blocked, so
// Stop has by then either returned (the bug) or parked in its join.
func TestCoordinatorStop_WaitsForStreamerPastItsCancelCheck(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	// Outside the bubble, for the reason internal/storage's
	// provenance_drain_test.go gives: an append is a Pebble Sync write, and
	// Pebble's own WaitGroups must not become the bubble's. Inside it the
	// streamer only reads.
	seedLogEntries(t, coord.repLog, 3)

	synctest.Test(t, func(t *testing.T) {
		conn := newPacedConn(1<<20, 0)
		defer conn.Close()
		coord.mgr.RegisterConn("lobe-parked", "127.0.0.1:19999", conn)
		defer stopInsideBubble(coord)

		parked := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseStreamer := func() { releaseOnce.Do(func() { close(release) }) }
		// Deferred last so it runs first: a failing run has to unpark the
		// streamer before the Stop above can join it.
		defer releaseStreamer()

		var calls atomic.Int32
		coord.streamerBeforeRead = func() {
			if calls.Add(1) == 1 {
				close(parked)
				<-release
			}
		}

		coord.startStreamerForLobe(NodeInfo{NodeID: "lobe-parked", Addr: "127.0.0.1:19999", Role: RoleReplica})
		<-parked // past its cancellation check, about to read the log

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			_ = coord.Stop()
		}()
		synctest.Wait()

		select {
		case <-stopped:
			t.Fatal("Stop returned while a streamer goroutine was still ahead of a replication-log read; " +
				"the caller closes the store next, and that read then runs against a closed Pebble")
		default:
		}

		releaseStreamer()
		<-stopped
		if n := logSubscribers(coord.repLog); n != 0 {
			t.Fatalf("Stop returned with %d streamer(s) still subscribed to the replication log, want 0", n)
		}
	})
}

// TestCoordinatorStop_LobeJoiningAfterStopGetsNoStreamer covers the streamer
// Stop cannot cancel because it does not exist yet. Stop closes every peer
// connection; a Lobe answers a dropped stream by rejoining after a short
// backoff, and nothing takes the cluster listener down before the process
// exits. A join handled after Stop used to start a fresh streamer under a
// context nobody would ever cancel, reading the log while the store closed
// underneath it.
//
// The first join is the positive control: it proves this fixture's join path
// does reach startStreamerForLobe, so the absence of a streamer after Stop is
// the latch and not a join that was refused for some other reason.
func TestCoordinatorStop_LobeJoiningAfterStopGetsNoStreamer(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	// Pebble writes stay outside the bubble (see the test above).
	if _, err := coord.epochStore.Advance(2); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Only the leader accepts joins (#533); mark this node as leader.
	coord.roleMu.Lock()
	coord.role = RolePrimary
	coord.roleMu.Unlock()
	seedLogEntries(t, coord.repLog, 3)

	joinPayload := func(nodeID string) []byte {
		t.Helper()
		payload, err := msgpack.Marshal(mbp.JoinRequest{
			NodeID:          nodeID,
			Addr:            "127.0.0.1:19998",
			ProtocolVersion: mbp.CurrentProtocolVersion,
		})
		if err != nil {
			t.Fatalf("marshal JoinRequest: %v", err)
		}
		return payload
	}
	early, late := joinPayload("lobe-early"), joinPayload("lobe-late")

	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int32
		coord.streamerBeforeRead = func() { reads.Add(1) }
		defer stopInsideBubble(coord)

		earlyConn := newPacedConn(1<<20, 0)
		defer earlyConn.Close()
		if _, adopted, err := coord.HandleIncomingJoin(earlyConn, early); err != nil || !adopted {
			t.Fatalf("fixture: join before Stop: adopted=%v err=%v", adopted, err)
		}
		synctest.Wait() // the streamer has drained the backlog and is idle
		if reads.Load() == 0 || logSubscribers(coord.repLog) != 1 {
			t.Fatalf("fixture: a join before Stop started no streamer (reads=%d subscribers=%d)",
				reads.Load(), logSubscribers(coord.repLog))
		}

		if err := coord.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		synctest.Wait()
		readsAtStop := reads.Load()

		lateConn := newPacedConn(1<<20, 0)
		defer lateConn.Close()
		if _, _, err := coord.HandleIncomingJoin(lateConn, late); err != nil {
			t.Fatalf("join after Stop: %v", err)
		}
		synctest.Wait() // a streamer, had one been started, has read the log by now

		if got := reads.Load(); got != readsAtStop {
			t.Fatalf("a streamer started after Stop read the replication log %d time(s); "+
				"nothing cancels it and nothing joins it before the store closes", got-readsAtStop)
		}
		if n := logSubscribers(coord.repLog); n != 0 {
			t.Fatalf("%d streamer(s) subscribed to the replication log after Stop, want 0", n)
		}
	})
}
