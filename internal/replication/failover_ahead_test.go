package replication

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
)

// The failover the sequence rule cannot repair by numbering: the follower that
// wins the election is BEHIND the other one. Votes do not compare positions,
// so this can happen whenever the two followers were not level when the leader
// died.

// TestFailover_FollowerAheadOfNewLeaderIsRefusedAndStaysRefused: one follower
// holds five entries the other never received, the leader dies, and the one
// that is behind is elected.
//
// The follower that is ahead cannot follow the new leader. It holds five
// entries the new leader has never seen, and the new leader's first five
// writes carry the same sequence numbers, so its applier would skip them. It
// must not join; it must say so where an operator looks; and it must go on
// refusing after the new leader has written past its position, which is the
// moment a plain position comparison stops seeing the problem.
func TestFailover_FollowerAheadOfNewLeaderIsRefusedAndStaysRefused(t *testing.T) {
	leader, ahead, behind := startThreeNodes(t)

	pre := writeKeys(leader, "probe/pre", 20)
	for _, f := range []*clusterNode{ahead, behind} {
		f := f
		waitUntil(t, func() bool { return f.count(pre) == len(pre) }, func() string {
			return fmt.Sprintf("%s holds %d of %d early writes", f.id, f.count(pre), len(pre))
		})
	}

	// Five writes reach only one follower.
	stopStreamTo(leader, behind)
	onlyAhead := writeKeys(leader, "probe/only-ahead", 5)
	waitUntil(t, func() bool { return ahead.count(onlyAhead) == len(onlyAhead) }, func() string {
		return fmt.Sprintf("%s holds %d of %d late writes", ahead.id, ahead.count(onlyAhead), len(onlyAhead))
	})
	if got := behind.count(onlyAhead); got != 0 {
		t.Fatalf("precondition: %s received %d of the writes meant to reach only %s", behind.id, got, ahead.id)
	}
	aheadAt, behindAt := ahead.applier.LastApplied(), behind.applier.LastApplied()
	if aheadAt != behindAt+uint64(len(onlyAhead)) {
		t.Fatalf("precondition: applied positions are %d and %d, want them %d apart", aheadAt, behindAt, len(onlyAhead))
	}

	leader.stop()
	promote(t, behind)
	if got := behind.log.CurrentSeq(); got != behindAt {
		t.Fatalf("new leader's log head = %d, want its own applied position %d", got, behindAt)
	}

	// The refusal, as an operator would find it on the replica.
	waitUntil(t, func() bool { _, halted := ahead.coord.ReplicationHalt(); return halted }, func() string {
		return fmt.Sprintf("%s (applied %d) never reported that it cannot follow %s (log head %d)",
			ahead.id, aheadAt, behind.id, behind.log.CurrentSeq())
	})
	reason, _ := ahead.coord.ReplicationHalt()
	for _, want := range []string{
		fmt.Sprint(aheadAt),  // where this replica is
		fmt.Sprint(behindAt), // where the leader's log ended
		behind.id,            // which leader
		"docs/cluster-operations.md",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("the halt reason does not mention %q: %s", want, reason)
		}
	}
	if _, member := memberByID(behind.coord.joinHandler, ahead.id); member {
		t.Errorf("the new leader recorded %s as a joined member", ahead.id)
	}

	// The new leader writes well past the refused replica's position.
	post := writeKeys(behind, "probe/post", 10)
	if head := behind.log.CurrentSeq(); head <= aheadAt {
		t.Fatalf("precondition: the new leader's head %d must have passed the refused replica's position %d", head, aheadAt)
	}

	// Nothing has changed its mind so far.
	if _, halted := ahead.coord.ReplicationHalt(); !halted {
		t.Fatalf("%s stopped reporting the halt once the leader's log passed it", ahead.id)
	}

	// Now make it ask again. Its own join loop stays away from a leader it has
	// refused for as long as it can see the term is unchanged; this is the
	// attempt it makes when it cannot tell. The leader's position check passes
	// this time — its head is past the replica — so the leader admits the
	// join. The replica must still refuse it.
	ctx, cancel := context.WithTimeout(context.Background(), clusterWait)
	defer cancel()
	if _, err := ahead.coord.joinClient.Join(ctx, behind.addr); !errors.Is(err, ErrFollowerAhead) {
		t.Fatalf("%s asked %s again after its log had passed it and got err = %v, want ErrFollowerAhead: "+
			"joining now means skipping the leader's entries numbered %d through %d",
			ahead.id, behind.id, err, behindAt+1, aheadAt)
	}
	if _, halted := ahead.coord.ReplicationHalt(); !halted {
		t.Errorf("%s no longer reports the halt after refusing a second time", ahead.id)
	}
	if got := ahead.count(post); got != 0 {
		t.Errorf("%s applied %d of the new leader's writes while refusing to follow it", ahead.id, got)
	}
	if got := ahead.applier.LastApplied(); got != aheadAt {
		t.Errorf("%s applied position moved from %d to %d while refusing to follow", ahead.id, aheadAt, got)
	}

	// It is still a voter with a live connection to the leader: only
	// replication stopped. The admitted-then-refused join above replaced that
	// connection with one the replica then closed, so this also shows the
	// link comes back by itself.
	waitUntil(t, func() bool {
		p, ok := behind.coord.mgr.GetPeer(ahead.id)
		return ok && p.IsConnected()
	}, func() string {
		return fmt.Sprintf("the new leader has no live connection to %s; a refused replica must stay reachable as a voter", ahead.id)
	})
	if !behind.coord.election.IsVoter(ahead.id) {
		t.Errorf("%s is no longer a voter on the new leader", ahead.id)
	}
	if !behind.coord.IsLeader() {
		t.Errorf("%s lost the leader role while refusing %s", behind.id, ahead.id)
	}
	// And nothing it holds was thrown away by refusing.
	if got := ahead.count(onlyAhead); got != len(onlyAhead) {
		t.Errorf("%s now holds %d of the %d entries only it had", ahead.id, got, len(onlyAhead))
	}
}

