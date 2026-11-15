package engine

// HNSW tombstones are in memory only: a hard delete marks the node deleted in
// the running index, and nothing removes its persisted vector or lists. These
// tests pin what a user sees across a restart. Synthetic vectors only.

import (
	"context"
	"math/rand"
	"strconv"
	"testing"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func tombstoneFixture(t *testing.T, env *rcEnv) (target string, targetVec []float32) {
	t.Helper()
	r := rand.New(rand.NewSource(77))
	var reqs []*mbp.WriteRequest
	for i := 0; i < 40; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "signal box log " + strconv.Itoa(i),
			Content: "lever frame inspection entry " + strconv.Itoa(i), Embedding: rcRand(r)})
	}
	targetVec = rcRand(r)
	reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "turntable pit survey",
		Content: "the turntable pit drain was cleared", Embedding: targetVec})
	ids := env.writeBatch(reqs)
	env.eng.waitWriteTimeIdle()
	return ids[len(ids)-1], targetVec
}

func rawHNSWHas(t *testing.T, env *rcEnv, q []float32, id string) bool {
	t.Helper()
	res, err := env.reg.Search(context.Background(), env.ws, q, 10)
	if err != nil {
		t.Fatal(err)
	}
	u := env.mustULID(id)
	for _, x := range res {
		if x.ID == [16]byte(u) {
			return true
		}
	}
	return false
}

func recallHas(resp *mbp.ActivateResponse, id string) bool {
	for _, a := range resp.Activations {
		if a.ID == id {
			return true
		}
	}
	return false
}

// TestHNSWTombstone_HardDeletedNeverRecalledAcrossRestart: the guarantee that
// holds — recall never returns a hard-deleted memory, before or after a
// restart (the engram record is gone, so recall cannot resolve the id).
func TestHNSWTombstone_HardDeletedNeverRecalledAcrossRestart(t *testing.T) {
	fs := vfs.NewMem()
	env := openRCEnv(t, fs)
	target, vec := tombstoneFixture(t, env)
	if !recallHas(env.recallVec(vec, 10), target) {
		t.Fatal("setup: the target is not recalled by its own vector before the delete")
	}
	if _, err := env.eng.Forget(env.ctx, &mbp.ForgetRequest{ID: target, Hard: true, Vault: "default"}); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()
	if recallHas(env.recallVec(vec, 10), target) {
		t.Error("hard-deleted memory recalled before a restart")
	}
	env.stop()
	env2 := openRCEnv(t, fs)
	defer env2.stop()
	if recallHas(env2.recallVec(vec, 10), target) {
		t.Error("hard-deleted memory recalled after a restart")
	}
}

// TestHNSWTombstone_KnownResidual_HardDeletedVectorIsAGraphHitAfterRestart
// pins the residual (fails when fixed): after a restart the tombstone is gone
// and the hard-deleted vector is an ordinary graph node again, so the vector
// index returns it and it occupies one of the vector candidate slots recall
// takes — the user does not see it, but it can displace a live candidate at
// the vector cut. Fixing it means deleting the node's 0x07 keys on hard
// delete (or persisting tombstones).
func TestHNSWTombstone_KnownResidual_HardDeletedVectorIsAGraphHitAfterRestart(t *testing.T) {
	fs := vfs.NewMem()
	env := openRCEnv(t, fs)
	target, vec := tombstoneFixture(t, env)
	if _, err := env.eng.Forget(env.ctx, &mbp.ForgetRequest{ID: target, Hard: true, Vault: "default"}); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()
	if rawHNSWHas(t, env, vec, target) {
		t.Error("before a restart the tombstoned node is returned by the vector index")
	}
	env.stop()
	env2 := openRCEnv(t, fs)
	defer env2.stop()
	if !rawHNSWHas(t, env2, vec, target) {
		t.Error("KNOWN RESIDUAL FIXED? the hard-deleted vector is no longer a vector-index hit after a restart — update this pin and the decision record")
	}
}
