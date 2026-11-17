package replication

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The sequence rule at promotion (STO-26), below the level of a running
// cluster: a coordinator, its log and its applier, and nothing on the network.
// failover_cluster_test.go proves the same rule through three live nodes.

// asFollower puts coord in the state runAsLobe leaves a node in.
func asFollower(coord *ClusterCoordinator) {
	coord.roleMu.Lock()
	coord.role = RoleReplica
	coord.roleMu.Unlock()
}

// applyThrough applies one entry per sequence in (from, through] to coord, the
// way a stream from a leader does.
func applyThrough(t *testing.T, coord *ClusterCoordinator, from, through uint64) {
	t.Helper()
	for seq := from + 1; seq <= through; seq++ {
		err := coord.applier.Apply(ReplicationEntry{
			Seq: seq, Op: OpSet,
			Key:   []byte(fmt.Sprintf("probe/applied/%04d", seq)),
			Value: []byte("invented"),
		})
		if err != nil {
			t.Fatalf("apply seq %d: %v", seq, err)
		}
	}
}

// logSeqs returns the sequence of every entry in log above after.
func logSeqs(t *testing.T, log *ReplicationLog, after uint64) []uint64 {
	t.Helper()
	entries, err := log.ReadSince(after, 100_000)
	if err != nil {
		t.Fatalf("ReadSince(%d): %v", after, err)
	}
	seqs := make([]uint64, len(entries))
	for i, e := range entries {
		seqs[i] = e.Seq
	}
	return seqs
}

// TestPromotion_ContinuesSequenceFromAppliedPosition covers every combination
// of "what this node numbered itself" and "what it applied from someone else"
// a promotion can meet. The counter after promotion is the larger of the two,
// it is on disk, and the first entry the new leader appends is one past it.
func TestPromotion_ContinuesSequenceFromAppliedPosition(t *testing.T) {
	cases := []struct {
		name string
		// ownHead is how far this node numbered its own log in an earlier term
		// as leader; applied is how far it has applied another leader's entries.
		ownHead, applied uint64
		wantHead         uint64
	}{
		{name: "empty log and nothing applied: a brand-new cluster still starts at 1", ownHead: 0, applied: 0, wantHead: 0},
		{name: "never led, followed through 40", ownHead: 0, applied: 40, wantHead: 40},
		{name: "led through 25 and never followed: a leader restart keeps its own head", ownHead: 25, applied: 0, wantHead: 25},
		{name: "led through 25, then followed through 40", ownHead: 25, applied: 40, wantHead: 40},
		{name: "led through 42, then followed only through 41: the counter is never lowered", ownHead: 42, applied: 41, wantHead: 42},
		{name: "own head and applied position equal", ownHead: 30, applied: 30, wantHead: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coord, db := newTestCoordinator(t, "replica")
			appendFn := LocalAppendFunc(coord, coord.repLog)

			// An earlier term as leader: role not yet settled, so appends land.
			for i := uint64(1); i <= tc.ownHead; i++ {
				if err := appendFn(uint8(OpSet), []byte(fmt.Sprintf("probe/own/%04d", i)), []byte("invented")); err != nil {
					t.Fatalf("append own entry %d: %v", i, err)
				}
			}
			asFollower(coord)
			if tc.applied > 0 {
				// The applier's cursor starts at 0 on a node that has only led;
				// what matters to the rule is where it ends.
				applyThrough(t, coord, tc.applied-1, tc.applied)
			}
			// A follower's own writes must not have moved its counter.
			if err := appendFn(uint8(OpSet), []byte("probe/follower-local"), []byte("invented")); err != nil {
				t.Fatalf("follower-local append: %v", err)
			}
			if got := coord.repLog.CurrentSeq(); got != tc.ownHead {
				t.Fatalf("precondition: log head = %d while following, want %d", got, tc.ownHead)
			}

			simulatePromotion(coord, 1)

			if !coord.IsLeader() {
				t.Fatalf("role after promotion = %v, want primary", coord.Role())
			}
			if got := coord.repLog.CurrentSeq(); got != tc.wantHead {
				t.Fatalf("log head after promotion = %d, want %d (own head %d, applied %d)",
					got, tc.wantHead, tc.ownHead, tc.applied)
			}
			// On disk, not just in this process: a checkpoint taken now, or a
			// restart before the first write, must see the same head.
			if got := NewReplicationLog(db).CurrentSeq(); got != tc.wantHead {
				t.Errorf("persisted log head after promotion = %d, want %d", got, tc.wantHead)
			}

			if err := appendFn(uint8(OpSet), []byte("probe/first-as-leader"), []byte("invented")); err != nil {
				t.Fatalf("first append as leader: %v", err)
			}
			seqs := logSeqs(t, coord.repLog, tc.wantHead)
			if len(seqs) != 1 || seqs[0] != tc.wantHead+1 {
				t.Fatalf("entries above %d after the first write as leader = %v, want exactly [%d]",
					tc.wantHead, seqs, tc.wantHead+1)
			}
			if seqs[0] <= tc.applied {
				t.Errorf("first entry as leader is seq %d, at or below the %d this node had applied: "+
					"a follower at that position discards it as already applied", seqs[0], tc.applied)
			}
		})
	}
}

