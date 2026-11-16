package replication

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/scrypster/muninndb/internal/config"
)

// A real three-node cluster inside one test process.
//
// Every node is built the way cmd/muninn builds it: NewClusterCoordinator (no
// WithDB seam), a real loopback listener whose accepted connections run
// ServeConn, Run on its own goroutine, and writes that go through
// LocalAppendFunc — the same role-gated callback the storage layer is handed.
// No join, vote or stream is simulated. Where a test steers the cluster (it
// stops one stream to make a follower lag) it does so through the
// coordinator's own code and says so.

// clusterWait bounds every wait in these tests. It is a hang detector, not a
// timing assumption: each wait polls a condition and returns the moment it
// holds, so a passing run takes as long as the cluster takes.
const clusterWait = 90 * time.Second

type clusterNode struct {
	t        *testing.T
	id       string
	addr     string
	db       *pebble.DB
	log      *ReplicationLog
	applier  *Applier
	coord    *ClusterCoordinator
	appendFn func(op uint8, key, value []byte) error

	ln      net.Listener
	cancel  context.CancelFunc
	runDone chan struct{}
	serveWG sync.WaitGroup
	stopped bool

	// accepted holds every connection the listener handed out, so stop can
	// close them all the way a dying process closes its sockets.
	// acceptClosed is set by stop under the same lock, so a connection the
	// listener returned just before it closed is not served after the node
	// has stopped.
	acceptedMu   sync.Mutex
	accepted     []net.Conn
	acceptClosed bool
}

// newCluster builds one node per id on loopback, each seeded with the others.
// roles[i] is the configured role of ids[i]. Nothing is started.
func newCluster(t *testing.T, ids, roles []string) []*clusterNode {
	t.Helper()
	nodes := make([]*clusterNode, len(ids))
	for i, id := range ids {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen for %s: %v", id, err)
		}
		nodes[i] = &clusterNode{t: t, id: id, addr: ln.Addr().String(), ln: ln}
	}
	for i, n := range nodes {
		db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
		if err != nil {
			t.Fatalf("open store for %s: %v", n.id, err)
		}
		n.db = db
		n.log = NewReplicationLog(db)
		n.applier = NewApplier(db)
		epochStore, err := NewEpochStore(db)
		if err != nil {
			t.Fatalf("epoch store for %s: %v", n.id, err)
		}
		// Seeds: every other node, the highest index first. A follower whose
		// stream dies retries the leader it knew and then its seeds in order, so
		// this ordering only decides how many backoffs a rejoin costs.
		var seeds []string
		for j := len(nodes) - 1; j >= 0; j-- {
			if j != i {
				seeds = append(seeds, nodes[j].addr)
			}
		}
		cfg := &config.ClusterConfig{
			Enabled:       true,
			NodeID:        n.id,
			BindAddr:      n.addr,
			Seeds:         seeds,
			ClusterSecret: "invented-test-cluster-secret",
			Role:          roles[i],
			LeaseTTL:      10,
			HeartbeatMS:   50,
			// The failure detector is taken out of the picture on purpose. A
			// stopped node is never declared down inside a test run, so the only
			// election is the one the test starts, and which node wins it is not
			// left to a race between two detectors.
			SDOWNBeats:       1_000_000,
			PruneIntervalSec: 3600,
		}
		n.coord = NewClusterCoordinator(cfg, n.log, n.applier, epochStore)
		n.appendFn = LocalAppendFunc(n.coord, n.log)
	}
	// Teardown in production order (cmd/muninn/server.go): stop every
	// coordinator and wait for the goroutines that read or write its store,
	// and only then close the stores.
	t.Cleanup(func() {
		for _, n := range nodes {
			n.stop()
		}
		for _, n := range nodes {
			_ = n.db.Close()
		}
	})
	return nodes
}

