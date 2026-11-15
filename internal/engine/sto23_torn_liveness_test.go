package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestSTO23_TornMetadataNeverHidesALiveMemory pins STO-23: the retired set,
// recall's final filter and Read agree on liveness for every memory. The
// memory's 0x02 metadata record is deliberately torn to "soft-deleted" while
// its 0x01 engram record stays active — the shape a clone copied across a
// concurrent forget left on disk. Read calls it live, so recall must return
// it, before and after a restart.
func TestSTO23_TornMetadataNeverHidesALiveMemory(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	eng, _, store, stop := newEngineOverDB(t, db)
	ctx := context.Background()
	const vault = "trail-crew"
	for i := 0; i < 20; i++ {
		r, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault,
			Concept: fmt.Sprintf("switchback drain %d", i),
			Content: fmt.Sprintf("cleared the switchback drain culvert %d after the storm", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: r.ID}); err != nil {
			t.Fatal(err)
		}
	}
	x, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "switchback drain X",
		Content: "the switchback drain culvert on the upper loop is still blocked"})
	if err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)
	ws := store.ResolveVaultPrefix(vault)
	xid, _ := storage.ParseULID(x.ID)

	mk := keys.MetaKey(ws, [16]byte(xid))
	v, closer, err := db.Get(mk)
	if err != nil {
		t.Fatal(err)
	}
	torn := append([]byte(nil), v...)
	closer.Close()
	torn[erf.OffsetState] = byte(storage.StateSoftDeleted)
	if err := db.Set(mk, torn, nil); err != nil {
		t.Fatal(err)
	}
	store.ResetAllRetired() // the next recall builds the set from the torn store

	check := func(e *Engine, when string) {
		got, err := e.store.GetEngram(ctx, ws, xid)
		if err != nil || got.State != storage.StateActive {
			t.Fatalf("%s: fixture: Read must call X live (0x01 active): %v %v", when, got, err)
		}
		resp, err := e.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"switchback drain culvert blocked upper loop"}, MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range resp.Activations {
			if a.ID == x.ID {
				return
			}
		}
		t.Fatalf("%s: recall hid a memory Read calls live (torn 0x02 said soft-deleted); returned %d results", when, len(resp.Activations))
	}
	check(eng, "before restart")
	stop()

	db2, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	eng2, _, _, stop2 := newEngineOverDB(t, db2)
	defer stop2()
	check(eng2, "after restart")
}