// restartNode builds a fresh coordinator for a stopped node over the same store
// and address, the way a restarted process finds its data directory. Nothing
// the old coordinator held in memory survives.
func restartNode(t *testing.T, old *clusterNode) *clusterNode {
	t.Helper()
	if !old.stopped {
		t.Fatalf("%s must be stopped before it is restarted", old.id)
	}
	ln, err := net.Listen("tcp", old.addr)
	if err != nil {
		t.Fatalf("listen again on %s for %s: %v", old.addr, old.id, err)
	}
	n := &clusterNode{t: t, id: old.id, addr: old.addr, ln: ln, db: old.db}
	n.log = NewReplicationLog(n.db)
	n.applier = NewApplier(n.db)
	epochStore, err := NewEpochStore(n.db)
	if err != nil {
		t.Fatalf("epoch store for restarted %s: %v", n.id, err)
	}
	cfg := *old.coord.cfg
	n.coord = NewClusterCoordinator(&cfg, n.log, n.applier, epochStore)
	n.coord.haltRetry = testHaltRetry
	n.appendFn = LocalAppendFunc(n.coord, n.log)
	// Registered after the cluster's own cleanup, so it runs before the stores
	// are closed.
	t.Cleanup(n.stop)
	return n
}

// startThreeNodesTuned is startThreeNodes with one difference: tune sees each
// node's configuration before the node starts.
func startThreeNodesTuned(t *testing.T, tune func(id string, cfg *config.ClusterConfig)) (leader, f1, f2 *clusterNode) {
	t.Helper()
	nodes := newCluster(t,
		[]string{"node-l", "node-f1", "node-f2"},
		[]string{"primary", "replica", "replica"})
	for _, n := range nodes {
		tune(n.id, n.coord.cfg)
	}
	leader, f1, f2 = nodes[0], nodes[1], nodes[2]
	f1.start()
	f2.start()
	leader.start()
	waitUntil(t, leader.coord.IsLeader, func() string { return "node-l never became leader" })
	leader.write("probe/formed", "invented marker")
	for _, f := range []*clusterNode{f1, f2} {
		f := f
		waitUntil(t, func() bool { return f.has("probe/formed") },
			func() string { return f.id + " never received the leader's first write" })
	}
	linked := func(a, b *clusterNode) bool {
		p, ok := a.coord.mgr.GetPeer(b.id)
		return ok && p.IsConnected() && a.coord.election.IsVoter(b.id)
	}
	waitUntil(t, func() bool { return linked(f1, f2) && linked(f2, f1) },
		func() string { return "the two followers never connected to each other as voters" })
	return leader, f1, f2
}

