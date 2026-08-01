package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
)

// Legacy key bytes derived INDEPENDENTLY of the encoder under repair: the
// pre-fix WeightComplement(1.0) overflowed to 0 and the complement MaxUint32-0
// is 0xFFFFFFFF. The post-fix 1.0 position is MaxUint32-MaxUint32 = 0.
var (
	engineLegacyComplement  = [4]byte{0xFF, 0xFF, 0xFF, 0xFF}
	engineCorrectComplement = [4]byte{0x00, 0x00, 0x00, 0x00}
)

func engineRawAssocKey(pfx byte, ws [8]byte, first [16]byte, complement [4]byte, second [16]byte) []byte {
	key := make([]byte, 45)
	key[0] = pfx
	copy(key[1:9], ws[:])
	copy(key[9:25], first[:])
	copy(key[25:29], complement[:])
	copy(key[29:45], second[:])
	return key
}

// seedLegacyFullWeightEdge writes the on-disk shape of a pre-fix weight-1.0
// edge directly through Pebble: 0x03/0x04 at the legacy complement, 0x14
// carrying the true 1.0. Direct writes are the only way to produce this state —
// the fixed encoder cannot.
func seedLegacyFullWeightEdge(t *testing.T, db *pebble.DB, ws [8]byte, src, dst [16]byte) {
	t.Helper()
	// 26-byte modern value with peakWeight 1.0 (the dominant, clamp-not-delete
	// case from the #756 correction).
	val := make([]byte, 26)
	binary.BigEndian.PutUint16(val[0:2], 1)
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(1.0))
	binary.BigEndian.PutUint64(val[6:14], uint64(time.Unix(1_700_000_000, 0).UnixNano()))
	binary.BigEndian.PutUint32(val[14:18], uint32(4242))
	binary.BigEndian.PutUint32(val[18:22], math.Float32bits(1.0))
	binary.BigEndian.PutUint32(val[22:26], 5)

	wiKey := make([]byte, 41)
	wiKey[0] = prefix.AssocWeightIndex
	copy(wiKey[1:9], ws[:])
	copy(wiKey[9:25], src[:])
	copy(wiKey[25:41], dst[:])
	var wi [4]byte
	binary.BigEndian.PutUint32(wi[:], math.Float32bits(1.0))

	batch := db.NewBatch()
	defer batch.Close()
	require.NoError(t, batch.Set(engineRawAssocKey(prefix.AssocFwd, ws, src, engineLegacyComplement, dst), val, nil))
	require.NoError(t, batch.Set(engineRawAssocKey(prefix.AssocRev, ws, dst, engineLegacyComplement, src), val, nil))
	require.NoError(t, batch.Set(wiKey, wi[:], nil))
	require.NoError(t, batch.Commit(pebble.NoSync))
}

func engineKeyPresent(t *testing.T, db *pebble.DB, key []byte) bool {
	t.Helper()
	_, closer, err := db.Get(key)
	if err == pebble.ErrNotFound {
		return false
	}
	require.NoError(t, err)
	_ = closer.Close()
	return true
}

// assocRepairTestEnv mirrors repairTestEnv but also hands back the raw
// *pebble.DB — the pre-fix on-disk layout can only be produced by writing keys
// directly, since the fixed encoder cannot place a 1.0 edge at the weight-0.0
// position any more.
func assocRepairTestEnv(t *testing.T) (*storage.PebbleStore, *pebble.DB, *fts.Index, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-assoc-weight-repair-test-*")
	require.NoError(t, err)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	return store, db, fts.New(db), func() {
		store.Close()
		os.RemoveAll(dir)
	}
}

func newAssocRepairEngine(store *storage.PebbleStore, ftsIdx *fts.Index, delay time.Duration) *Engine {
	embedder := &noopEmbedder{}
	long := time.Hour
	return NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx,
		ActivationEngine:       activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder),
		TriggerSystem:          trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder),
		Embedder:               embedder,
		EvolveRepairDelay:      &long, // keep the unrelated #681 pass out of the way
		AssocWeightRepairDelay: &delay,
	})
}