// start runs the coordinator and then opens the node to inbound connections:
// every accepted connection runs ServeConn, as in a real server.
//
// The order is the one thing here that is stricter than cmd/muninn, which
// starts its listener and Run side by side. Run registers a placeholder voter
// for every seed, and the first handshake with a seed swaps the placeholder
// for the seed's real identity. A handshake that is served BEFORE Run has
// registered the placeholders finds nothing to swap, and the placeholder Run
// adds afterwards is never retired: the node counts one voter too many for
// the rest of its life, and a three-node cluster that loses a node can no
// longer elect. That is a property of the code under test, not of this
// harness, so the harness does not leave it to scheduling: it accepts nothing
// until Run has registered every seed.
func (n *clusterNode) start() {
	n.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.runDone = make(chan struct{})
	go func() {
		defer close(n.runDone)
		_ = n.coord.Run(ctx)
	}()

	// Run registers this node plus one placeholder per seed, and a later
	// identity swap adds the real id before it removes the placeholder, so the
	// voter count reaches this value once and never drops below it.
	registered := 1 + len(n.coord.cfg.Seeds)
	waitUntil(n.t, func() bool {
		n.coord.election.mu.Lock()
		defer n.coord.election.mu.Unlock()
		return len(n.coord.election.voters) >= registered
	}, func() string { return n.id + ": Run never registered its seeds as voters" })

	n.serveWG.Add(1)
	go func() {
		defer n.serveWG.Done()
		for {
			conn, err := n.ln.Accept()
			if err != nil {
				return // listener closed
			}
			n.acceptedMu.Lock()
			if n.acceptClosed {
				n.acceptedMu.Unlock()
				_ = conn.Close()
				return
			}
			n.accepted = append(n.accepted, conn)
			n.acceptedMu.Unlock()
			n.serveWG.Add(1)
			go func() {
				defer n.serveWG.Done()
				n.coord.ServeConn(conn)
			}()
		}
	}()
}

// stop takes the node down the way a killed process goes down as far as its
// peers can tell: the listener closes, every connection drops, nothing more is
// streamed. It returns once no goroutine of this node can touch its store.
func (n *clusterNode) stop() {
	if n.stopped {
		return
	}
	n.stopped = true
	if n.cancel != nil {
		n.cancel()
	}
	_ = n.ln.Close()
	_ = n.coord.Stop()
	// Stop closes the connections the coordinator has registered. A handshake
	// that was still being served when Stop ran registers its connection
	// afterwards, and nothing closes that one while the peer stays up: the
	// loop serving it would read its pings until the peer itself stopped. A
	// killed process leaves no such connection behind, so neither does this.
	n.acceptedMu.Lock()
	n.acceptClosed = true
	for _, conn := range n.accepted {
		_ = conn.Close()
	}
	n.acceptedMu.Unlock()
	if n.runDone != nil {
		select {
		case <-n.runDone:
		case <-time.After(clusterWait):
			n.t.Errorf("%s: coordinator Run did not return after stop", n.id)
		}
	}
	served := make(chan struct{})
	go func() { n.serveWG.Wait(); close(served) }()
	select {
	case <-served:
	case <-time.After(clusterWait):
		n.t.Errorf("%s: connection loops did not exit after stop", n.id)
	}
}

// write commits one key to the node's own store and hands the committed batch
// to the replication callback, in that order — what PebbleStore does on every
// write (storage/impl.go replicateBatch). On a follower the callback
// suppresses the append, exactly as in production.
func (n *clusterNode) write(key, value string) {
	n.t.Helper()
	b := n.db.NewBatch()
	defer b.Close()
	if err := b.Set([]byte(key), []byte(value), nil); err != nil {
		n.t.Fatalf("%s: build batch for %s: %v", n.id, key, err)
	}
	if err := b.Commit(pebble.Sync); err != nil {
		n.t.Fatalf("%s: commit %s: %v", n.id, key, err)
	}
	if err := n.appendFn(uint8(OpBatch), nil, b.Repr()); err != nil {
		n.t.Fatalf("%s: replicate %s: %v", n.id, key, err)
	}
}

// has reports whether the node's store holds key.
func (n *clusterNode) has(key string) bool {
	_, closer, err := n.db.Get([]byte(key))
	if err != nil {
		if !errors.Is(err, pebble.ErrNotFound) {
			n.t.Errorf("%s: read %s: %v", n.id, key, err)
		}
		return false
	}
	_ = closer.Close()
	return true
}

// count returns how many of keys the node holds.
func (n *clusterNode) count(keys []string) int {
	held := 0
	for _, k := range keys {
		if n.has(k) {
			held++
		}
	}
	return held
}