// aheadAndBehind brings up three nodes and leaves one follower five entries
// ahead of the other. It returns the positions of both.
func aheadAndBehind(t *testing.T) (leader, ahead, behind *clusterNode, aheadAt, behindAt uint64) {
	t.Helper()
	return aheadAndBehindTuned(t, func(string, *config.ClusterConfig) {})
}

func aheadAndBehindTuned(t *testing.T, tune func(id string, cfg *config.ClusterConfig)) (leader, ahead, behind *clusterNode, aheadAt, behindAt uint64) {
	t.Helper()
	leader, ahead, behind = startThreeNodesTuned(t, tune)
	pre := writeKeys(leader, "probe/pre", 20)
	for _, f := range []*clusterNode{ahead, behind} {
		f := f
		waitUntil(t, func() bool { return f.count(pre) == len(pre) }, func() string {
			return fmt.Sprintf("%s holds %d of %d early writes", f.id, f.count(pre), len(pre))
		})
	}
	stopStreamTo(leader, behind)
	onlyAhead := writeKeys(leader, "probe/only-ahead", 5)
	waitUntil(t, func() bool { return ahead.count(onlyAhead) == len(onlyAhead) }, func() string {
		return fmt.Sprintf("%s holds %d of %d late writes", ahead.id, ahead.count(onlyAhead), len(onlyAhead))
	})
	aheadAt, behindAt = ahead.applier.LastApplied(), behind.applier.LastApplied()
	if aheadAt != behindAt+uint64(len(onlyAhead)) {
		t.Fatalf("precondition: applied positions are %d and %d, want them %d apart", aheadAt, behindAt, len(onlyAhead))
	}
	return leader, ahead, behind, aheadAt, behindAt
}

// assertRefusedNotSkipping checks the one outcome that is acceptable for a
// replica that holds entries its leader does not: it reports that it cannot
// follow, and it has applied none of the leader's writes.
func assertRefusedNotSkipping(t *testing.T, ahead, leader *clusterNode, aheadAt uint64, post []string) {
	t.Helper()
	waitUntil(t, func() bool {
		_, halted := ahead.coord.ReplicationHalt()
		return halted || ahead.applier.LastApplied() >= leader.log.CurrentSeq()
	}, func() string {
		return fmt.Sprintf("%s neither refused %s nor caught up with it", ahead.id, leader.id)
	})
	reason, halted := ahead.coord.ReplicationHalt()
	if !halted {
		t.Fatalf("%s joined %s without a refusal and skipped its writes: it holds %d of the %d writes the leader accepted, "+
			"its applied position went from %d to %d (leader head %d), and it reports replication lag %d",
			ahead.id, leader.id, ahead.count(post), len(post), aheadAt, ahead.applier.LastApplied(),
			leader.log.CurrentSeq(), ahead.coord.ReplicationLag())
	}
	if got := ahead.count(post); got != 0 {
		t.Errorf("%s applied %d of the leader's writes while refusing to follow it", ahead.id, got)
	}
	if got := ahead.applier.LastApplied(); got != aheadAt {
		t.Errorf("%s applied position moved from %d to %d", ahead.id, aheadAt, got)
	}
	for _, want := range []string{fmt.Sprint(aheadAt), leader.id, "docs/cluster-operations.md"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the halt reason does not mention %q: %s", want, reason)
		}
	}
	t.Logf("%s refused: %s", ahead.id, reason)
}

