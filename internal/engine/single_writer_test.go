package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// #596 / #631 claim 3 — writes to a non-Cortex node are accepted locally,
// returned as success, and never forwarded. The engram is durable on that one
// node, invisible cluster-wide, and destroyed by the next WipeForResnapshot.
//
// Verified on develop before the fix: IsLeader() had no caller on any write
// path — it appeared only inside internal/replication and the REST status
// handlers — so nothing anywhere in the write path knew the node's role.
// ---------------------------------------------------------------------------

// followerGate is the write gate a Lobe installs: this node is not the Cortex.
func followerGate() error {
	return &NotLeaderError{Role: "lobe", LeaderID: "cortex-1", LeaderAddr: "10.0.0.1:8474"}
}

// TestSingleWriter_FollowerRefusesClientWrite is the #596 reproduction: a
// muninn_remember delivered to a Lobe. Before the fix it returned success with
// an engram id that only that node would ever see.
func TestSingleWriter_FollowerRefusesClientWrite(t *testing.T) {
	cortex, cleanupC := testEnv(t)
	defer cleanupC()
	lobe, cleanupL := testEnv(t)
	defer cleanupL()

	lobe.SetWriteGate(followerGate)

	resp, err := lobe.Write(context.Background(), &mbp.WriteRequest{
		Vault: "default", Content: "written on a lobe", Concept: "probe",
	})
	if err == nil {
		// Prove the divergence the caller was never told about: durable here,
		// absent on the Cortex, and no error was returned.
		if _, rerr := cortex.Read(context.Background(), &mbp.ReadRequest{Vault: "default", ID: resp.ID}); rerr != nil {
			t.Fatalf("a Lobe accepted a client write and returned success (id=%s); "+
				"the engram is durable on the Lobe and NOT on the Cortex (read there: %v). "+
				"Silent cluster-wide data loss — #596.", resp.ID, rerr)
		}
		t.Fatalf("a Lobe accepted a client write and returned success (id=%s) — #596", resp.ID)
	}
	if !errors.Is(err, ErrNotLeader) {
		t.Fatalf("Write on a follower: err = %v, want ErrNotLeader", err)
	}

	// The rejection must name where to go instead — a client that cannot find
	// the Cortex from the error is only marginally better off than a silent loss.
	var nle *NotLeaderError
	if !errors.As(err, &nle) {
		t.Fatalf("err does not carry a *NotLeaderError: %v", err)
	}
	if nle.LeaderID != "cortex-1" || nle.LeaderAddr != "10.0.0.1:8474" {
		t.Errorf("NotLeaderError leader hint = %q/%q, want cortex-1/10.0.0.1:8474", nle.LeaderID, nle.LeaderAddr)
	}
	if !strings.Contains(err.Error(), "cortex-1") {
		t.Errorf("error text must name the leader for humans too: %q", err.Error())
	}

	// And nothing was written locally.
	if st, serr := lobe.Stat(context.Background(), &mbp.StatRequest{Vault: "default"}); serr == nil && st != nil && st.EngramCount != 0 {
		t.Errorf("refused write still landed: engram count = %d, want 0", st.EngramCount)
	}
}

// TestSingleWriter_LeaderIsUnaffected: the gate is specific to non-leaders.
func TestSingleWriter_LeaderIsUnaffected(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	eng.SetWriteGate(func() error { return nil }) // this node IS the Cortex

	if _, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "default", Content: "written on the cortex",
	}); err != nil {
		t.Fatalf("Cortex write refused: %v", err)
	}
}

// TestSingleWriter_NoGateMeansSingleNode: a standalone (non-cluster) server
// installs no gate at all and must be completely unaffected. Fail-open here is
// correct — there is no leader to be wrong about.
func TestSingleWriter_NoGateMeansSingleNode(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	if _, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "default", Content: "standalone write",
	}); err != nil {
		t.Fatalf("standalone write refused: %v", err)
	}
}