// TestLegacyFullWeightAssocRepair_EngineLifecycleAndWatermark drives the repair
// through the real NewEngine/Stop path rather than calling the pass directly —
// deleting the NewEngine spawn line, or breaking the delay/stop plumbing, turns
// this red while the storage-level tests stay green.
//
// Boot 1 has damage on disk and a short delay: the pass must relocate the edge
// and watermark the vault. Boot 2 faces NEW damage in the same vault: the
// watermark must make it skip, leaving that damage in place. That skip is the
// documented cost of not paying a full 0x03 scan on every boot — and it is
// sound here because the fixed encoder cannot produce new damage of this kind
// (the "new damage" below is hand-written, unreachable from any code path).
func TestLegacyFullWeightAssocRepair_EngineLifecycleAndWatermark(t *testing.T) {
	store, db, ftsIdx, cleanup := assocRepairTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := store.ResolveVaultPrefix("test")
	// The vault must be registered for ListVaults to surface it to the pass.
	require.NoError(t, store.WriteVaultName(ws, "test"))

	src1 := [16]byte{0x01, 0xAA}
	dst1 := [16]byte{0x02, 0xBB}
	seedLegacyFullWeightEdge(t, db, ws, src1, dst1)

	// Boot 1: the startup pass must relocate the edge and mark the vault.
	boot1 := newAssocRepairEngine(store, ftsIdx, time.Millisecond)
	select {
	case <-boot1.assocWeightRepairDone:
	case <-time.After(10 * time.Second):
		t.Fatal("repair pass did not complete within 10s of engine start")
	}
	require.False(t, engineKeyPresent(t, db, engineRawAssocKey(prefix.AssocFwd, ws, src1, engineLegacyComplement, dst1)),
		"boot-time repair must clear the legacy key position")
	require.True(t, engineKeyPresent(t, db, engineRawAssocKey(prefix.AssocFwd, ws, src1, engineCorrectComplement, dst1)),
		"boot-time repair must place the edge at the true 1.0 position")

	mark, err := store.GetAssocWeightRepairMark(ctx, ws)
	require.NoError(t, err)
	require.Equal(t, assocWeightRepairVersion, mark, "clean pass must watermark the vault")

	// New damage after the watermark (hand-written; unreachable in practice).
	src2 := [16]byte{0x03, 0xCC}
	dst2 := [16]byte{0x04, 0xDD}
	seedLegacyFullWeightEdge(t, db, ws, src2, dst2)
	boot1.Stop()

	// Boot 2: the watermark short-circuits the vault — the second scan never
	// happens, so the new damage survives untouched.
	boot2 := newAssocRepairEngine(store, ftsIdx, time.Millisecond)
	select {
	case <-boot2.assocWeightRepairDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watermarked pass did not complete within 10s of engine start")
	}
	require.True(t, engineKeyPresent(t, db, engineRawAssocKey(prefix.AssocFwd, ws, src2, engineLegacyComplement, dst2)),
		"watermarked vault must be skipped — a full 0x03 re-scan on every boot is what the watermark exists to prevent")
	boot2.Stop()
}

// The prune worker's assoc-decay must not run until the repair pass has
// finished CLEANLY: decay over an unrepaired vault permanently destroys the
// 0x14 evidence the repair depends on (#756 correction). The gate is what turns
// that from a race between two startup timers into an ordering.
func TestAssocWeightRepairGate_ClosedUntilCleanCompletion(t *testing.T) {
	store, _, ftsIdx, cleanup := assocRepairTestEnv(t)
	defer cleanup()

	// A pass parked behind a long delay: not complete, so decay must be gated.
	blocked := newAssocRepairEngine(store, ftsIdx, time.Hour)
	require.False(t, blocked.assocWeightRepairComplete(),
		"decay must be gated while the one-shot repair is still pending")
	blocked.Stop()
	require.False(t, blocked.assocWeightRepairComplete(),
		"a pass that never swept must not open the gate — shutdown is not a clean pass")

	// An Engine constructed without the pass (nil channel) must report complete,
	// or a direct-struct test engine would have decay gated off permanently.
	require.True(t, (&Engine{}).assocWeightRepairComplete())
}

// bareRepairEngine builds an Engine directly (no NewEngine, no background
// goroutines) so the repair pass and the decay gate can be driven
// deterministically, with no timers and nothing racing the test.
func bareRepairEngine(t *testing.T, store *storage.PebbleStore) *Engine {
	t.Helper()
	e := &Engine{
		store:                   store,
		assocWeightRepairDelay:  time.Millisecond,
		assocWeightRepairDone:   make(chan struct{}),
		assocWeightRepairExited: make(chan struct{}),
	}
	e.stopCtx, e.stopCancel = context.WithCancel(context.Background())
	t.Cleanup(e.stopCancel)
	return e
}