// TestFailover_AheadFollowerIsRefusedAfterTheLeaderHasWrittenPastIt is the
// failover the position comparison cannot see. The replica that is ahead does
// not reach the new leader at once: its own rejoin backoff takes a second or
// more. In that time the new leader takes ordinary writes, more of them than
// the replica was ahead by. By the time the replica asks, the leader's log
// head is past the replica's position, and comparing the two numbers says
// everything is fine.
//
// It is not. The replica's last five entries came from the dead leader, and
// the new leader has numbered five different entries the same way. The leader
// must refuse because the entry at the replica's position is not its own.
func TestFailover_AheadFollowerIsRefusedAfterTheLeaderHasWrittenPastIt(t *testing.T) {
	leader, ahead, behind, aheadAt, _ := aheadAndBehind(t)

	leader.stop()
	promote(t, behind)
	// Ordinary traffic on the new leader, before the other replica's rejoin.
	post := writeKeys(behind, "probe/post", 10)
	if head := behind.log.CurrentSeq(); head <= aheadAt {
		t.Fatalf("precondition: the new leader's head %d must be past the other replica's position %d", head, aheadAt)
	}

	assertRefusedNotSkipping(t, ahead, behind, aheadAt, post)
}

// TestFailover_AheadFollowerThatRestartsIsStillRefused: the same replica, but
// it is down while the new leader takes its writes and comes back as a fresh
// process. Nothing it remembered in memory survives, and its first join comes
// after the leader's log has passed it, with certainty rather than by timing.
// What it kept is on disk: its applied position and the checksum of the entry
// it applied there.
func TestFailover_AheadFollowerThatRestartsIsStillRefused(t *testing.T) {
	leader, ahead, behind, aheadAt, _ := aheadAndBehind(t)

	leader.stop()
	promote(t, behind) // while the other replica can still vote
	ahead.stop()
	post := writeKeys(behind, "probe/post", 10)
	if head := behind.log.CurrentSeq(); head <= aheadAt {
		t.Fatalf("precondition: the new leader's head %d must be past the other replica's position %d", head, aheadAt)
	}

	back := restartNode(t, ahead)
	if got := back.applier.LastApplied(); got != aheadAt {
		t.Fatalf("precondition: restarted %s loaded applied position %d, want %d", back.id, got, aheadAt)
	}
	back.start()

	assertRefusedNotSkipping(t, back, behind, aheadAt, post)
}

// TestLeaderRestart_LevelFollowersAreNotRefused is the control for the two
// tests above, and the direction that must never go wrong: a healthy replica
// refused. The leader restarts as a fresh process while both followers are
// level with it, leads again, and takes writes. Each follower reports a
// position and a checksum the restarted leader has to recognise from its own
// log on disk. Neither may be refused, and both must receive every write.
func TestLeaderRestart_LevelFollowersAreNotRefused(t *testing.T) {
	leader, f1, f2 := startThreeNodes(t)

	pre := writeKeys(leader, "probe/pre", 20)
	for _, f := range []*clusterNode{f1, f2} {
		f := f
		waitUntil(t, func() bool { return f.applier.LastApplied() == leader.log.CurrentSeq() && f.count(pre) == len(pre) },
			func() string { return f.id + " never caught up before the restart" })
	}

	leader.stop()
	back := restartNode(t, leader)
	back.start()
	waitUntil(t, back.coord.IsLeader, func() string { return "the restarted leader never led again" })
	post := writeKeys(back, "probe/post", 10)

	for _, f := range []*clusterNode{f1, f2} {
		f := f
		waitUntil(t, func() bool {
			_, halted := f.coord.ReplicationHalt()
			return halted || f.count(post) == len(post)
		}, func() string {
			return fmt.Sprintf("%s holds %d of %d writes made after the restart (applied %d, leader head %d)",
				f.id, f.count(post), len(post), f.applier.LastApplied(), back.log.CurrentSeq())
		})
		if reason, halted := f.coord.ReplicationHalt(); halted {
			t.Errorf("%s was refused by its own leader after the leader restarted: %s", f.id, reason)
		}
	}
}

