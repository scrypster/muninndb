package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func writeApiaryCorpus(t *testing.T, eng *Engine, vault string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	for _, c := range []string{
		"inspected the north hive and found capped brood on six frames",
		"replaced the queen excluder on the south hive before the flow",
		"fed one to one syrup to the nucleus colony after the cold snap",
	} {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "hive inspection", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "smoker fuel",
		Content: "the smokerwick pellets burn cool and long enough for a full inspection",
		Tags:    []string{"apiary-kit"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)
	return id
}

// TestRestore_ReadsCommittedStateUnderLock is item 4 for Engine.Restore: it
// read the memory (state check and every field it writes back), then wrote
// those fields through UpdateMetadata — so a write committed between the read
// and the write (here a confidence change) was reverted. The write is landed
// right after Restore's read: at the cache fill seam if Restore reads through
// the cache (the old code), at the read-modify-write seam if it reads under
// the engram's lock (the fix, where the concurrent write must wait).
func TestRestore_ReadsCommittedStateUnderLock(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-restore-lock"
	id := writeApiaryCorpus(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id.String()}); err != nil {
		t.Fatal(err)
	}
	eng.store.InvalidateAllCaches() // Restore's read must be a Pebble read

	var fired atomic.Bool
	var wg sync.WaitGroup
	concurrent := func() {
		if err := eng.store.UpdateConfidence(ctx, ws, id, 0.42); err != nil {
			t.Errorf("UpdateConfidence: %v", err)
		}
	}
	land := func(hws [8]byte, hid storage.ULID) {
		if hws != ws || hid != id || !fired.CompareAndSwap(false, true) {
			return
		}
		if !eng.store.EngramStripeHeldForTesting(id) {
			concurrent()
			return
		}
		wg.Add(1)
		go func() { defer wg.Done(); concurrent() }()
	}
	restoreFill := storage.SetCacheFillHookForTesting(land)
	restoreRMW := storage.SetRMWHookForTesting(func(_ string, hws [8]byte, hid storage.ULID) { land(hws, hid) })
	_, err := eng.Restore(ctx, vault, id.String())
	restoreFill()
	restoreRMW()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !fired.Load() {
		t.Fatal("fixture: no seam fired inside Restore")
	}

	got, err := eng.store.GetEngramUncached(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != storage.StateActive {
		t.Fatalf("state after restore = %v", got.State)
	}
	if got.Confidence != 0.42 {
		t.Fatalf("confidence = %v, want 0.42: Restore wrote back a value it read before a committed confidence change", got.Confidence)
	}
}
