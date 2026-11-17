package replication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// inventedHalt is the record a node at position at holds after an invented
// leader refused it.
func inventedHalt(at uint64) HaltRecord {
	return HaltRecord{
		LeaderID: "node-cortex", Epoch: 2, LastApplied: at, LeaderHead: at + 5,
		Why: fmt.Sprintf("this node's entry at seq %d is not the entry cortex %q (epoch 2) holds at that position (cortex log head %d)",
			at, "node-cortex", at+5),
		At: 1_700_000_000,
	}
}

// haltOnDisk reads the refusal record the way a restarted process would.
func haltOnDisk(db *pebble.DB) (HaltRecord, bool) {
	return NewApplier(db).Halt()
}

func keyPresent(t *testing.T, db *pebble.DB, key []byte) bool {
	t.Helper()
	_, closer, err := db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	closer.Close()
	return true
}

// logBuffer collects log output for a test that asserts on a log line.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns the collected lines that contain every one of wants.
func (b *logBuffer) lines(wants ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
next:
	for _, line := range strings.Split(b.buf.String(), "\n") {
		for _, want := range wants {
			if !strings.Contains(line, want) {
				continue next
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// captureLogs redirects the default logger for the duration of the test. It
// puts the standard log package's output back as well: slog.SetDefault points
// that at the handler it is given and does not undo it when the previous
// default logger is restored.
func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
	return buf
}

// TestApplier_ARefusalIsOnDiskAndARefusedApplierAppliesNothing: the record is
// what makes a refusal outlast the process that received it, and while it is
// there the applier applies nothing, whoever hands it an entry.
func TestApplier_ARefusalIsOnDiskAndARefusedApplierAppliesNothing(t *testing.T) {
	db := newTestDB(t)
	a := NewApplier(db)
	for seq := uint64(1); seq <= 3; seq++ {
		if err := a.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatalf("apply %d: %v", seq, err)
		}
	}
	if _, halted := a.Halt(); halted {
		t.Fatal("precondition: a node that was never refused reports a refusal")
	}

	want := inventedHalt(3)
	if err := a.SetHalt(want); err != nil {
		t.Fatalf("SetHalt: %v", err)
	}
	want.Version = haltRecordVersion

	next := otherLeadersEntry(4, "new-leader")
	for name, applier := range map[string]*Applier{"the process that was refused": a, "the process after a restart": NewApplier(db)} {
		got, halted := applier.Halt()
		if !halted {
			t.Fatalf("%s: no refusal recorded", name)
		}
		if got != want {
			t.Errorf("%s: record = %+v, want %+v", name, got, want)
		}
		if err := applier.Apply(next); !errors.Is(err, ErrApplierHalted) {
			t.Errorf("%s: Apply of the next entry returned %v, want ErrApplierHalted", name, err)
		}
		if got := applier.LastApplied(); got != 3 {
			t.Errorf("%s: applied position = %d, want 3 (unchanged)", name, got)
		}
	}
	if keyPresent(t, db, next.Key) {
		t.Error("a refused applier wrote the entry it was handed")
	}

	// Lifted: the record goes from disk, and the node applies again.
	b := NewApplier(db)
	was, cleared, err := b.ClearHalt()
	if err != nil || !cleared || was != want {
		t.Fatalf("ClearHalt = (%+v, %v, %v), want the record, true, nil", was, cleared, err)
	}
	if _, cleared, err := b.ClearHalt(); cleared || err != nil {
		t.Errorf("a second ClearHalt = (cleared %v, err %v), want nothing to clear", cleared, err)
	}
	c := NewApplier(db)
	if _, halted := c.Halt(); halted {
		t.Fatal("the refusal came back after it was lifted and the process restarted")
	}
	if err := c.Apply(next); err != nil {
		t.Fatalf("apply after the refusal was lifted: %v", err)
	}
	if !keyPresent(t, db, next.Key) || c.LastApplied() != 4 {
		t.Errorf("after the refusal was lifted the next entry did not apply (position %d)", c.LastApplied())
	}
}

// TestApplier_ARecordThisBuildCannotReadStillHalts: the key existing is the
// refusal. A value from a later build, or a damaged one, must not read as "not
// refused".
func TestApplier_ARecordThisBuildCannotReadStillHalts(t *testing.T) {
	later, err := msgpack.Marshal(HaltRecord{Version: haltRecordVersion + 1, LeaderID: "node-cortex", Why: "written by a later build"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for name, val := range map[string][]byte{
		"a later format version": later,
		"not a record at all":    {0xff, 0x00, 0x13},
		"an empty value":         {},
	} {
		t.Run(name, func(t *testing.T) {
			db := newTestDB(t)
			if err := db.Set(replHaltKey(), val, pebble.Sync); err != nil {
				t.Fatalf("write: %v", err)
			}
			a := NewApplier(db)
			rec, halted := a.Halt()
			if !halted {
				t.Fatal("a node holding a record it cannot read reports that it was never refused")
			}
			for _, want := range []string{"cannot read", "docs/cluster-operations.md"} {
				if !strings.Contains(rec.text(), want) {
					t.Errorf("the reason does not mention %q: %s", want, rec.text())
				}
			}
			if err := a.Apply(otherLeadersEntry(1, "anyone")); !errors.Is(err, ErrApplierHalted) {
				t.Errorf("Apply returned %v, want ErrApplierHalted", err)
			}
		})
	}
}

// TestApplier_ResetToLiftsARefusalAndAlwaysRewritesTheCursor covers the one
// way the cursor moves without an entry being applied: a snapshot has replaced
// this node's state.
func TestApplier_ResetToLiftsARefusalAndAlwaysRewritesTheCursor(t *testing.T) {
	t.Run("the Cortex's own cursor, shipped with the snapshot, does not survive", func(t *testing.T) {
		db := newTestDB(t)
		a := NewApplier(db)
		if err := a.ResetTo(40); err != nil { // a node that took a snapshot at 40
			t.Fatalf("ResetTo: %v", err)
		}
		// It takes another one at the same sequence number. The snapshot wrote
		// the Cortex's replication metadata into this store, and that includes
		// the Cortex's own last_applied: its cursor, with a checksum.
		if err := db.Set(lastAppliedKey(), appliedMark(26, 0xdeadbeef), pebble.Sync); err != nil {
			t.Fatalf("write the shipped cursor: %v", err)
		}
		if err := a.ResetTo(40); err != nil {
			t.Fatalf("second ResetTo: %v", err)
		}
		seq, _, ok := NewApplier(db).LastAppliedMark()
		if seq != 40 || ok {
			t.Fatalf("after a restart the cursor is (%d, checksum %v), want (40, none): "+
				"the node came back with the Cortex's cursor and a checksum of an entry it never applied", seq, ok)
		}
	})

	t.Run("a refusal goes with the state it was a verdict on", func(t *testing.T) {
		db := newTestDB(t)
		a := NewApplier(db)
		for seq := uint64(1); seq <= 3; seq++ {
			if err := a.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
				t.Fatalf("apply %d: %v", seq, err)
			}
		}
		if err := a.SetHalt(inventedHalt(3)); err != nil {
			t.Fatalf("SetHalt: %v", err)
		}
		if err := a.ResetTo(10); err != nil {
			t.Fatalf("ResetTo: %v", err)
		}
		if _, halted := a.Halt(); halted {
			t.Error("the refusal is still held in memory after a snapshot replaced the state")
		}
		if _, halted := haltOnDisk(db); halted {
			t.Error("the refusal is still on disk after a snapshot replaced the state")
		}
		if err := a.Apply(otherLeadersEntry(11, "new-leader")); err != nil {
			t.Errorf("apply after the snapshot: %v", err)
		}
	})
}

// TestJoin_ASnapshotNeitherCarriesNorKeepsARefusal runs the real snapshot
// sender and receiver. The sending node holds a refusal record (a leader never
// should, so this is the filter's own test), and so does the receiving node.
// Neither survives: the first is not shipped, the second was a verdict on the
// state the snapshot replaces. And the second snapshot at the same sequence
// number must not leave the Cortex's cursor in the receiver's store.
func TestJoin_ASnapshotNeitherCarriesNorKeepsARefusal(t *testing.T) {
	cortexDB := newTestDB(t)
	cortexLog := NewReplicationLog(cortexDB)
	for i := 0; i < 3; i++ {
		if _, err := cortexLog.Append(OpSet, []byte(fmt.Sprintf("probe/own/%d", i)), []byte("invented")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	snapshotSeq := cortexLog.CurrentSeq()
	// The Cortex followed someone once: it has a cursor of its own, at 1.
	if err := cortexDB.Set(lastAppliedKey(), appliedMark(1, 0xdeadbeef), pebble.Sync); err != nil {
		t.Fatalf("write cortex cursor: %v", err)
	}
	cortexHalt, err := msgpack.Marshal(inventedHalt(1))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := cortexDB.Set(replHaltKey(), cortexHalt, pebble.Sync); err != nil {
		t.Fatalf("write cortex refusal record: %v", err)
	}

	lobeDB := newTestDB(t)
	lobeApplier := NewApplier(lobeDB)
	for seq := uint64(1); seq <= 9; seq++ {
		if err := lobeApplier.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatalf("lobe apply %d: %v", seq, err)
		}
	}
	if err := lobeApplier.SetHalt(inventedHalt(9)); err != nil {
		t.Fatalf("SetHalt: %v", err)
	}
	client := NewJoinClientWithDB("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), lobeApplier, lobeDB, NewConnManager("node-lobe"))

	for round := 1; round <= 2; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, done := mockCortexJoinWithSnapshot(t, cortexDB, cortexLog, snapshotSeq, 1)
		_, err := client.joinConn(ctx, conn)
		cancel()
		if err != nil {
			t.Fatalf("snapshot join %d: %v", round, err)
		}
		<-done

		if _, halted := lobeApplier.Halt(); halted {
			t.Fatalf("after snapshot %d the node still reports the refusal it held before", round)
		}
		if keyPresent(t, lobeDB, replHaltKey()) {
			t.Fatalf("after snapshot %d the node's store holds a refusal record: its own was kept, or the Cortex's was shipped", round)
		}
		seq, _, ok := NewApplier(lobeDB).LastAppliedMark()
		if seq != snapshotSeq || ok {
			t.Fatalf("after snapshot %d a restart reads the cursor as (%d, checksum %v), want (%d, none)", round, seq, ok, snapshotSeq)
		}
	}
}

// TestJoinHandler_ALobeThatWasRefusedBeforeIsAdmittedOnlyOnAMatch is the
// leader's half of the rule. For a lobe that has never been refused, "cannot
// compare" admits, because a wrong refusal takes a healthy replica out of
// service. For one that has, it does not: the earlier refusal was proven, and
// not being able to repeat the proof is not a reason to undo it.
func TestJoinHandler_ALobeThatWasRefusedBeforeIsAdmittedOnlyOnAMatch(t *testing.T) {
	l := newMatchLeader(t)
	followed := l.follow(0, 40, "old-leader") // applied as a follower, never logged
	l.promote()
	l.write(10) // its own entries, 41 through 50
	if err := l.log.Prune(44); err != nil {
		t.Fatalf("prune: %v", err)
	}
	const head = 50

	const (
		admitConfirmed = "admitted, last entry confirmed"
		admitBlind     = "admitted, nothing confirmed"
		refuseProof    = "refused on proof"
		refuseStands   = "refused, the earlier refusal stands"
	)
	cases := []struct {
		name          string
		at            uint64
		sum           []byte
		neverRefused  string
		refusedBefore string
	}{
		{"its last entry is the one this cortex applied at its own cursor", 40, sumBytes(followed[40]), admitConfirmed, admitConfirmed},
		{"its last entry is an entry in this cortex's log", 47, sumBytes(l.sum(47)), admitConfirmed, admitConfirmed},
		{"this cortex has pruned the entry at its position", 42, otherSum(42, "old-leader"), admitBlind, refuseStands},
		{"it sent no checksum", 47, nil, admitBlind, refuseStands},
		{"its position is in the range this cortex applied as a follower and never logged", 30, sumBytes(followed[30]), admitBlind, refuseStands},
		{"its entry is not this cortex's entry", 47, otherSum(47, "old-leader"), refuseProof, refuseProof},
		{"its position is past this cortex's log head", 60, otherSum(60, "old-leader"), refuseProof, refuseProof},
	}
	outcome := func(resp mbp.JoinResponse) string {
		switch {
		case resp.Accepted && resp.Matched:
			return admitConfirmed
		case resp.Accepted:
			return admitBlind
		case resp.Diverged && !resp.Unverified:
			return refuseProof
		case resp.Unverified && !resp.Diverged:
			return refuseStands
		}
		return fmt.Sprintf("Accepted=%v Matched=%v Diverged=%v Unverified=%v", resp.Accepted, resp.Matched, resp.Diverged, resp.Unverified)
	}
	for i, tc := range cases {
		for _, refusedBefore := range []bool{false, true} {
			want, who := tc.neverRefused, "a lobe that was never refused"
			if refusedBefore {
				want, who = tc.refusedBefore, "a lobe that was refused before"
			}
			t.Run(tc.name+"/"+who, func(t *testing.T) {
				id := fmt.Sprintf("node-lobe-%d-%v", i, refusedBefore)
				resp := l.h.HandleJoinRequest(mbp.JoinRequest{
					NodeID: id, LastApplied: tc.at, LastAppliedSum: tc.sum, Halted: refusedBefore,
					ProtocolVersion: mbp.CurrentProtocolVersion,
				}, nil)
				if got := outcome(resp); got != want {
					t.Fatalf("%s at seq %d: %s, want %s (reason %q)", who, tc.at, got, want, resp.RejectReason)
				}
				_, member := memberByID(l.h, id)
				if member != resp.Accepted {
					t.Errorf("recorded as a member = %v, accepted = %v", member, resp.Accepted)
				}
				if want != refuseStands {
					return
				}
				if !strings.HasPrefix(resp.RejectReason, unverifiedRejectPrefix) {
					t.Errorf("RejectReason = %q, want it to start with %q", resp.RejectReason, unverifiedRejectPrefix)
				}
				if !strings.Contains(resp.RejectReason, "docs/cluster-operations.md") {
					t.Errorf("RejectReason does not say where the procedure is: %q", resp.RejectReason)
				}
				if resp.LeaderHeadSeq == nil || *resp.LeaderHeadSeq != head {
					t.Errorf("LeaderHeadSeq = %v, want %d", resp.LeaderHeadSeq, head)
				}
				if resp.CortexID != "node-cortex" || resp.Epoch != 2 {
					t.Errorf("the refusal names cortex %q epoch %d, want node-cortex epoch 2", resp.CortexID, resp.Epoch)
				}
			})
		}
	}
}

// TestHandleIncomingJoin_UnconfirmedLobeIsRefusedBeforeItsConnectionIsTouched:
// a lobe that was refused before asks again after every restart and every
// election. Each time, the leader must answer before it registers the join
// connection, or it would close the connection that carries the lobe's
// heartbeats and votes for the sake of a join it is about to turn down.
func TestHandleIncomingJoin_UnconfirmedLobeIsRefusedBeforeItsConnectionIsTouched(t *testing.T) {
	coord := newLeaderForJoinTest(t, 10)
	if err := coord.repLog.Prune(8); err != nil {
		t.Fatalf("prune: %v", err)
	}

	standingCortexEnd, standingLobeEnd := net.Pipe()
	t.Cleanup(func() { standingCortexEnd.Close(); standingLobeEnd.Close() })
	standing, ok := coord.mgr.RegisterConnKind("node-refused", "127.0.0.1:2", standingCortexEnd, kindHello, true)
	if !ok {
		t.Fatal("could not register the standing connection")
	}

	req := mbp.JoinRequest{
		NodeID: "node-refused", Addr: "127.0.0.1:2",
		LastApplied: 7, LastAppliedSum: otherSum(7, "old-leader"), Halted: true,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}
	resp, adopted, _ := serveJoin(t, coord, req)
	if adopted || resp.Accepted || !resp.Unverified || resp.Diverged {
		t.Fatalf("adopted=%v Accepted=%v Unverified=%v Diverged=%v, want a refusal on the raw connection that is not a new proof",
			adopted, resp.Accepted, resp.Unverified, resp.Diverged)
	}
	if now, _ := coord.mgr.GetPeer("node-refused"); now != standing || !standing.IsConnected() {
		t.Error("refusing the join replaced or closed the lobe's standing connection")
	}
	coord.streamersMu.Lock()
	_, streaming := coord.streamers["node-refused"]
	coord.streamersMu.Unlock()
	if streaming {
		t.Error("a replication stream was started to a refused lobe")
	}
	if _, member := memberByID(coord.joinHandler, "node-refused"); member {
		t.Error("a refused lobe was recorded as a member")
	}

	// The control: the same lobe, had it never been refused, is admitted,
	// because the leader has pruned the entry and cannot compare.
	req.NodeID, req.Halted = "node-never-refused", false
	resp, adopted, _ = serveJoin(t, coord, req)
	if !adopted || !resp.Accepted || resp.Matched {
		t.Fatalf("control: adopted=%v Accepted=%v Matched=%v, want an admission that confirmed nothing", adopted, resp.Accepted, resp.Matched)
	}
}

// TestJoinClient_ARefusalStandsUntilACortexConfirmsTheLastEntry is the
// follower's half, and the half that holds whatever the leader is. Once
// refused, the node follows a leader only when that leader says it compared
// the node's last entry with its own and they are the same. Being accepted is
// not that. Neither is a different leader, a new term, or a restart.
func TestJoinClient_ARefusalStandsUntilACortexConfirmsTheLastEntry(t *testing.T) {
	const lastApplied = 20
	db := newTestDB(t)
	seedLastApplied(t, db, lastApplied)
	applier := NewApplier(db)
	client := NewJoinClient("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), applier, NewConnManager("node-lobe"))
	join := func(c *JoinClient, resp mbp.JoinResponse) (mbp.JoinRequest, error) {
		t.Helper()
		conn, reqCh := mockCortexJoin(t, resp)
		_, err := c.joinConn(context.Background(), conn)
		return <-reqCh, err
	}

	// Never refused: it does not say it was, and an ordinary acceptance is enough.
	req, err := join(client, mbp.JoinResponse{Accepted: true, CortexID: "node-a", Epoch: 1, LeaderHeadSeq: headPtr(20)})
	if err != nil || req.Halted {
		t.Fatalf("join before any refusal: err = %v, Halted = %v", err, req.Halted)
	}

	// Refused on proof: the Cortex's log ends at 15.
	if _, err := join(client, mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(15),
		RejectReason: aheadRejectPrefix + ": invented", Diverged: true}); !errors.Is(err, ErrFollowerAhead) {
		t.Fatalf("first refusal: err = %v, want ErrFollowerAhead", err)
	}
	rec, onDisk := haltOnDisk(db)
	if !onDisk || rec.LeaderID != "node-b" || rec.Epoch != 2 || rec.LastApplied != lastApplied || rec.LeaderHead != 15 {
		t.Fatalf("record on disk after the refusal = (%+v, %v), want node-b, epoch 2, position 20, head 15", rec, onDisk)
	}

	stands := []struct {
		name string
		resp mbp.JoinResponse
	}{
		{"the same cortex in the same term, its log now past this node", mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 2, LeaderHeadSeq: headPtr(500)}},
		{"a different cortex", mbp.JoinResponse{Accepted: true, CortexID: "node-c", Epoch: 2, LeaderHeadSeq: headPtr(500)}},
		{"a new term", mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 3, LeaderHeadSeq: headPtr(500)}},
		{"a cortex that reports nothing at all (an older build)", mbp.JoinResponse{Accepted: true, CortexID: "node-old", Epoch: 4}},
		{"a cortex that says it cannot confirm", mbp.JoinResponse{Accepted: false, CortexID: "node-d", Epoch: 5, LeaderHeadSeq: headPtr(500),
			RejectReason: unverifiedRejectPrefix + ": invented", Unverified: true}},
	}
	for _, tc := range stands {
		req, err := join(client, tc.resp)
		if !req.Halted {
			t.Errorf("%s: the node did not tell the cortex it had been refused before", tc.name)
		}
		if !errors.Is(err, ErrFollowerAhead) {
			t.Fatalf("%s: err = %v, want ErrFollowerAhead. The node would follow and skip every entry numbered at or below %d",
				tc.name, err, lastApplied)
		}
		for _, want := range []string{"node-b", "15", tc.resp.CortexID, "docs/cluster-operations.md", "two choices"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the error does not mention %q: %v", tc.name, want, err)
			}
		}
		rec, _ := applier.Halt()
		if rec.LeaderID != "node-b" || rec.Epoch != 2 || rec.UpheldBy != tc.resp.CortexID || rec.UpheldEpoch != tc.resp.Epoch {
			t.Errorf("%s: record = %+v, want the proof from node-b epoch 2 and upheld by %s epoch %d",
				tc.name, rec, tc.resp.CortexID, tc.resp.Epoch)
		}
	}

	// A rejection that has nothing to do with the refusal is still just that.
	if _, err := join(client, mbp.JoinResponse{Accepted: false, CortexID: "node-b", Epoch: 5, RejectReason: "not cortex"}); err == nil || errors.Is(err, ErrFollowerAhead) {
		t.Errorf("an ordinary rejection: err = %v, want an error that is not ErrFollowerAhead", err)
	}
	if _, halted := applier.Halt(); !halted {
		t.Fatal("an ordinary rejection removed the refusal")
	}

	// A restart changes nothing.
	restarted := NewJoinClient("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), NewApplier(db), NewConnManager("node-lobe"))
	if req, err := join(restarted, mbp.JoinResponse{Accepted: true, CortexID: "node-b", Epoch: 5, LeaderHeadSeq: headPtr(900)}); !errors.Is(err, ErrFollowerAhead) || !req.Halted {
		t.Fatalf("after a restart: err = %v, Halted = %v, want ErrFollowerAhead from a node that says it was refused", err, req.Halted)
	}

	// A cortex that compared the last entry and found it its own: lifted.
	if _, err := join(restarted, mbp.JoinResponse{Accepted: true, CortexID: "node-e", Epoch: 6, LeaderHeadSeq: headPtr(900), Matched: true}); err != nil {
		t.Fatalf("join to a cortex that confirmed the last entry: %v", err)
	}
	if _, halted := restarted.applier.Halt(); halted {
		t.Error("the refusal is still held after a cortex confirmed the last entry")
	}
	if _, halted := haltOnDisk(db); halted {
		t.Error("the refusal is still on disk after a cortex confirmed the last entry")
	}
	if req, err := join(restarted, mbp.JoinResponse{Accepted: true, CortexID: "node-e", Epoch: 6, LeaderHeadSeq: headPtr(900)}); err != nil || req.Halted {
		t.Errorf("the next join: err = %v, Halted = %v, want an ordinary join", err, req.Halted)
	}
}

// TestJoin_ARefusedLobeIsAdmittedAgainByACortexThatHoldsItsLastEntry runs the
// one automatic way back through both real halves. The lobe was refused by
// one leader. A later leader holds the lobe's last entry (it comes from the
// same history: a node that was level with the lobe, or a cluster rebuilt from
// a copy of the lobe's own store). That leader confirms it, and the refusal is
// lifted without an operator.
func TestJoin_ARefusedLobeIsAdmittedAgainByACortexThatHoldsItsLastEntry(t *testing.T) {
	l := newMatchLeader(t)
	l.follow(0, 40, "old-leader")
	l.promote()
	l.write(5)

	lobeDB := newTestDB(t)
	lobeApplier := NewApplier(lobeDB)
	for seq := uint64(1); seq <= 40; seq++ {
		if err := lobeApplier.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatalf("lobe apply %d: %v", seq, err)
		}
	}
	if err := lobeApplier.SetHalt(inventedHalt(40)); err != nil {
		t.Fatalf("SetHalt: %v", err)
	}
	client := NewJoinClient("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), lobeApplier, NewConnManager("node-lobe"))

	seq, sum, ok := lobeApplier.LastAppliedMark()
	if !ok || seq != 40 {
		t.Fatalf("precondition: lobe mark = (%d, ok=%v)", seq, ok)
	}
	resp := l.h.HandleJoinRequest(mbp.JoinRequest{
		NodeID: "node-lobe", LastApplied: seq, LastAppliedSum: sumBytes(sum), Halted: true, ProtocolVersion: mbp.CurrentProtocolVersion,
	}, nil)
	if !resp.Accepted || !resp.Matched {
		t.Fatalf("the leader answered Accepted=%v Matched=%v (%q), want an admission that confirms the entry", resp.Accepted, resp.Matched, resp.RejectReason)
	}

	conn, reqCh := mockCortexJoin(t, resp)
	if _, err := client.joinConn(context.Background(), conn); err != nil {
		t.Fatalf("joinConn: %v", err)
	}
	if req := <-reqCh; !req.Halted {
		t.Error("the lobe did not tell the leader it had been refused before")
	}
	if _, halted := haltOnDisk(lobeDB); halted {
		t.Fatal("the refusal is still on disk after the leader confirmed the lobe's last entry")
	}
	for _, e := range mustReadSince(t, l.log, 40) {
		if err := lobeApplier.Apply(e); err != nil {
			t.Fatalf("apply the leader's entry %d: %v", e.Seq, err)
		}
	}
	if got := lobeApplier.LastApplied(); got != 45 {
		t.Errorf("lobe applied through %d, want 45", got)
	}
}

func mustReadSince(t *testing.T, log *ReplicationLog, after uint64) []ReplicationEntry {
	t.Helper()
	entries, err := log.ReadSince(after, 100_000)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return entries
}

// joinRequestBeforeTheRefusal and joinResponseBeforeTheRefusal are the two
// frames as a build without any of this encodes and decodes them: no log head,
// no checksum, no Diverged, Halted, Matched or Unverified.
type joinRequestBeforeTheRefusal struct {
	NodeID          string   `msgpack:"node_id"`
	Addr            string   `msgpack:"addr"`
	LastApplied     uint64   `msgpack:"last_applied"`
	Capabilities    []string `msgpack:"capabilities"`
	SecretHash      []byte   `msgpack:"secret_hash"`
	Role            uint8    `msgpack:"role,omitempty"`
	Probe           bool     `msgpack:"probe,omitempty"`
	ProtocolVersion uint16   `msgpack:"proto_ver,omitempty"`
}

type joinResponseBeforeTheRefusal struct {
	Accepted               bool               `msgpack:"accepted"`
	CortexID               string             `msgpack:"cortex_id"`
	CortexAddr             string             `msgpack:"cortex_addr"`
	Epoch                  uint64             `msgpack:"epoch"`
	Members                []mbp.GossipMember `msgpack:"members"`
	RejectReason           string             `msgpack:"reject_reason,omitempty"`
	NeedsSnapshot          bool               `msgpack:"needs_snapshot,omitempty"`
	SnapshotSeq            uint64             `msgpack:"snapshot_seq,omitempty"`
	MinProtocolVersion     uint16             `msgpack:"min_proto_ver,omitempty"`
	CurrentProtocolVersion uint16             `msgpack:"current_proto_ver,omitempty"`
}

// olderCortexJoin stands in for a Cortex built before the refusal existed. It
// decodes the request into the older shape, accepts, and answers in the older
// shape, which is all such a build can do.
func olderCortexJoin(t *testing.T, id string, epoch uint64) (clientConn net.Conn, sawRequest <-chan joinRequestBeforeTheRefusal) {
	t.Helper()
	cConn, sConn := net.Pipe()
	reqCh := make(chan joinRequestBeforeTheRefusal, 1)
	go func() {
		defer sConn.Close()
		frame, err := mbp.ReadFrame(sConn)
		if err != nil {
			t.Errorf("older cortex: read request: %v", err)
			return
		}
		var req joinRequestBeforeTheRefusal
		if err := msgpack.Unmarshal(frame.Payload, &req); err != nil {
			t.Errorf("older cortex: a request from this build does not decode: %v", err)
			return
		}
		reqCh <- req
		payload, err := msgpack.Marshal(joinResponseBeforeTheRefusal{Accepted: true, CortexID: id, CortexAddr: "127.0.0.1:9", Epoch: epoch})
		if err != nil {
			t.Errorf("older cortex: marshal response: %v", err)
			return
		}
		_ = mbp.WriteFrame(sConn, &mbp.Frame{Version: 0x01, Type: mbp.TypeJoinResponse, PayloadLength: uint32(len(payload)), Payload: payload})
	}()
	return cConn, reqCh
}

// TestMixedVersions_AnOlderCortexDoesNotReleaseARefusedLobe is a rolling
// upgrade with a refused replica in the cluster. An older leader knows nothing
// of the refusal. It accepts every join. The refused replica must not take
// that for an answer, and a replica that was never refused must still join
// such a leader as it always has.
func TestMixedVersions_AnOlderCortexDoesNotReleaseARefusedLobe(t *testing.T) {
	db := newTestDB(t)
	applier := NewApplier(db)
	for seq := uint64(1); seq <= 20; seq++ {
		if err := applier.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatalf("apply %d: %v", seq, err)
		}
	}
	if err := applier.SetHalt(inventedHalt(20)); err != nil {
		t.Fatalf("SetHalt: %v", err)
	}
	client := NewJoinClient("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), applier, NewConnManager("node-lobe"))

	conn, sawRequest := olderCortexJoin(t, "node-older", 3)
	_, err := client.joinConn(context.Background(), conn)
	if req := <-sawRequest; req.NodeID != "node-lobe" || req.LastApplied != 20 {
		t.Errorf("the older cortex read the request as %+v, want node-lobe at 20", req)
	}
	if !errors.Is(err, ErrFollowerAhead) {
		t.Fatalf("a refused lobe accepted by an older cortex: err = %v, want ErrFollowerAhead. "+
			"It would follow and skip every entry numbered at or below 20", err)
	}
	rec, halted := haltOnDisk(db)
	if !halted || rec.UpheldBy != "node-older" || rec.UpheldEpoch != 3 || rec.LeaderID != "node-cortex" {
		t.Errorf("record on disk = (%+v, %v), want the original refusal, upheld by node-older epoch 3", rec, halted)
	}
	if err := applier.Apply(otherLeadersEntry(21, "node-older")); !errors.Is(err, ErrApplierHalted) {
		t.Errorf("Apply after an older cortex accepted the join returned %v, want ErrApplierHalted", err)
	}

	// The control: nothing changes for a lobe that holds no refusal.
	plainDB := newTestDB(t)
	seedLastApplied(t, plainDB, 20)
	plain := NewJoinClient("node-plain", "127.0.0.1:6", "", newTestEpochStore(t), NewApplier(plainDB), NewConnManager("node-plain"))
	conn, sawRequest = olderCortexJoin(t, "node-older", 3)
	if _, err := plain.joinConn(context.Background(), conn); err != nil {
		t.Fatalf("a lobe that was never refused could not join an older cortex: %v", err)
	}
	<-sawRequest
}

// TestMixedVersions_AnOlderLobeAndThisCortex is the other direction. An older
// replica sends a position and nothing else. This leader still refuses it when
// the position alone is proof, the older replica reads that as an ordinary
// rejection, and everything else is admitted, including what a replica that
// could say it had been refused would not be: an older build does not read the
// record, so it does not say so.
func TestMixedVersions_AnOlderLobeAndThisCortex(t *testing.T) {
	l := newMatchLeader(t)
	l.follow(0, 40, "old-leader")
	l.promote()
	l.write(10)
	if err := l.log.Prune(44); err != nil {
		t.Fatalf("prune: %v", err)
	}

	ask := func(at uint64) joinResponseBeforeTheRefusal {
		t.Helper()
		sent, err := msgpack.Marshal(joinRequestBeforeTheRefusal{NodeID: fmt.Sprintf("node-older-%d", at), Addr: "127.0.0.1:7", LastApplied: at, ProtocolVersion: mbp.CurrentProtocolVersion})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var req mbp.JoinRequest
		if err := msgpack.Unmarshal(sent, &req); err != nil {
			t.Fatalf("this cortex cannot decode an older lobe's request: %v", err)
		}
		if req.Halted || len(req.LastAppliedSum) != 0 {
			t.Fatalf("an older lobe's request decoded with Halted=%v LastAppliedSum=%x", req.Halted, req.LastAppliedSum)
		}
		answered, err := msgpack.Marshal(l.h.HandleJoinRequest(req, nil))
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		var got joinResponseBeforeTheRefusal
		if err := msgpack.Unmarshal(answered, &got); err != nil {
			t.Fatalf("an older lobe cannot decode this cortex's response: %v", err)
		}
		return got
	}

	if got := ask(60); got.Accepted || !strings.HasPrefix(got.RejectReason, aheadRejectPrefix) || got.CortexID != "node-cortex" || got.Epoch != 2 {
		t.Errorf("an older lobe past the log head read the answer as %+v, want a rejection that says why", got)
	}
	for _, at := range []uint64{50, 47, 42, 30} {
		if got := ask(at); !got.Accepted || got.CortexID != "node-cortex" || got.Epoch != 2 {
			t.Errorf("an older lobe at %d read the answer as %+v, want an admission", at, got)
		}
	}
}

// TestPruneOnce_SaysItPrunedOnlyWhenItDeletedSomething: the periodic prune
// logged "pruned replication log" every time it was handed a new watermark,
// including when the watermark was the log head and nothing was deleted (the
// head entry is never pruned). An operator reading that line was told the log
// had been cut when it had not.
func TestPruneOnce_SaysItPrunedOnlyWhenItDeletedSomething(t *testing.T) {
	logs := captureLogs(t)
	coord := newLeaderForJoinTest(t, 10)
	held := func(seq uint64) bool {
		t.Helper()
		_, found, err := coord.repLog.sumAt(seq)
		if err != nil {
			t.Fatalf("read log at %d: %v", seq, err)
		}
		return found
	}
	const said = "pruned replication log"

	// Every replica has acknowledged the head. Nothing can be deleted yet.
	coord.UpdateReplicaSeq("node-lobe", 10)
	if until, deleted := coord.pruneOnce(); until != 10 || deleted {
		t.Fatalf("pruneOnce at the head = (until %d, deleted %v), want (10, false)", until, deleted)
	}
	if !held(1) || !held(10) {
		t.Fatal("precondition: a prune at the head deleted entries")
	}
	if got := logs.lines(said); len(got) != 0 {
		t.Errorf("nothing was deleted, and the log says: %v", got)
	}

	// One more write and the same acknowledged position is below the head.
	fillLog(t, coord.repLog, 1)
	if until, deleted := coord.pruneOnce(); until != 10 || !deleted {
		t.Fatalf("pruneOnce below the head = (until %d, deleted %v), want (10, true)", until, deleted)
	}
	if held(10) || !held(11) {
		t.Fatalf("after the prune: entry 10 held = %v, entry 11 held = %v, want false and true", held(10), held(11))
	}
	if got := logs.lines(said, "until_seq=10"); len(got) != 1 {
		t.Errorf("the prune that deleted entries 1 through 10 is logged %d times, want once: %v", len(got), got)
	}

	// The same watermark again deletes nothing and says nothing.
	if _, deleted := coord.pruneOnce(); deleted {
		t.Error("a second prune at the same watermark reports that it deleted something")
	}
	if got := logs.lines(said); len(got) != 1 {
		t.Errorf("after a prune that deleted nothing the line appears %d times, want still once", len(got))
	}
}

// TestSkipFromSnapshot_ShipsMetadataExceptTheRefusal pins what the snapshot
// filter lets through under the replication prefix.
func TestSkipFromSnapshot_ShipsMetadataExceptTheRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		key  []byte
		skip bool
	}{
		"a log entry":          {replicationEntryKey(7), true},
		"the refusal record":   {replHaltKey(), true},
		"the sequence counter": {seqCounterKey(), false},
		"the apply cursor":     {lastAppliedKey(), false},
		"the cluster epoch":    {clusterEpochKey(), false},
		"an ordinary data key": {[]byte("probe/key"), false},
	} {
		if got := skipFromSnapshot(tc.key); got != tc.skip {
			t.Errorf("%s: skipped = %v, want %v", name, got, tc.skip)
		}
	}
}