// refusedThenPruned sets up the situation the two tests below start from. A
// replica has been refused by the new leader, and the new leader has since
// pruned the entry at the replica's position, so it can no longer compare.
// The new leader runs with a small backlog ceiling so that a few dozen writes
// are enough. The production ceiling is 5000 entries and behaves the same.
func refusedThenPruned(t *testing.T) (ahead, behind *clusterNode, aheadAt uint64, post []string) {
	t.Helper()
	leader, ahead, behind, aheadAt, _ := aheadAndBehindTuned(t, func(id string, cfg *config.ClusterConfig) {
		if id == "node-f2" { // the node that will win
			cfg.MaxLogBacklog = 20
			cfg.PruneIntervalSec = 1
		}
	})
	leader.stop()
	promote(t, behind)
	post = writeKeys(behind, "probe/post", 10)
	assertRefusedNotSkipping(t, ahead, behind, aheadAt, post)

	writeKeys(behind, "probe/bulk", 40)
	waitUntil(t, func() bool {
		_, held, err := behind.log.sumAt(aheadAt)
		return err == nil && !held
	}, func() string {
		return fmt.Sprintf("%s never pruned the entry at seq %d (log head %d)", behind.id, aheadAt, behind.log.CurrentSeq())
	})
	if _, halted := ahead.coord.ReplicationHalt(); !halted {
		t.Fatalf("precondition: %s must still be refused before anything restarts", ahead.id)
	}
	return ahead, behind, aheadAt, post
}

// TestFailover_RefusedFollowerStaysRefusedAfterItRestarts: restarting a node
// that reports "down" is the first thing an operator does. The refusal must
// survive it, and it must survive it when the leader can no longer prove
// anything: by now the leader has pruned the entry it refused the replica
// over, and all it can say is that it cannot compare.
//
// A replica that has been refused is admitted again only on proof that its
// last entry is the leader's. "Cannot compare" is not that.
func TestFailover_RefusedFollowerStaysRefusedAfterItRestarts(t *testing.T) {
	ahead, behind, aheadAt, post := refusedThenPruned(t)

	ahead.stop()
	back := restartNode(t, ahead)

	// Before it has talked to anyone: the refusal is on its disk.
	reason, halted := back.coord.ReplicationHalt()
	if !halted {
		t.Fatalf("after a restart %s no longer knows it was refused: its health would answer ok before it has asked any leader", back.id)
	}
	for _, want := range []string{fmt.Sprint(aheadAt), behind.id, "docs/cluster-operations.md"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason kept across the restart does not mention %q: %s", want, reason)
		}
	}

	back.start()
	// Wait for the restarted node's own join loop to have had its answer from
	// the leader: the record then names who was asked. Or for it to have
	// applied something, which is the failure.
	waitUntil(t, func() bool {
		rec, _ := back.applier.Halt()
		return rec.UpheldBy == behind.id || back.applier.LastApplied() != aheadAt
	}, func() string { return fmt.Sprintf("%s never heard back from %s after its restart", back.id, behind.id) })

	if _, halted := back.coord.ReplicationHalt(); !halted {
		t.Fatalf("%s was admitted after a restart although the leader could not compare its last entry: "+
			"it holds %d of the %d post-failover writes, its applied position went from %d to %d, and it reports replication lag %d",
			back.id, back.count(post), len(post), aheadAt, back.applier.LastApplied(), back.coord.ReplicationLag())
	}
	if got := back.applier.LastApplied(); got != aheadAt {
		t.Errorf("%s applied position moved from %d to %d while refused", back.id, aheadAt, got)
	}
	if got := back.count(post); got != 0 {
		t.Errorf("%s applied %d of the leader's writes while refused", back.id, got)
	}
	if _, member := memberByID(behind.coord.joinHandler, back.id); member {
		t.Errorf("the leader recorded %s as a joined member", back.id)
	}
}

