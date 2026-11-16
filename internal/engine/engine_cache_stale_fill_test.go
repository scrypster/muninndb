package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestForgetDuringRecall_ForgetSticksAndRestoreWorks is reproduction 3 end to
// end: a forget commits while a recall is between loading a memory from its
// pinned Pebble snapshot and filling the shared cache with it. Before the
// fix the recall re-cached the PRE-forget copy, so the forgotten memory read
// as active afterwards and Restore refused it ("not soft-deleted").
//
// The interleaving is driven by the storage fill seam: the forget runs at the
// exact point between the recall's load and its fill, not on a timer.
func TestForgetDuringRecall_ForgetSticksAndRestoreWorks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-log"

	for i := 0; i < 4; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("hive %d inspection", i),
			Content: fmt.Sprintf("queen cells found on frame %d during the spring inspection", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	target, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "swarm trap relocated",
		Content: "moved the swarm trap to the east fence line before the spring inspection",
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)

	ws := eng.store.ResolveVaultPrefix(vault)
	tid, err := storage.ParseULID(target.ID)
	if err != nil {
		t.Fatal(err)
	}

	var fired atomic.Bool
	restoreHook := storage.SetCacheFillHookForTesting(func(hws [8]byte, hid storage.ULID) {
		if hws != ws || hid != tid || !fired.CompareAndSwap(false, true) {
			return
		}
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: target.ID}); err != nil {
			t.Errorf("Forget: %v", err)
		}
	})
	defer restoreHook()

	if _, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"swarm trap spring inspection"},
		MaxResults: 10,
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	restoreHook()
	if !fired.Load() {
		t.Fatal("fixture: the recall never filled the cache for the target, so the forget was never interleaved")
	}

	got, err := eng.store.GetEngram(ctx, ws, tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != storage.StateSoftDeleted {
		t.Fatalf("forget did not stick: the memory reads as %v after a committed forget", got.State)
	}

	restored, err := eng.Restore(ctx, vault, target.ID)
	if err != nil {
		t.Fatalf("restore refused a forgotten memory: %v", err)
	}
	if restored.State != storage.StateActive {
		t.Fatalf("restore returned state %v", restored.State)
	}
	again, err := eng.store.GetEngram(ctx, ws, tid)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != storage.StateActive {
		t.Fatalf("after restore the memory reads as %v", again.State)
	}
}
