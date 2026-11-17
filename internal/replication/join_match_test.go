package replication

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The entry match check (STO-27): a follower keeps the checksum of the last
// entry it applied, sends it when it joins, and the leader compares it with
// the entry it holds at that sequence number. join_ahead_test.go covers the
// older half of the rule (a position past the leader's head) and
// failover_ahead_test.go runs both through a live cluster.

func sumBytes(sum uint32) []byte { return binary.BigEndian.AppendUint32(nil, sum) }

// batchRepr returns the committed repr of a one-key Pebble batch, which is
// what a leader's storage layer hands the replication log for every write.
func batchRepr(t *testing.T, db *pebble.DB, key, value string) []byte {
	t.Helper()
	b := db.NewBatch()
	defer b.Close()
	if err := b.Set([]byte(key), []byte(value), nil); err != nil {
		t.Fatalf("build batch: %v", err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatalf("commit batch: %v", err)
	}
	return append([]byte(nil), b.Repr()...)
}

func TestEntrySum_CoversEveryFieldThatIdentifiesAnEntry(t *testing.T) {
	base := ReplicationEntry{Seq: 7, Op: OpBatch, Key: []byte("k"), Value: []byte("v"), TimestampNS: 1000}
	same := ReplicationEntry{Seq: 7, Op: OpBatch, Key: []byte("k"), Value: []byte("v"), TimestampNS: 1000}
	if entrySum(base) != entrySum(same) {
		t.Fatal("equal entries have different checksums: a healthy follower would be refused")
	}
	// A nil key and an empty key are the same entry: the key is nil on the
	// leader and may come back empty from the wire.
	if entrySum(ReplicationEntry{Seq: 7, Op: OpBatch, Value: []byte("v")}) !=
		entrySum(ReplicationEntry{Seq: 7, Op: OpBatch, Key: []byte{}, Value: []byte("v")}) {
		t.Fatal("a nil key and an empty key have different checksums")
	}
	for name, other := range map[string]ReplicationEntry{
		"sequence":  {Seq: 8, Op: OpBatch, Key: []byte("k"), Value: []byte("v"), TimestampNS: 1000},
		"operation": {Seq: 7, Op: OpSet, Key: []byte("k"), Value: []byte("v"), TimestampNS: 1000},
		"key":       {Seq: 7, Op: OpBatch, Key: []byte("j"), Value: []byte("v"), TimestampNS: 1000},
		"value":     {Seq: 7, Op: OpBatch, Key: []byte("k"), Value: []byte("w"), TimestampNS: 1000},
		"timestamp": {Seq: 7, Op: OpBatch, Key: []byte("k"), Value: []byte("v"), TimestampNS: 1001},
		// The same bytes split differently between key and value.
		"key and value boundary": {Seq: 7, Op: OpBatch, Key: []byte("kv"), Value: nil, TimestampNS: 1000},
	} {
		if entrySum(base) == entrySum(other) {
			t.Errorf("entries that differ in %s have the same checksum", name)
		}
	}
}

// TestApplier_ChecksumMatchesTheLeadersForTheSameEntry is the property a
// healthy follower's join depends on: the checksum it stored for an entry it
// applied equals the checksum the leader computes from its own log.
//
// It is here because the first version got it wrong. Applying a batch repr
// lets Pebble stamp its own sequence number into the header of that buffer, so
// a checksum taken after the apply was of bytes the leader never held, and a
// leader that restarted refused every one of its followers. The two stores
// below are deliberately at different Pebble sequence numbers.
func TestApplier_ChecksumMatchesTheLeadersForTheSameEntry(t *testing.T) {
	leaderDB, followerDB := newTestDB(t), newTestDB(t)
	leaderLog := NewReplicationLog(leaderDB)
	for i := 0; i < 5; i++ { // move the follower's store ahead of the leader's
		if err := followerDB.Set([]byte(fmt.Sprintf("probe/unrelated/%d", i)), []byte("invented"), pebble.Sync); err != nil {
			t.Fatalf("seed follower store: %v", err)
		}
	}
	if _, err := leaderLog.Append(OpBatch, nil, batchRepr(t, leaderDB, "probe/batch", "invented")); err != nil {
		t.Fatalf("append batch entry: %v", err)
	}
	if _, err := leaderLog.Append(OpSet, []byte("probe/set"), []byte("invented")); err != nil {
		t.Fatalf("append set entry: %v", err)
	}

	follower := NewApplier(followerDB)
	entries, err := leaderLog.ReadSince(0, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadSince = %d entries, %v", len(entries), err)
	}
	for _, e := range entries {
		if err := follower.Apply(e); err != nil {
			t.Fatalf("apply seq %d: %v", e.Seq, err)
		}
		seq, got, ok := follower.LastAppliedMark()
		want, found, err := leaderLog.sumAt(e.Seq)
		if err != nil || !found {
			t.Fatalf("leader sumAt(%d) = found %v, %v", e.Seq, found, err)
		}
		if !ok || seq != e.Seq || got != want {
			t.Fatalf("after applying seq %d (op %d): follower mark = (%d, %08x, %v), leader's checksum for that entry = %08x",
				e.Seq, e.Op, seq, got, ok, want)
		}
	}
}

// TestApplier_LastAppliedMarkSurvivesARestartAndNeverGoesStale covers where
// the mark lives: in the cursor's own stored value.
func TestApplier_LastAppliedMarkSurvivesARestartAndNeverGoesStale(t *testing.T) {
	db := newTestDB(t)
	a := NewApplier(db)
	if seq, _, ok := a.LastAppliedMark(); seq != 0 || ok {
		t.Fatalf("a fresh applier reports mark (%d, ok=%v), want (0, false)", seq, ok)
	}
	entry := ReplicationEntry{Seq: 9, Op: OpSet, Key: []byte("probe/k"), Value: []byte("invented"), TimestampNS: 5}
	if err := a.Apply(entry); err != nil {
		t.Fatalf("apply: %v", err)
	}
	seq, sum, ok := a.LastAppliedMark()
	if seq != 9 || !ok || sum != entrySum(entry) {
		t.Fatalf("mark after apply = (%d, %08x, %v), want (9, %08x, true)", seq, sum, ok, entrySum(entry))
	}

	// An entry at or below the cursor is skipped and must not touch the mark.
	if err := a.Apply(ReplicationEntry{Seq: 9, Op: OpSet, Key: []byte("probe/other"), Value: []byte("different")}); err != nil {
		t.Fatalf("apply skipped entry: %v", err)
	}
	if _, got, _ := a.LastAppliedMark(); got != sum {
		t.Errorf("a skipped entry changed the stored checksum from %08x to %08x", sum, got)
	}

	// A new process over the same store.
	if rseq, rsum, rok := NewApplier(db).LastAppliedMark(); rseq != 9 || rsum != sum || !rok {
		t.Errorf("mark after restart = (%d, %08x, %v), want (9, %08x, true)", rseq, rsum, rok, sum)
	}
	// The cursor alone still reads back as before.
	if got := NewApplier(db).LastApplied(); got != 9 {
		t.Errorf("LastApplied after restart = %d, want 9", got)
	}

	// A build from before the checksum reads eight bytes and writes eight
	// back. The checksum must be gone, not left behind beside a newer cursor.
	older := make([]byte, 8)
	binary.BigEndian.PutUint64(older, 12)
	if err := db.Set(lastAppliedKey(), older, pebble.Sync); err != nil {
		t.Fatalf("write an older build's cursor: %v", err)
	}
	if rseq, _, rok := NewApplier(db).LastAppliedMark(); rseq != 12 || rok {
		t.Errorf("mark after an older build wrote the cursor = (%d, ok=%v), want (12, false)", rseq, rok)
	}

	// A snapshot rebase moves the cursor without applying an entry at it.
	b := NewApplier(db)
	if err := b.Apply(ReplicationEntry{Seq: 13, Op: OpSet, Key: []byte("probe/k2"), Value: []byte("invented")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, baseline := range []uint64{40, 40} { // the second call is a rebase onto the cursor's own value
		if err := b.ResetTo(baseline); err != nil {
			t.Fatalf("ResetTo(%d): %v", baseline, err)
		}
		if rseq, _, rok := b.LastAppliedMark(); rseq != 40 || rok {
			t.Errorf("mark after ResetTo(%d) = (%d, ok=%v), want (40, false)", baseline, rseq, rok)
		}
	}
	if rseq, _, rok := NewApplier(db).LastAppliedMark(); rseq != 40 || rok {
		t.Errorf("mark on disk after ResetTo = (%d, ok=%v), want (40, false)", rseq, rok)
	}
	// A rebase onto the value the cursor already has must still drop a
	// checksum that belonged to an applied entry.
	c := NewApplier(db)
	if err := c.Apply(ReplicationEntry{Seq: 41, Op: OpSet, Key: []byte("probe/k3"), Value: []byte("invented")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := c.ResetTo(41); err != nil {
		t.Fatalf("ResetTo(41): %v", err)
	}
	if _, _, rok := NewApplier(db).LastAppliedMark(); rok {
		t.Error("a snapshot rebase onto the cursor's own value left the applied entry's checksum on disk")
	}
}

// matchLeader is a join handler over a real log and a real applier, set up as
// a node with a given history.
type matchLeader struct {
	t       *testing.T
	db      *pebble.DB
	log     *ReplicationLog
	applier *Applier
	h       *JoinHandler
}

func newMatchLeader(t *testing.T) *matchLeader {
	t.Helper()
	db := newTestDB(t)
	es := newTestEpochStore(t)
	if _, err := es.Advance(2); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	l := &matchLeader{t: t, db: db, log: NewReplicationLog(db), applier: NewApplier(db)}
	l.h = NewJoinHandler("node-cortex", "", es, l.log, NewConnManager("node-cortex"))
	l.h.appliedMark = l.applier.LastAppliedMark
	return l
}

// write appends n entries of this node's own. Read a checksum back with sum.
func (l *matchLeader) write(n int) {
	l.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.log.Append(OpSet, []byte(fmt.Sprintf("probe/own/%d", l.log.CurrentSeq()+1)), []byte("written by this node")); err != nil {
			l.t.Fatalf("append: %v", err)
		}
	}
}

// follow applies one entry per sequence in (from, through] as a follower of
// some other leader, and returns the checksum of each by sequence number.
func (l *matchLeader) follow(from, through uint64, author string) map[uint64]uint32 {
	l.t.Helper()
	sums := map[uint64]uint32{}
	for seq := from + 1; seq <= through; seq++ {
		e := otherLeadersEntry(seq, author)
		if err := l.applier.Apply(e); err != nil {
			l.t.Fatalf("apply seq %d: %v", seq, err)
		}
		sums[seq] = entrySum(e)
	}
	return sums
}

// promote raises the log counter the way takeLeaderRole does.
func (l *matchLeader) promote() {
	l.t.Helper()
	if _, err := l.log.AdvanceTo(l.applier.LastApplied()); err != nil {
		l.t.Fatalf("advance: %v", err)
	}
}

func (l *matchLeader) sum(seq uint64) uint32 {
	l.t.Helper()
	s, found, err := l.log.sumAt(seq)
	if err != nil || !found {
		l.t.Fatalf("no entry at seq %d in this node's log (found %v, err %v)", seq, found, err)
	}
	return s
}

func (l *matchLeader) verdict(at uint64, sum []byte) string {
	l.t.Helper()
	head, err := l.log.Head()
	if err != nil {
		l.t.Fatalf("head: %v", err)
	}
	verdict, why := l.h.compare(mbp.JoinRequest{NodeID: "node-lobe", LastApplied: at, LastAppliedSum: sum}, head)
	if verdict != joinDiffers {
		return ""
	}
	if why == "" {
		l.t.Fatalf("compare said the entries at seq %d differ and gave no reason", at)
	}
	return why
}

// confirms reports whether this node positively confirms that a lobe at
// position at, holding an entry with checksum sum there, holds this node's own
// entry: the only answer that lets a lobe that was refused before back in.
func (l *matchLeader) confirms(at uint64, sum []byte) bool {
	l.t.Helper()
	head, err := l.log.Head()
	if err != nil {
		l.t.Fatalf("head: %v", err)
	}
	verdict, _ := l.h.compare(mbp.JoinRequest{NodeID: "node-lobe", LastApplied: at, LastAppliedSum: sum}, head)
	return verdict == joinMatches
}

// otherLeadersEntry is the entry some other leader wrote at seq. Two authors
// never produce the same entry for one sequence number.
func otherLeadersEntry(seq uint64, author string) ReplicationEntry {
	return ReplicationEntry{
		Seq: seq, Op: OpSet,
		Key:         []byte(fmt.Sprintf("probe/%s/%d", author, seq)),
		Value:       []byte("written by " + author),
		TimestampNS: int64(seq),
	}
}

func otherSum(seq uint64, author string) []byte {
	return sumBytes(entrySum(otherLeadersEntry(seq, author)))
}

func expectVerdict(t *testing.T, what string, got string, wantRefused bool) {
	t.Helper()
	if (got != "") != wantRefused {
		if wantRefused {
			t.Errorf("%s: admitted, want refused", what)
		} else {
			t.Errorf("%s: REFUSED (%s), want admitted", what, got)
		}
	}
}

// TestDivergence_PromotedFollower is the failover case. A node that followed
// the old leader through 40 is promoted and writes 41 through 45.
func TestDivergence_PromotedFollower(t *testing.T) {
	l := newMatchLeader(t)
	applied := l.follow(0, 40, "old-leader")
	l.promote()
	l.write(5)
	if head := l.log.CurrentSeq(); head != 45 {
		t.Fatalf("precondition: head = %d, want 45", head)
	}

	// Level with the node that won: it applied the same entry 40.
	expectVerdict(t, "follower at 40 with the old leader's entry 40",
		l.verdict(40, sumBytes(applied[40])), false)
	// Holds a different entry at 40: it followed some other leader there.
	expectVerdict(t, "follower at 40 with another leader's entry 40",
		l.verdict(40, otherSum(40, "someone-else")), true)

	// The reviewer's finding. The follower is at 43 with the OLD leader's 43.
	// The new leader's head (45) is past it, so positions alone say fine.
	if 43 > l.log.CurrentSeq() {
		t.Fatal("precondition: the follower's position must be at or below the leader's head")
	}
	why := l.verdict(43, otherSum(43, "old-leader"))
	expectVerdict(t, "follower at 43 holding the old leader's entry 43 while this leader wrote its own 43", why, true)
	for _, want := range []string{"43", "45"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason does not mention %q: %s", want, why)
		}
	}
	// A follower that applied THIS leader's 43 (it joined and then
	// reconnected) is the ordinary case and must go through.
	expectVerdict(t, "follower at 43 holding this leader's own entry 43",
		l.verdict(43, sumBytes(l.sum(43))), false)
	expectVerdict(t, "follower at 45 (the head) holding this leader's own entry 45",
		l.verdict(45, sumBytes(l.sum(45))), false)

	// Past the head: refused whatever it says about the entry.
	expectVerdict(t, "follower at 46, past the head, with a checksum", l.verdict(46, otherSum(46, "old-leader")), true)
	expectVerdict(t, "follower at 46, past the head, without a checksum", l.verdict(46, nil), true)

	// Nothing to compare: admitted.
	expectVerdict(t, "follower at 43 that sent no checksum", l.verdict(43, nil), false)
	expectVerdict(t, "follower at 43 that sent a checksum of the wrong length", l.verdict(43, []byte{1, 2}), false)
	expectVerdict(t, "follower that has applied nothing", l.verdict(0, nil), false)
	// Behind the node that won, inside the range it applied and never logged.
	expectVerdict(t, "follower at 33, behind, with the old leader's entry 33", l.verdict(33, sumBytes(applied[33])), false)
	expectVerdict(t, "follower at 33, behind, with some other entry 33", l.verdict(33, otherSum(33, "someone-else")), false)
}

// TestDivergence_LeaderThatOnlyEverLed is the leader restart: no apply cursor
// at all, a log of its own entries, and followers at positions inside it.
func TestDivergence_LeaderThatOnlyEverLed(t *testing.T) {
	l := newMatchLeader(t)
	l.write(10)
	own7 := sumBytes(l.sum(7))

	// A new process over the same store, as after a restart.
	l.log = NewReplicationLog(l.db)
	l.applier = NewApplier(l.db)
	l.h.repLog, l.h.appliedMark = l.log, l.applier.LastAppliedMark

	expectVerdict(t, "follower at 7 with this leader's entry 7, after the leader restarted", l.verdict(7, own7), false)
	expectVerdict(t, "follower at 10 (the head) with this leader's entry 10", l.verdict(10, sumBytes(l.sum(10))), false)
	expectVerdict(t, "follower at 7 with another leader's entry 7", l.verdict(7, otherSum(7, "someone-else")), true)

	// The log is pruned through 8, as an ordinary acked prune does, including
	// the entry at the slowest follower's own position.
	if err := l.log.Prune(8); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, found, _ := l.log.sumAt(7); found {
		t.Fatal("precondition: entry 7 must be pruned")
	}
	expectVerdict(t, "follower at 7 with this leader's entry 7, after 7 was pruned", l.verdict(7, own7), false)
	expectVerdict(t, "follower at 8, exactly at the prune point", l.verdict(8, otherSum(8, "unknown")), false)
	// Even a follower that really has diverged is admitted once its entry is
	// pruned: there is nothing left to prove it with. Stated, not hidden.
	expectVerdict(t, "follower at 7 with another leader's entry 7, after 7 was pruned (cannot be proven)",
		l.verdict(7, otherSum(7, "someone-else")), false)
	// Above the prune point the comparison still works.
	expectVerdict(t, "follower at 9 with another leader's entry 9", l.verdict(9, otherSum(9, "someone-else")), true)
}

// TestDivergence_LogEntriesAtOrBelowTheApplyCursorNeverRefuse: a node that led
// through 42, was deposed while 40 through 42 had reached nobody, followed the
// new leader through 45, and now leads again. Its log still holds its own old
// 40 through 42. The cluster's entries at those numbers are the other
// leader's, which this node applied over its own. A healthy follower holding
// the cluster's entry 42 must not be refused because this node's log has a
// stale one there.
func TestDivergence_LogEntriesAtOrBelowTheApplyCursorNeverRefuse(t *testing.T) {
	l := newMatchLeader(t)
	l.write(42)                              // its own earlier term
	applied := l.follow(39, 45, "successor") // then it followed someone else from 40
	l.promote()
	l.write(3) // 46..48
	if _, found, _ := l.log.sumAt(42); !found {
		t.Fatal("precondition: this node's own old entry 42 must still be in its log")
	}
	if l.sum(42) == applied[42] {
		t.Fatal("precondition: the stale entry 42 and the cluster's entry 42 must differ")
	}

	expectVerdict(t, "follower at 42 with the cluster's entry 42, against a leader whose log holds a stale 42",
		l.verdict(42, sumBytes(applied[42])), false)
	// At the cursor itself the applied entry is the witness.
	expectVerdict(t, "follower at 45 with the cluster's entry 45", l.verdict(45, sumBytes(applied[45])), false)
	expectVerdict(t, "follower at 45 with another entry 45", l.verdict(45, otherSum(45, "someone-else")), true)
	// Above it, the log is.
	expectVerdict(t, "follower at 47 with another entry 47", l.verdict(47, otherSum(47, "someone-else")), true)
}

// TestDivergence_NodeBehindItsOwnCounter: a node that led through 42 and then
// followed only through 41 before it was elected again. Its counter (42) is
// ahead of what it applied (41). A follower holding the other leader's entry
// 42 has an entry this node never received, and that is what the comparison
// against this node's own entry 42 finds.
func TestDivergence_NodeBehindItsOwnCounter(t *testing.T) {
	l := newMatchLeader(t)
	l.write(42)
	l.follow(39, 41, "successor")
	l.promote()
	if head := l.log.CurrentSeq(); head != 42 {
		t.Fatalf("precondition: head = %d, want 42 (the counter, not the cursor)", head)
	}
	expectVerdict(t, "follower at 42 with the other leader's entry 42, which this node never applied",
		l.verdict(42, otherSum(42, "successor")), true)
	expectVerdict(t, "follower at 42 with this node's own entry 42", l.verdict(42, sumBytes(l.sum(42))), false)
}

// TestDivergence_EitherWitnessAdmits: after a graceful handoff the new leader
// can hold two entries for one sequence number: its own (in its log) and the
// one the old leader was still streaming (applied). A follower holding either
// is following a history this node has, and is admitted.
func TestDivergence_EitherWitnessAdmits(t *testing.T) {
	l := newMatchLeader(t)
	l.follow(0, 40, "old-leader")
	l.promote()
	l.write(1)                             // this leader's own 41
	tail := l.follow(40, 41, "old-leader") // and the old leader's 41 arrives after
	expectVerdict(t, "follower at 41 with the old leader's entry 41", l.verdict(41, sumBytes(tail[41])), false)
	expectVerdict(t, "follower at 41 with this leader's own entry 41", l.verdict(41, sumBytes(l.sum(41))), false)
	expectVerdict(t, "follower at 41 with a third entry 41", l.verdict(41, otherSum(41, "someone-else")), true)
}

// TestDivergence_CannotCompareMeansAdmit covers the remaining ways the leader
// has nothing to compare with.
func TestDivergence_CannotCompareMeansAdmit(t *testing.T) {
	t.Run("the leader's own cursor was written by an older build", func(t *testing.T) {
		l := newMatchLeader(t)
		older := make([]byte, 8)
		binary.BigEndian.PutUint64(older, 40)
		if err := l.db.Set(lastAppliedKey(), older, pebble.Sync); err != nil {
			t.Fatalf("write cursor: %v", err)
		}
		l.applier = NewApplier(l.db)
		l.h.appliedMark = l.applier.LastAppliedMark
		l.promote()
		expectVerdict(t, "follower at 40 against a leader with no checksum for its own entry 40",
			l.verdict(40, otherSum(40, "anyone")), false)
	})
	t.Run("the handler has no applier", func(t *testing.T) {
		l := newMatchLeader(t)
		l.write(5)
		l.h.appliedMark = nil
		expectVerdict(t, "follower at 3 with this leader's entry 3", l.verdict(3, sumBytes(l.sum(3))), false)
		expectVerdict(t, "follower at 3 with another entry 3", l.verdict(3, otherSum(3, "someone-else")), true)
	})
	t.Run("a snapshot will replace the joiner's state", func(t *testing.T) {
		db := newTestDB(t)
		es := newTestEpochStore(t)
		if _, err := es.Advance(2); err != nil {
			t.Fatalf("seed epoch: %v", err)
		}
		log := NewReplicationLog(db)
		fillLog(t, log, 5)
		h := NewJoinHandlerWithDB("node-cortex", "", es, log, db, NewConnManager("node-cortex"))
		resp := h.HandleJoinRequest(mbp.JoinRequest{
			NodeID: "node-lobe", LastApplied: 3, LastAppliedSum: otherSum(3, "someone-else"),
			ProtocolVersion: mbp.CurrentProtocolVersion,
		}, nil)
		if !resp.Accepted || !resp.NeedsSnapshot || resp.Diverged {
			t.Fatalf("Accepted=%v NeedsSnapshot=%v Diverged=%v, want an accepted join with a snapshot", resp.Accepted, resp.NeedsSnapshot, resp.Diverged)
		}
	})
}

// TestJoin_DivergedRefusalEndToEnd runs the refusal through the join handshake
// on both sides: what the leader answers, and what the follower makes of it.
func TestJoin_DivergedRefusalEndToEnd(t *testing.T) {
	l := newMatchLeader(t)
	l.follow(0, 40, "old-leader")
	l.promote()
	l.write(5)

	// The follower: applied the old leader's entries through 43.
	followerDB := newTestDB(t)
	followerApplier := NewApplier(followerDB)
	for seq := uint64(1); seq <= 43; seq++ {
		if err := followerApplier.Apply(otherLeadersEntry(seq, "old-leader")); err != nil {
			t.Fatalf("follower apply %d: %v", seq, err)
		}
	}
	client := NewJoinClient("node-lobe", "127.0.0.1:5", "", newTestEpochStore(t), followerApplier, NewConnManager("node-lobe"))

	// The leader's answer to exactly what this follower sends.
	seq, sum, ok := followerApplier.LastAppliedMark()
	if !ok || seq != 43 {
		t.Fatalf("precondition: follower mark = (%d, ok=%v)", seq, ok)
	}
	resp := l.h.HandleJoinRequest(mbp.JoinRequest{
		NodeID: "node-lobe", LastApplied: seq, LastAppliedSum: sumBytes(sum), ProtocolVersion: mbp.CurrentProtocolVersion,
	}, nil)
	if resp.Accepted || !resp.Diverged {
		t.Fatalf("leader answered Accepted=%v Diverged=%v, want a refusal marked diverged", resp.Accepted, resp.Diverged)
	}
	if !strings.HasPrefix(resp.RejectReason, aheadRejectPrefix) {
		t.Errorf("RejectReason = %q, want it to start with %q", resp.RejectReason, aheadRejectPrefix)
	}
	if resp.LeaderHeadSeq == nil || *resp.LeaderHeadSeq != 45 {
		t.Errorf("LeaderHeadSeq = %v, want 45", resp.LeaderHeadSeq)
	}
	if _, member := memberByID(l.h, "node-lobe"); member {
		t.Error("a refused lobe was recorded as a member")
	}

	// The follower sends its checksum, and treats that answer as the refusal
	// it is although its position (43) is below the leader's head (45).
	conn, reqCh := mockCortexJoin(t, resp)
	_, err := client.joinConn(context.Background(), conn)
	req := <-reqCh
	if req.LastApplied != 43 || len(req.LastAppliedSum) != 4 || binary.BigEndian.Uint32(req.LastAppliedSum) != sum {
		t.Errorf("JoinRequest carried LastApplied=%d LastAppliedSum=%x, want 43 and %08x", req.LastApplied, req.LastAppliedSum, sum)
	}
	if !errors.Is(err, ErrFollowerAhead) {
		t.Fatalf("joinConn error = %v, want ErrFollowerAhead", err)
	}
	for _, want := range []string{"43", "node-cortex", "docs/cluster-operations.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	// A follower with nothing applied sends no checksum at all.
	fresh := NewJoinClient("node-fresh", "127.0.0.1:6", "", newTestEpochStore(t), NewApplier(newTestDB(t)), NewConnManager("node-fresh"))
	conn, reqCh = mockCortexJoin(t, mbp.JoinResponse{Accepted: true, CortexID: "node-cortex", Epoch: 2})
	if _, err := fresh.joinConn(context.Background(), conn); err != nil {
		t.Fatalf("fresh join: %v", err)
	}
	if req := <-reqCh; len(req.LastAppliedSum) != 0 {
		t.Errorf("a follower that has applied nothing sent a checksum: %x", req.LastAppliedSum)
	}
}

// TestHandleIncomingJoin_DivergedLobeIsRefusedBeforeItsConnectionIsTouched: the
// coordinator's path for the case positions cannot show. The lobe's position
// is BELOW the leader's head, and its standing connection must survive the
// refusal all the same.
func TestHandleIncomingJoin_DivergedLobeIsRefusedBeforeItsConnectionIsTouched(t *testing.T) {
	coord := newLeaderForJoinTest(t, 10)

	standingCortexEnd, standingLobeEnd := net.Pipe()
	t.Cleanup(func() { standingCortexEnd.Close(); standingLobeEnd.Close() })
	standing, ok := coord.mgr.RegisterConnKind("node-diverged", "127.0.0.1:2", standingCortexEnd, kindHello, true)
	if !ok {
		t.Fatal("could not register the standing connection")
	}

	resp, adopted, _ := serveJoin(t, coord, mbp.JoinRequest{
		NodeID: "node-diverged", Addr: "127.0.0.1:2",
		LastApplied: 7, LastAppliedSum: otherSum(7, "old-leader"),
		ProtocolVersion: mbp.CurrentProtocolVersion,
	})
	if adopted || resp.Accepted || !resp.Diverged {
		t.Fatalf("adopted=%v Accepted=%v Diverged=%v, want a refusal on the raw connection", adopted, resp.Accepted, resp.Diverged)
	}
	if now, _ := coord.mgr.GetPeer("node-diverged"); now != standing || !standing.IsConnected() {
		t.Error("refusing the join replaced or closed the lobe's standing connection")
	}
	coord.streamersMu.Lock()
	_, streaming := coord.streamers["node-diverged"]
	coord.streamersMu.Unlock()
	if streaming {
		t.Error("a replication stream was started to a refused lobe")
	}
}