// TestFailover_RefusedFollowerStaysRefusedUnderANewLeaderTerm: the other way a
// refusal used to lapse. The replica keeps running, and the leader restarts
// and leads again under a new epoch. The replica votes for it, which a refused
// replica must still be able to do. The new term's leader has the same pruned
// log and still cannot compare, and the replica must still not follow it.
func TestFailover_RefusedFollowerStaysRefusedUnderANewLeaderTerm(t *testing.T) {
	ahead, behind, aheadAt, post := refusedThenPruned(t)

	epochBefore := behind.coord.CurrentEpoch()
	behind.stop()
	leaderAgain := restartNode(t, behind)
	leaderAgain.start()
	// With the first leader gone, the election needs the refused replica's
	// vote, so the two must be connected again first.
	linked := func(a, b *clusterNode) bool {
		p, ok := a.coord.mgr.GetPeer(b.id)
		return ok && p.IsConnected() && a.coord.election.IsVoter(b.id)
	}
	waitUntil(t, func() bool { return linked(leaderAgain, ahead) && linked(ahead, leaderAgain) },
		func() string {
			return fmt.Sprintf("%s and %s never reconnected after the restart", leaderAgain.id, ahead.id)
		})
	promote(t, leaderAgain)
	newEpoch := leaderAgain.coord.CurrentEpoch()
	if newEpoch <= epochBefore {
		t.Fatalf("precondition: the leader's new term has epoch %d, want it above %d", newEpoch, epochBefore)
	}

	// The replica's own join loop notices the new term and asks its leader.
	waitUntil(t, func() bool {
		rec, _ := ahead.applier.Halt()
		return rec.UpheldEpoch == newEpoch || ahead.applier.LastApplied() != aheadAt
	}, func() string {
		return fmt.Sprintf("%s never asked the leader of the new term (epoch %d)", ahead.id, newEpoch)
	})

	if _, halted := ahead.coord.ReplicationHalt(); !halted {
		t.Fatalf("%s was admitted by the leader of a new term (epoch %d) that could not compare its last entry: "+
			"it holds %d of the %d post-failover writes, its applied position went from %d to %d, and it reports replication lag %d",
			ahead.id, newEpoch, ahead.count(post), len(post), aheadAt, ahead.applier.LastApplied(), ahead.coord.ReplicationLag())
	}
	if got := ahead.applier.LastApplied(); got != aheadAt {
		t.Errorf("%s applied position moved from %d to %d while refused", ahead.id, aheadAt, got)
	}
	if got := ahead.count(post); got != 0 {
		t.Errorf("%s applied %d of the leader's writes while refused", ahead.id, got)
	}
}

