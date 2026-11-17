package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestVaultCount_HardForgetThenWriteSurvivesRestart: a sweep saved the count,
// a hard forget wrote count-1 straight to disk, and one more write brought the
// count back to the value the sweep had saved. A coalescer that skipped
// entries whose count equalled the last value it saved never wrote that
// count, so every restart loaded one fewer than the vault held, and the loss
// compounded per cycle (3/4, 3/5, 3/6 over three cycles). Sweeps are forced
// with FlushCounters, the deterministic stand-in for the 100 ms ticker.
func TestVaultCount_HardForgetThenWriteSurvivesRestart(t *testing.T) {
	c := newCFEnv(t)
	ctx := context.Background()
	const vault = "apiary-forget-then-write"
	ws := c.store.ResolveVaultPrefix(vault)
	write := func(i int) string {
		t.Helper()
		resp, err := c.eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: fmt.Sprintf("hive check %d", i), Content: fmt.Sprintf("hive %d had capped brood and stores", i)})
		if err != nil {
			t.Fatal(err)
		}
		return resp.ID
	}
	live := func() int64 {
		t.Helper()
		ids, err := c.store.EngramIDsByCreatedRange(ctx, ws, time.Unix(0, 0), time.Now().Add(48*time.Hour), 100000)
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(ids))
	}
	i := 0
	for ; i < 3; i++ {
		write(i)
	}
	for cycle := 1; cycle <= 3; cycle++ {
		id := write(i)
		i++
		c.store.FlushCounters()
		if _, err := c.eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id, Hard: true}); err != nil {
			t.Fatal(err)
		}
		write(i)
		i++
		c.store.FlushCounters()
		c.restart()
		ws = c.store.ResolveVaultPrefix(vault)
		if got, want := c.store.GetVaultCount(ctx, ws), live(); got != want {
			t.Fatalf("cycle %d: after restart the vault count is %d, the vault holds %d memories", cycle, got, want)
		}
	}
}
