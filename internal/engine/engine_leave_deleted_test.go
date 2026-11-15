package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestStateTransition_CannotUndeleteAroundRestore: a soft-deleted memory has
// no keyword postings, and only Restore puts them back. UpdateLifecycleState
// (muninn_state) and CompareAndSet (muninn_compare_and_set) went straight to
// the store and happily moved a forgotten memory to active or archived — it
// came back live (or archived, one un-archive from live) with no postings,
// unfindable by keyword. There is now exactly one way out of the
// soft-deleted state, Restore; a state transition out of it is refused with
// an error that names muninn_restore, and changes nothing.
func TestStateTransition_CannotUndeleteAroundRestore(t *testing.T) {
	for _, target := range []string{"active", "archived", "completed"} {
		for _, via := range []string{"UpdateLifecycleState", "CompareAndSet"} {
			t.Run(via+"->"+target, func(t *testing.T) {
				eng, cleanup := testEnv(t)
				defer cleanup()
				ctx := context.Background()
				vault := "apiary-state-" + target
				id := writeApiaryCorpus(t, eng, vault)
				ws := eng.store.ResolveVaultPrefix(vault)

				if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id.String()}); err != nil {
					t.Fatal(err)
				}
				var err error
				if via == "UpdateLifecycleState" {
					err = eng.UpdateLifecycleState(ctx, vault, id.String(), target)
				} else {
					tg := target
					_, err = eng.CompareAndSet(ctx, vault, id.String(), nil, &tg)
				}
				if !errors.Is(err, storage.ErrRestoreRequired) {
					t.Fatalf("%s(%s) on a forgotten memory: err = %v, want ErrRestoreRequired", via, target, err)
				}
				got, gerr := eng.store.GetEngramUncached(ctx, ws, id)
				if gerr != nil {
					t.Fatal(gerr)
				}
				if got.State != storage.StateSoftDeleted {
					t.Fatalf("a refused transition changed the state to %v", got.State)
				}
				if _, err := eng.Restore(ctx, vault, id.String()); err != nil {
					t.Fatal(err)
				}
				if !recallFinds(t, eng, vault, "smokerwick", id) {
					t.Fatal("after the refused transition and a restore, keyword recall does not find the memory")
				}
			})
		}
	}
}

// TestLifecycle_RandomizedEveryLiveMemoryIsKeywordFindable: forget, restore,
// state transitions, compare-and-set and retags race on a few memories; at
// quiescence every memory that is live is found by keyword for its own word,
// and every forgotten one is not. Short on purpose (engine CI budget).
func TestLifecycle_RandomizedEveryLiveMemoryIsKeywordFindable(t *testing.T) {
	const nMem, nOps = 4, 40
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)

	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-lifecycle-stress"
	words := []string{"quillcomb", "propolith", "waxmoth", "droneflue"}
	ids := make([]storage.ULID, nMem)
	for i := range ids {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "hive log " + words[i],
			Content: fmt.Sprintf("the %s note from the spring inspection", words[i])})
		if err != nil {
			t.Fatal(err)
		}
		ids[i], _ = storage.ParseULID(resp.ID)
	}
	awaitFTS(t, eng)
	ws := eng.store.ResolveVaultPrefix(vault)

	ops := []func(r *rand.Rand, i int) error{
		func(r *rand.Rand, i int) error {
			_, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: ids[i].String()})
			return err
		},
		func(r *rand.Rand, i int) error {
			_, err := eng.Restore(ctx, vault, ids[i].String())
			if err != nil && !isNotRestorable(err) {
				return err
			}
			return nil
		},
		func(r *rand.Rand, i int) error {
			st := []string{"active", "archived", "paused", "completed"}[r.Intn(4)]
			err := eng.UpdateLifecycleState(ctx, vault, ids[i].String(), st)
			if errors.Is(err, storage.ErrRestoreRequired) {
				return nil
			}
			return err
		},
		func(r *rand.Rand, i int) error {
			st := []string{"active", "archived"}[r.Intn(2)]
			_, err := eng.CompareAndSet(ctx, vault, ids[i].String(), nil, &st)
			if errors.Is(err, storage.ErrRestoreRequired) {
				return nil
			}
			return err
		},
		func(r *rand.Rand, i int) error {
			return eng.UpdateTags(ctx, vault, ids[i], []string{fmt.Sprintf("frame-%d", r.Intn(5))})
		},
	}
	// Weighted: a live retag reindexes the whole document and so HEALS a
	// postings-less live memory; it is kept rare so it cannot mask the bug.
	weights := []int{3, 2, 3, 2, 1}
	var pick []int
	for g, w := range weights {
		for k := 0; k < w; k++ {
			pick = append(pick, g)
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed + int64(g)))
			for k := 0; k < nOps; k++ {
				op := pick[r.Intn(len(pick))]
				if err := ops[op](r, r.Intn(nMem)); err != nil {
					t.Errorf("op %d: %v", op, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	for i, id := range ids {
		got, err := eng.store.GetEngramUncached(ctx, ws, id)
		if err != nil {
			t.Fatal(err)
		}
		score := ftsScoreOrZero(t, eng, ws, words[i], id)
		switch got.State {
		case storage.StateSoftDeleted:
			if score != 0 {
				t.Errorf("memory %d is forgotten but keyword search still finds it (score %v)", i, score)
			}
		default: // live, or archived (archival keeps postings)
			if score == 0 {
				t.Errorf("memory %d is %v but keyword search cannot find it by its own word", i, got.State)
			}
		}
	}
}

func isNotRestorable(err error) bool {
	return strings.Contains(err.Error(), "is not soft-deleted or archived")
}