// TestFailover_ARefusedReplicaCanWinAnElection walks what the refusal does not
// prevent. A vote is granted on the epoch alone, so a replica that has been
// refused can stand and win, with the vote of the very leader that refused it.
//
// This test records the cluster that results. It is not what anyone would
// design. It is what happens, and it is pinned so that a change to any of it
// is a decision:
//
//   - The refused replica leads, numbering from its own position. Its refusal
//     record is gone, and one ERROR line says the cluster's history changed.
//   - The writes the deposed leader acknowledged in its term are on the
//     deposed leader only.
//   - The deposed leader is NOT refused in turn. It asks to join from its
//     apply cursor, which is where it stood when it was promoted, so nothing
//     it wrote as leader is compared with anything. It follows the new leader,
//     keeps its own term's writes, never receives the entries the new leader
//     had applied past the deposed leader's cursor, and reports no refusal.
//
// The last point is the open defect. If this test fails there because the
// deposed leader is now refused, that is the fix: update the residuals in
// docs/internals/invariants.md and docs/cluster-operations.md with it.
func TestFailover_ARefusedReplicaCanWinAnElection(t *testing.T) {
	logs := captureLogs(t)
	leader, ahead, behind, aheadAt, behindAt := aheadAndBehind(t)
	leader.stop()
	promote(t, behind)
	post := writeKeys(behind, "probe/post", 10)
	assertRefusedNotSkipping(t, ahead, behind, aheadAt, post)
	if _, onDisk := haltOnDisk(ahead.db); !onDisk {
		t.Fatalf("precondition: %s holds no refusal record on disk", ahead.id)
	}
	if got := logs.lines("HISTORY HAS CHANGED LINE"); len(got) != 0 {
		t.Fatalf("precondition: the history-changed line was logged before the refused replica was promoted: %v", got)
	}

	// The refused replica stands. The leader that refused it votes for it.
	promote(t, ahead)
	newEpoch := ahead.coord.CurrentEpoch()

	if reason, halted := ahead.coord.ReplicationHalt(); halted {
		t.Errorf("%s leads and still reports itself refused: %s", ahead.id, reason)
	}
	if _, onDisk := haltOnDisk(ahead.db); onDisk {
		t.Errorf("%s leads and still holds a refusal record on disk: it would report itself refused after a restart", ahead.id)
	}
	// The line is logged by the promoting goroutine after the role has
	// flipped, and promote returns as soon as it sees the flip. Asserting on
	// it at once is a race this test lost about once in a hundred runs, on
	// this branch and in review. Wait for it.
	waitUntil(t, func() bool { return len(logs.lines("HISTORY HAS CHANGED LINE")) > 0 },
		func() string { return "the promotion of a refused replica was never logged as an error" })
	said := logs.lines("level=ERROR", "HISTORY HAS CHANGED LINE", "node="+ahead.id, "was_refused_by="+behind.id,
		fmt.Sprintf("this_node_position=%d", aheadAt), fmt.Sprintf("epoch=%d", newEpoch))
	if len(said) != 1 {
		t.Errorf("the promotion of a refused replica is logged as an error %d times, want once, naming the node, the leader that refused it and the position: %v",
			len(said), logs.lines("HISTORY HAS CHANGED LINE"))
	}

	waitUntil(t, func() bool { return !behind.coord.IsLeader() }, func() string { return behind.id + " never stepped down" })
	fromNew := writeKeys(ahead, "probe/from-the-refused-node", 10)
	if got, want := ahead.log.CurrentSeq(), aheadAt+uint64(len(fromNew)); got != want {
		t.Errorf("%s numbered its writes up to %d, want %d: one past its own position %d onwards", ahead.id, got, want, aheadAt)
	}
	if got := ahead.count(post); got != 0 {
		t.Errorf("%s holds %d of the %d writes the deposed leader acknowledged; it never received them", ahead.id, got, len(post))
	}

	// What the deposed leader does next.
	waitUntil(t, func() bool {
		_, halted := behind.coord.ReplicationHalt()
		return halted || behind.count(fromNew) == len(fromNew)
	}, func() string {
		return fmt.Sprintf("%s was neither refused nor caught up: it holds %d of %d", behind.id, behind.count(fromNew), len(fromNew))
	})
	if reason, halted := behind.coord.ReplicationHalt(); halted {
		t.Fatalf("the deposed leader %s is now refused (%s). That closes the open defect this test records: update the residuals named in its comment", behind.id, reason)
	}
	var onlyAhead []string
	for i := 1; i <= int(aheadAt-behindAt); i++ {
		onlyAhead = append(onlyAhead, fmt.Sprintf("probe/only-ahead/%03d", i))
	}
	if got := ahead.count(onlyAhead); got != len(onlyAhead) {
		t.Fatalf("precondition: %s holds %d of the %d entries only it received from the first leader", ahead.id, got, len(onlyAhead))
	}
	t.Logf("the cluster after a refused replica won: %s leads epoch %d from position %d and holds %d of %d writes the deposed leader acknowledged. "+
		"%s follows it without a refusal: it holds %d of %d of the new leader's writes, still holds %d of %d of its own term's writes, "+
		"and holds %d of the %d entries the new leader had applied past it",
		ahead.id, newEpoch, aheadAt, ahead.count(post), len(post),
		behind.id, behind.count(fromNew), len(fromNew), behind.count(post), len(post), behind.count(onlyAhead), len(onlyAhead))
	if got := behind.count(post); got != len(post) {
		t.Errorf("%s holds %d of its own term's %d writes, want all of them still on its disk", behind.id, got, len(post))
	}
	if got := behind.count(onlyAhead); got != 0 {
		t.Errorf("%s holds %d of the %d entries the new leader applied as a follower; the new leader has no log of them to send", behind.id, got, len(onlyAhead))
	}
}