// B2 (review finding): the gate itself, tested directly with a counting double.
//
// The pre-review version inlined `if !decayReady { break }` in runPruneWorker's
// loop, and DELETING that line left the entire engine suite green — the gate was
// untested wiring. decayAllVaults exists so this assertion can be made in
// microseconds: zero DecayAssocWeights calls while the gate is shut, non-zero
// once it opens. No 60-second prune-timer test; the CI budget does not buy one.
//
// RED-verified: removing the gate check from decayAllVaults makes the first
// assertion fail.
func TestDecayAllVaults_GateSuppressesDecayCalls(t *testing.T) {
	store, _, _, cleanup := assocRepairTestEnv(t)
	defer cleanup()

	ws := store.ResolveVaultPrefix("vault-a")
	require.NoError(t, store.WriteVaultName(ws, "vault-a"))

	e := bareRepairEngine(t, store)
	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, float64, float32, float64) (int, error) {
		calls++
		return 0, nil
	}

	e.decayAllVaults(e.stopCtx, []string{"vault-a"})
	require.Zero(t, calls, "association decay ran while the full-weight repair gate was shut")

	close(e.assocWeightRepairDone) // what a clean pass does
	e.decayAllVaults(e.stopCtx, []string{"vault-a"})
	require.Positive(t, calls, "decay must resume once the repair has completed cleanly")
}

// B3 (review finding): the error path. A pass that could not repair a vault must
// leave the gate SHUT for the process lifetime rather than opening it, because
// decay over that vault destroys the 0x14 evidence permanently and
// unidentifiably. The goroutine must still exit and still close its exited
// channel, so Stop() cannot hang and the goroutine cannot leak.
//
// RED-verified: restoring `defer close(e.assocWeightRepairDone)` makes both the
// gate assertion and the zero-decay assertion fail.
func TestAssocWeightRepairGate_StaysShutWhenPassErrors(t *testing.T) {
	store, _, _, cleanup := assocRepairTestEnv(t)
	defer cleanup()

	ws := store.ResolveVaultPrefix("vault-b")
	require.NoError(t, store.WriteVaultName(ws, "vault-b"))

	e := bareRepairEngine(t, store)
	e.repairAssocWeightsFn = func(context.Context, [8]byte) (int, error) {
		return 0, errors.New("synthetic repair failure")
	}

	go e.runLegacyFullWeightAssocRepair()
	select {
	case <-e.assocWeightRepairExited:
	case <-time.After(10 * time.Second):
		t.Fatal("repair goroutine did not exit — Stop() would block on this")
	}

	require.False(t, e.assocWeightRepairComplete(),
		"a failed repair must leave association decay parked, not open the gate on its evidence")

	// The vault must NOT be watermarked: a failed pass must be retried next boot.
	mark, err := store.GetAssocWeightRepairMark(context.Background(), ws)
	require.NoError(t, err)
	require.Zero(t, mark, "a failed pass must not claim the vault is repaired")

	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, float64, float32, float64) (int, error) {
		calls++
		return 0, nil
	}
	e.decayAllVaults(e.stopCtx, []string{"vault-b"})
	require.Zero(t, calls, "decay ran over a vault whose repair failed — the 0x14 evidence is now gone")
}

// The mirror of the above: a clean pass DOES open the gate and DOES watermark.
// Without this, "stays shut on error" could be satisfied by a gate that never
// opens at all.
func TestAssocWeightRepairGate_OpensOnCleanPass(t *testing.T) {
	store, _, _, cleanup := assocRepairTestEnv(t)
	defer cleanup()

	ws := store.ResolveVaultPrefix("vault-c")
	require.NoError(t, store.WriteVaultName(ws, "vault-c"))

	e := bareRepairEngine(t, store)
	go e.runLegacyFullWeightAssocRepair()
	select {
	case <-e.assocWeightRepairExited:
	case <-time.After(10 * time.Second):
		t.Fatal("repair goroutine did not exit")
	}

	require.True(t, e.assocWeightRepairComplete(), "a clean pass must open the decay gate")
	mark, err := store.GetAssocWeightRepairMark(context.Background(), ws)
	require.NoError(t, err)
	require.Equal(t, assocWeightRepairVersion, mark)

	calls := 0
	e.decayAssocWeightsFn = func(context.Context, [8]byte, float64, float32, float64) (int, error) {
		calls++
		return 0, nil
	}
	e.decayAllVaults(e.stopCtx, []string{"vault-c"})
	require.Positive(t, calls)
}