// streamTo applies every entry in the leader's log to follower, from the very
// start of the log — what a stream to a follower the leader has no ack for
// does — and returns the sequences the follower actually applied.
func streamTo(t *testing.T, leader *ReplicationLog, follower *Applier) (applied []uint64) {
	t.Helper()
	entries, err := leader.ReadSince(0, 100_000)
	if err != nil {
		t.Fatalf("read leader log: %v", err)
	}
	for _, e := range entries {
		before := follower.LastApplied()
		if err := follower.Apply(e); err != nil {
			t.Fatalf("apply seq %d: %v", e.Seq, err)
		}
		if follower.LastApplied() != before {
			applied = append(applied, e.Seq)
		}
	}
	return applied
}

// TestPromotion_FollowersAtAndBehindReceiveEveryNewEntry is the follower's side
// of the rule. A leader promoted at position 40 writes five entries. A
// follower that had applied exactly through 40, and one that was seven entries
// short of it, each apply all five.
//
// The follower that was behind still lacks the seven entries between its
// position and 40: the new leader applied them as a follower and never logged
// them, so its log has nothing in that range to send. This test pins what the
// sequence rule owes that follower — none of the new leader's entries are
// skipped — and states the rest rather than asserting it away. Telling such a
// follower that it is incomplete belongs to the join-gap work.
func TestPromotion_FollowersAtAndBehindReceiveEveryNewEntry(t *testing.T) {
	const promotedAt, newEntries = 40, 5

	leader, _ := newTestCoordinator(t, "replica")
	asFollower(leader)
	applyThrough(t, leader, 0, promotedAt)
	simulatePromotion(leader, 2)
	appendFn := LocalAppendFunc(leader, leader.repLog)
	for i := 1; i <= newEntries; i++ {
		if err := appendFn(uint8(OpSet), []byte(fmt.Sprintf("probe/new-term/%02d", i)), []byte("invented")); err != nil {
			t.Fatalf("append %d as leader: %v", i, err)
		}
	}
	want := make([]uint64, newEntries)
	for i := range want {
		want[i] = promotedAt + uint64(i) + 1
	}

	for _, tc := range []struct {
		name     string
		position uint64
	}{
		{name: "exactly at the new leader's position", position: promotedAt},
		{name: "behind the new leader's position", position: promotedAt - 7},
		{name: "a follower that has applied nothing", position: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			follower, db := newTestCoordinator(t, "replica")
			asFollower(follower)
			if tc.position > 0 {
				applyThrough(t, follower, tc.position-1, tc.position)
			}

			got := streamTo(t, leader.repLog, follower.applier)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("follower at %d applied %v from the new leader, want every new entry %v", tc.position, got, want)
			}
			for i := 1; i <= newEntries; i++ {
				key := []byte(fmt.Sprintf("probe/new-term/%02d", i))
				if _, closer, err := db.Get(key); err != nil {
					t.Errorf("follower at %d is missing %s: %v", tc.position, key, err)
				} else {
					closer.Close()
				}
			}
			if got := follower.applier.LastApplied(); got != promotedAt+newEntries {
				t.Errorf("follower applied position = %d, want the new leader's head %d", got, promotedAt+newEntries)
			}
			if tc.position < promotedAt {
				t.Logf("not covered by this rule: the new leader's log holds nothing in (%d, %d], "+
					"so this follower never receives those %d entries and is not told so",
					tc.position, promotedAt, promotedAt-tc.position)
			}
		})
	}
}