// additiveWriteOps returns one closure per exported *Engine method classified
// as ADDITIVE by the append-mode census (appendAdditive). Every one of them
// originates new state and so must be refused on a follower.
//
// The destructive half is not duplicated here: it is guardedOps(), reused
// verbatim by TestSingleWriter_RefusesEveryWriteOnFollower. Between the two,
// the completeness of this gate is inherited from TestAppendMode_MethodCensus
// — a newly-added *Engine method cannot be written without being classified,
// and every method in either write bucket is exercised against a follower here.
func additiveWriteOps(eng *Engine, ctx context.Context) map[string]func() error {
	const v, id = "single-writer-vault", "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	return map[string]func() error{
		"Write": func() error { _, e := eng.Write(ctx, &mbp.WriteRequest{Vault: v, Content: "c"}); return e },
		"WriteBatch": func() error {
			_, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{{Vault: v, Content: "c"}})
			return errs[0]
		},
		"WriteIdempotency": func() error { return eng.WriteIdempotency(ctx, "op-1", id) },
		"RememberTree": func() error {
			_, e := eng.RememberTree(ctx, &RememberTreeRequest{Vault: v, Root: TreeNodeInput{Content: "root"}})
			return e
		},
		"AddChild": func() error {
			_, e := eng.AddChild(ctx, v, id, &AddChildInput{Content: "child"})
			return e
		},
		"RegisterVaultName": func() error { return eng.RegisterVaultName(v) },
		"Intend":            func() error { _, e := eng.Intend(ctx, v, "do a thing", []string{"cue"}, nil, false, nil); return e },
	}
}

// TestSingleWriter_AdditiveCensusIsComplete pins additiveWriteOps against the
// append-mode census's additive bucket. If someone adds an additive write
// method, TestAppendMode_MethodCensus forces them to classify it and this
// forces them to prove it is leader-gated.
func TestSingleWriter_AdditiveCensusIsComplete(t *testing.T) {
	ops := additiveWriteOps(nil, context.Background())
	for name := range appendAdditive {
		if _, ok := ops[name]; !ok {
			t.Errorf("additive *Engine method %q has no follower-rejection case: add it to "+
				"additiveWriteOps so the cluster single-writer gate is proven for it (#596)", name)
		}
	}
	for name := range ops {
		if !appendAdditive[name] {
			t.Errorf("additiveWriteOps has %q, which the append census does not classify as additive", name)
		}
	}
}

// TestSingleWriter_RefusesEveryWriteOnFollower is the completeness pin: EVERY
// engine method that originates state — additive or destructive — must refuse
// on a non-leader. Anything that slipped the gate is a silent-divergence hole
// of exactly the #596 shape on all four transports at once.
func TestSingleWriter_RefusesEveryWriteOnFollower(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	eng.SetWriteGate(followerGate)

	ctx := context.Background()
	all := map[string]func() error{}
	for name, fn := range guardedOps(eng, ctx) {
		all[name] = fn
	}
	for name, fn := range additiveWriteOps(eng, ctx) {
		all[name] = fn
	}
	for name, fn := range all {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrNotLeader) {
				t.Errorf("%s on a follower: err = %v, want ErrNotLeader — this write would be "+
					"accepted locally and never replicated (#596)", name, err)
			}
		})
	}
}

// TestSingleWriter_GateRunsBeforeAppendGate documents the ordering choice: the
// cluster gate is checked first, so a client on the wrong node is told to go to
// the Cortex rather than being told its credential is at fault. Both errors are
// true; the actionable one wins.
func TestSingleWriter_GateRunsBeforeAppendGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	eng.SetWriteGate(followerGate)

	var z storage.ULID
	err := eng.UpdateTags(withMode("append"), "v", z, []string{"t"})
	if !errors.Is(err, ErrNotLeader) {
		t.Errorf("append-mode write on a follower: err = %v, want ErrNotLeader (cluster gate first)", err)
	}
}