// waitUntil polls cond until it holds. It fails the test with what() if
// clusterWait passes first.
func waitUntil(t *testing.T, cond func() bool, what func() string) {
	t.Helper()
	deadline := time.Now().Add(clusterWait)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %s: %s", clusterWait, what())
		}
		<-tick.C
	}
}

// startThreeNodes brings up a leader and two followers and returns them once
// the cluster is fully formed: both followers streaming from the leader, and
// the two followers connected to each other as voters (a failover election
// needs that link to carry the vote).
func startThreeNodes(t *testing.T) (leader, f1, f2 *clusterNode) {
	t.Helper()
	nodes := newCluster(t,
		[]string{"node-l", "node-f1", "node-f2"},
		[]string{"primary", "replica", "replica"})
	leader, f1, f2 = nodes[0], nodes[1], nodes[2]

	// Followers first, so the leader's startup probe of its seeds is answered
	// at once instead of waiting out a dial to a node that is not serving yet.
	f1.start()
	f2.start()
	leader.start()
	waitUntil(t, leader.coord.IsLeader, func() string { return "node-l never became leader" })

	// A marker write proves each follower is on the stream, not merely joined.
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

// writeKeys writes n keys named <prefix>/NNN to node and returns their names.
func writeKeys(node *clusterNode, prefix string, n int) []string {
	keys := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		k := fmt.Sprintf("%s/%03d", prefix, i)
		node.write(k, "invented value for "+k)
		keys = append(keys, k)
	}
	return keys
}

// promote makes node win an election the way an operator's promote call does
// (POST /v1/replication/promote is Election.StartElection), and waits for it
// to hold the leader role.
func promote(t *testing.T, node *clusterNode) {
	t.Helper()
	if err := node.coord.Election().StartElection(context.Background()); err != nil {
		t.Fatalf("%s: StartElection: %v", node.id, err)
	}
	waitUntil(t, node.coord.IsLeader, func() string {
		return fmt.Sprintf("%s did not win the election it started (state %d, epoch %d)",
			node.id, node.coord.election.State(), node.coord.CurrentEpoch())
	})
}

// TestFailover_SurvivingFollowerReceivesNewLeadersWrites is the defect this
// file exists for, end to end.
//
// Three nodes, both followers attached from the start and fully caught up, no
// prune anywhere. The leader dies, a follower is elected, and the new leader
// accepts writes. The other follower must receive every one of them.
//
// Before the fix it received none. A follower's replication-log counter does
// not move while it applies entries (the applier records last_applied only,
// and a follower's own appends are suppressed), so the promoted node numbered
// its first entry 1. The surviving follower's applier skips every entry whose
// seq is at or below its own applied position, which is the old leader's head,
// so it discarded the new leader's writes one after another — while reporting
// a replication lag of 0.
func TestFailover_SurvivingFollowerReceivesNewLeadersWrites(t *testing.T) {
	leader, f1, f2 := startThreeNodes(t)

	pre := writeKeys(leader, "probe/pre", 20)
	for _, f := range []*clusterNode{f1, f2} {
		f := f
		waitUntil(t, func() bool { return f.count(pre) == len(pre) }, func() string {
			return fmt.Sprintf("%s holds %d of %d writes made before the failover", f.id, f.count(pre), len(pre))
		})
	}
	oldHead := leader.log.CurrentSeq()
	for _, f := range []*clusterNode{f1, f2} {
		if got := f.applier.LastApplied(); got != oldHead {
			t.Fatalf("%s applied position = %d before the failover, want the leader's head %d", f.id, got, oldHead)
		}
	}

	leader.stop()
	promote(t, f2)

	// The numbering rule itself, before any write: the promoted node's log
	// must not be behind anything a follower of the old leader has applied.
	if head, applied := f2.log.CurrentSeq(), f1.applier.LastApplied(); head < applied {
		t.Errorf("promoted %s has log head %d but surviving %s has applied through %d: "+
			"the next %d entries the new leader writes carry sequence numbers the follower skips as already applied",
			f2.id, head, f1.id, applied, applied-head)
	}

	post := writeKeys(f2, "probe/post", 10)

	// Wait for the surviving follower to hold every post-failover write, or
	// for proof that it never will: it has acknowledged an entry from the new
	// leader, and the new leader's head is still at or below the follower's
	// own applied position. In that state every entry in the new leader's log
	// is one the follower's applier skips, however long the test waits.
	skipping := func() bool {
		_, acked := f2.coord.replicaSeqs.Load(f1.id)
		return acked && f2.log.CurrentSeq() <= f1.applier.LastApplied() && f1.count(post) < len(post)
	}
	waitUntil(t, func() bool { return f1.count(post) == len(post) || skipping() }, func() string {
		return fmt.Sprintf("%s holds %d of %d post-failover writes and never acknowledged the new leader",
			f1.id, f1.count(post), len(post))
	})
	if got := f1.count(post); got != len(post) {
		t.Fatalf("after failover to %s, %s received %d of %d writes the new leader accepted "+
			"(new leader log head %d, follower applied position %d, follower-reported replication lag %d)",
			f2.id, f1.id, got, len(post), f2.log.CurrentSeq(), f1.applier.LastApplied(), f1.coord.ReplicationLag())
	}

	if got := f1.count(pre); got != len(pre) {
		t.Errorf("%s holds %d of %d pre-failover writes after the failover", f1.id, got, len(pre))
	}
	if head, applied := f2.log.CurrentSeq(), f1.applier.LastApplied(); applied != head {
		t.Errorf("%s applied position = %d, want the new leader's head %d", f1.id, applied, head)
	}
	if want := oldHead + uint64(len(post)); f2.log.CurrentSeq() != want {
		t.Errorf("new leader's log head = %d, want %d: the old leader's head %d plus the %d writes it accepted",
			f2.log.CurrentSeq(), want, oldHead, len(post))
	}
}