// TestPromotion_EveryPathContinuesTheSequence drives each way a node becomes
// leader and checks the counter after each one. They all funnel through
// handlePromotion; this is what notices if one stops doing so.
func TestPromotion_EveryPathContinuesTheSequence(t *testing.T) {
	const applied = 37

	follow := func(t *testing.T, role string) *ClusterCoordinator {
		t.Helper()
		coord, _ := newTestCoordinator(t, role)
		asFollower(coord)
		applyThrough(t, coord, 0, applied)
		return coord
	}
	check := func(t *testing.T, coord *ClusterCoordinator) {
		t.Helper()
		if !coord.IsLeader() {
			t.Fatalf("role = %v, want primary", coord.Role())
		}
		if got := coord.repLog.CurrentSeq(); got != applied {
			t.Fatalf("log head after promotion = %d, want the applied position %d", got, applied)
		}
	}

	t.Run("won election (Election.StartElection)", func(t *testing.T) {
		coord := follow(t, "replica")
		coord.election.RegisterVoter(coord.cfg.NodeID) // sole voter: its own vote is a quorum
		if err := coord.election.StartElection(context.Background()); err != nil {
			t.Fatalf("StartElection: %v", err)
		}
		check(t, coord)
	})

	t.Run("designated primary asserting leadership at bootstrap", func(t *testing.T) {
		coord := follow(t, "primary")
		coord.election.mu.Lock()
		coord.election.state = ElectionCandidate
		coord.election.candidateEpoch = 3
		coord.election.mu.Unlock()
		coord.bootstrapPromoteIfDesignatedPrimary()
		check(t, coord)
	})

	t.Run("graceful handoff target (HandleHandoff)", func(t *testing.T) {
		coord := follow(t, "replica")
		if _, err := coord.epochStore.Advance(4); err != nil {
			t.Fatalf("seed epoch: %v", err)
		}
		payload, err := msgpack.Marshal(mbp.HandoffMessage{TargetID: coord.cfg.NodeID, Epoch: 4, CortexSeq: applied})
		if err != nil {
			t.Fatalf("marshal handoff: %v", err)
		}
		// The handoff promotes and then fails to ack a sender this test never
		// connected. The promotion is what is under test.
		_ = coord.HandleHandoff("node-old-leader", payload)
		check(t, coord)
	})

	t.Run("crash recovery from a persisted cortex role (Run)", func(t *testing.T) {
		coord := follow(t, "primary")
		if _, err := coord.epochStore.Advance(5); err != nil {
			t.Fatalf("seed epoch: %v", err)
		}
		if err := coord.epochStore.PersistRole("cortex"); err != nil {
			t.Fatalf("persist role: %v", err)
		}
		// runAsLobe is not what set this node's role in a real restart; clear
		// it so Run starts from the state a fresh process has.
		coord.roleMu.Lock()
		coord.role = RoleUnknown
		coord.roleMu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = coord.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })

		waitUntil(t, coord.IsLeader, func() string { return "node never recovered its cortex role" })
		check(t, coord)
	})
}

// TestPromotion_NoAppendIsNumberedAtOrBelowAppliedPosition is the ordering
// half of the rule: the counter moves BEFORE the role does. Writers hammer the
// replication callback the whole time a follower is being promoted. While it
// follows, their appends are suppressed; the instant it leads, they land. Not
// one of them may carry a sequence the node had already applied, and no
// sequence may be issued twice.
func TestPromotion_NoAppendIsNumberedAtOrBelowAppliedPosition(t *testing.T) {
	const applied, writers, perWriter = 50, 8, 40

	for round := 0; round < 20; round++ {
		coord, _ := newTestCoordinator(t, "replica")
		asFollower(coord)
		applyThrough(t, coord, applied-1, applied)
		appendFn := LocalAppendFunc(coord, coord.repLog)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for i := 0; i < perWriter; i++ {
					if err := appendFn(uint8(OpSet), []byte(fmt.Sprintf("probe/w%d/%03d", w, i)), []byte("invented")); err != nil {
						t.Errorf("append: %v", err)
						return
					}
				}
			}(w)
		}
		close(start)
		simulatePromotion(coord, 1)
		wg.Wait()

		seen := make(map[uint64]bool)
		for _, seq := range logSeqs(t, coord.repLog, 0) {
			if seq <= applied {
				t.Fatalf("round %d: an entry was numbered %d, at or below the applied position %d", round, seq, applied)
			}
			if seen[seq] {
				t.Fatalf("round %d: sequence %d was issued twice", round, seq)
			}
			seen[seq] = true
		}
	}
}

