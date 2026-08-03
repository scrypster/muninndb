package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// #760 — the prune worker had no notion of leadership: runPruneWorker ran
// unconditionally on every cluster node, so a Lobe (follower) independently
// decayed its own copy of every association weight on its own 60s cadence.
// The decay math (COG-28) is a pure recompute from stored peakWeight and
// lastActivated, so a leader and a follower evaluating it a few seconds apart
// land on different float32 weights — and the on-disk association key
// ENCODES the weight (keys.AssocFwdKey(ws, src, weight, dst)). Two different
// computed weights for the same edge are therefore two different LIVE keys,
// not one edge updated twice: the follower's independent pass leaves a stale
// duplicate every tick, on top of whatever the leader's decay replicates in.
//
// The fix mirrors the leader-only ticket already used for the periodic
// replication-log prune (ClusterCoordinator.startPeriodicPrune: "if
// !c.IsLeader() { continue }") — reusing the engine's existing write gate
// (e.writeGate, installed by SetWriteGate, #596) rather than inventing a
// second leadership signal.
//
// TestDecayAllVaults_FollowerSkipsDecay is the reproduction: a follower gate
// installed, decayAllVaults called, and the store-level decay function must
// never run.
func TestDecayAllVaults_FollowerSkipsDecay(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "follower-decay-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocHalfLifeDays: f32(30)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	e := decayConfigEngine(t, store, as)
	e.SetWriteGate(followerGate) // this node is a Lobe, not the Cortex

	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
		calls++
		return 0, nil
	}

	e.decayAllVaults(e.stopCtx, []string{vaultName})

	if calls != 0 {
		t.Fatalf("decayAssocWeightsFn called %d times on a follower, want 0 — "+
			"a role-blind decay pass on a Lobe writes weight-keyed duplicates "+
			"that its own leader's replicated decay never converges away (#760)", calls)
	}
}

// TestDecayAllVaults_LeaderStillDecays proves the gate is leader-specific: a
// node that IS the Cortex (or a standalone, gate-less server) must be
// completely unaffected by the #760 fix.
func TestDecayAllVaults_LeaderStillDecays(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "leader-decay-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocHalfLifeDays: f32(30)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	e := decayConfigEngine(t, store, as)
	e.SetWriteGate(func() error { return nil }) // this node IS the Cortex

	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
		calls++
		return 0, nil
	}

	e.decayAllVaults(e.stopCtx, []string{vaultName})

	if calls != 1 {
		t.Fatalf("decayAssocWeightsFn called %d times on the leader, want 1", calls)
	}
}

// TestDecayAllVaults_NoGateMeansSingleNode: a standalone (non-cluster) server
// installs no write gate at all. Decay must run exactly as it always has —
// the #760 fix is a cluster-topology check, not a new default-off switch.
func TestDecayAllVaults_NoGateMeansSingleNode(t *testing.T) {
	_, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "standalone-decay-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{AssocHalfLifeDays: f32(30)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	e := decayConfigEngine(t, store, as)

	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
		calls++
		return 0, nil
	}

	e.decayAllVaults(e.stopCtx, []string{vaultName})

	if calls != 1 {
		t.Fatalf("decayAssocWeightsFn called %d times with no write gate installed, want 1", calls)
	}
}