// stopStreamTo makes leader stop shipping entries to follower without telling
// it, so follower falls behind while staying a connected, voting member: a
// replica on a slow link at the moment the leader dies.
func stopStreamTo(leader, follower *clusterNode) {
	leader.coord.stopStreamerForLobe(follower.id)
}

// TestFailover_FollowerBehindNewLeaderReceivesItsWrites: the two followers are
// not level when the leader dies. One holds five entries the other never
// received, and that one wins. The follower that was behind joins it, and none
// of the new leader's writes may be skipped.
//
// The five entries the lagging follower never received are still missing
// afterwards. The new leader applied them as a follower and has nothing in its
// log to send, and nothing reports it. That is a separate, open defect (a
// replica behind the node that won is silently incomplete). This test asserts
// only what the sequence rule owes this follower, and logs the rest.
func TestFailover_FollowerBehindNewLeaderReceivesItsWrites(t *testing.T) {
	leader, ahead, behind := startThreeNodes(t)

	pre := writeKeys(leader, "probe/pre", 20)
	for _, f := range []*clusterNode{ahead, behind} {
		f := f
		waitUntil(t, func() bool { return f.count(pre) == len(pre) }, func() string {
			return fmt.Sprintf("%s holds %d of %d early writes", f.id, f.count(pre), len(pre))
		})
	}
	stopStreamTo(leader, behind)
	gap := writeKeys(leader, "probe/only-ahead", 5)
	waitUntil(t, func() bool { return ahead.count(gap) == len(gap) }, func() string {
		return fmt.Sprintf("%s holds %d of %d late writes", ahead.id, ahead.count(gap), len(gap))
	})

	leader.stop()
	promote(t, ahead)
	post := writeKeys(ahead, "probe/post", 10)

	waitUntil(t, func() bool { return behind.count(post) == len(post) }, func() string {
		return fmt.Sprintf("%s holds %d of %d writes the new leader accepted (leader head %d, follower applied %d)",
			behind.id, behind.count(post), len(post), ahead.log.CurrentSeq(), behind.applier.LastApplied())
	})
	if got := behind.applier.LastApplied(); got != ahead.log.CurrentSeq() {
		t.Errorf("%s applied position = %d, want the new leader's head %d", behind.id, got, ahead.log.CurrentSeq())
	}
	t.Logf("still open, not covered by the sequence rule: %s holds %d of the %d entries written while it lagged, "+
		"and reports replication lag %d", behind.id, behind.count(gap), len(gap), behind.coord.ReplicationLag())
}