// TestPromotion_AppliedEntryIsCountedBeforeTheRoleChanges is the other
// ordering: against the applier. Entries from the previous leader keep
// arriving while the node is promoted. Every entry that was applied while the
// node was still a follower must be below the first sequence the node numbers
// as leader; an entry that slipped in between "read the applied position" and
// "become leader" would share its number with the new leader's first write.
//
// The commit hook runs inside Apply, under the applier's lock, so "the hook saw
// a follower" means the whole apply finished before the role changed.
func TestPromotion_AppliedEntryIsCountedBeforeTheRoleChanges(t *testing.T) {
	const base, incoming = 100, 400

	for round := 0; round < 30; round++ {
		coord, _ := newTestCoordinator(t, "replica")
		asFollower(coord)
		applyThrough(t, coord, base-1, base)

		var hookCalls, appliedAsFollower atomic.Uint64
		appliedAsFollower.Store(base)
		coord.applier.SetCommitHooks(func([]byte) {
			seq := base + hookCalls.Add(1)
			if !coord.IsLeader() {
				appliedAsFollower.Store(seq)
			}
		}, nil)

		streaming := make(chan struct{})
		go func() {
			defer close(streaming)
			for seq := uint64(base + 1); seq <= base+incoming; seq++ {
				if err := coord.applier.Apply(ReplicationEntry{Seq: seq, Op: OpSet, Key: []byte("probe/tail"), Value: []byte("invented")}); err != nil {
					t.Errorf("apply seq %d: %v", seq, err)
					return
				}
			}
		}()
		simulatePromotion(coord, 1)
		headAtPromotion := coord.repLog.CurrentSeq()
		<-streaming

		if last := appliedAsFollower.Load(); last > headAtPromotion {
			t.Fatalf("round %d: seq %d was applied while still a follower, but the node took the leader role "+
				"with its log head at %d: its next write reuses a number it has already applied",
				round, last, headAtPromotion)
		}
	}
}

// TestOnlyTakeLeaderRoleAssignsThePrimaryRole keeps the sequence rule from
// being bypassed by a future promotion path. The rule lives in the one
// function that makes a node leader. A second place that gave a node the
// leader role would promote with a stale counter and nothing else would
// notice.
//
// The scan reads the package's source and looks at every place the name
// RolePrimary is used as a VALUE: assigned, passed to a function, returned,
// stored in a variable. Comparing against it (==, !=, a switch case) is how
// code asks "am I the leader" and is always allowed. Using it as a value is
// allowed in takeLeaderRole and in the two places listed below, which describe
// some other node's role or the configured role and never set this node's own.
// Everything else fails, including a setter called with RolePrimary, which an
// earlier version of this scan (it matched only `x.role = RolePrimary`) let
// through.
//
// What it cannot see: the role written as a number (NodeRole(1)), and code in
// another package. Neither exists today.
func TestOnlyTakeLeaderRoleAssignsThePrimaryRole(t *testing.T) {
	// function name -> why RolePrimary may appear there as a value.
	allowed := map[string]string{
		"takeLeaderRole":       "the one place a node becomes leader",
		"streamReplication":    "names the role of the PEER this node has just joined, in the call to reconcileJoinedPeer",
		"roleFromConfigString": "maps the configured role string for the handshake advertisement, not the live role",
	}
	seen := map[string]int{}

	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue // the constant's own declaration is not a use
			}
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				id, ok := n.(*ast.Ident)
				if !ok || id.Name != "RolePrimary" || len(stack) < 2 {
					return true
				}
				switch parent := stack[len(stack)-2].(type) {
				case *ast.BinaryExpr:
					if parent.Op == token.EQL || parent.Op == token.NEQ {
						return true // a comparison
					}
				case *ast.CaseClause:
					return true // a switch case is a comparison
				}
				seen[fn.Name.Name]++
				if _, ok := allowed[fn.Name.Name]; !ok {
					t.Errorf("%s: %s uses RolePrimary as a value. Only takeLeaderRole may give a node the leader role: "+
						"it raises the replication sequence to the applied position first (STO-26). "+
						"If this use does not set this node's own role, add the function to the allowlist with the reason",
						fset.Position(id.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	// Every allowlist entry must still match something, or it is a standing
	// exemption for code that has moved. For takeLeaderRole this is also the
	// check that the scan is looking at what it thinks it is.
	for fn, why := range allowed {
		if seen[fn] == 0 {
			t.Errorf("allowlist entry %q (%s) no longer matches any use of RolePrimary: remove it, or the scan has stopped seeing the code", fn, why)
		}
	}
	if seen["takeLeaderRole"] != 1 {
		t.Errorf("takeLeaderRole uses RolePrimary as a value %d times, want exactly the one assignment", seen["takeLeaderRole"])
	}
}
