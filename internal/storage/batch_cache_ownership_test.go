package storage

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// TestMutateEngram_DoesNotMutateCachedPointer is the behavioural pin for #858.
//
// mutateEngram used to read the engram with ps.GetEngram — which returns the
// SHARED L1-cache pointer, under a documented immutability contract (see
// L1Cache.Get) — and then apply its mutation to that struct in place. Two
// concurrent evolves of the same id therefore wrote and read one struct, and a
// concurrent recall reading eng.State/eng.ValidUntil saw a half-applied write.
//
// This test does not need the race detector to fail. It holds the cached
// pointer exactly as a concurrent recall would, runs a batch mutation, and
// asserts the held pointer is untouched while the PERSISTED record carries the
// change. That is the whole contract: mutators copy, the cache stays read-only.
func TestMutateEngram_DoesNotMutateCachedPointer(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("cache-ownership")

	eng := &Engram{Concept: "Wren", Content: "a nesting note"}
	if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Warm the L1 cache and hold the pointer, as a concurrent recall does.
	held, err := store.GetEngram(ctx, ws, eng.ID)
	if err != nil {
		t.Fatalf("GetEngram (warm): %v", err)
	}
	if held.State != StateActive {
		t.Fatalf("precondition: state = %v, want StateActive", held.State)
	}
	beforeState := held.State
	beforeUpdated := held.UpdatedAt
	beforeValidUntil := held.ValidUntil

	validUntil := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	b := store.NewBatch()
	if err := b.SupersedeEngram(ctx, ws, eng.ID, validUntil); err != nil {
		b.Discard()
		t.Fatalf("SupersedeEngram: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The pointer a concurrent reader is holding must be exactly as it was.
	if held.State != beforeState {
		t.Errorf("held cache pointer State = %v, want %v — mutateEngram wrote through the "+
			"shared L1-cache pointer (#858). It must deep-copy before mutating; L1Cache.Get "+
			"documents the returned struct as read-only.", held.State, beforeState)
	}
	if !held.UpdatedAt.Equal(beforeUpdated) {
		t.Errorf("held cache pointer UpdatedAt = %v, want %v — mutateEngram stamped "+
			"UpdatedAt on the shared struct (#858)", held.UpdatedAt, beforeUpdated)
	}
	if !held.ValidUntil.Equal(beforeValidUntil) {
		t.Errorf("held cache pointer ValidUntil = %v, want %v — mutateEngram stamped "+
			"ValidUntil on the shared struct (#858)", held.ValidUntil, beforeValidUntil)
	}

	// ...and the change must still have been PERSISTED. A "fix" that stopped
	// mutating by not writing anything would pass the assertions above.
	got, err := store.GetEngram(ctx, ws, eng.ID)
	if err != nil {
		t.Fatalf("GetEngram (after): %v", err)
	}
	if got.State != StateSoftDeleted {
		t.Errorf("persisted State = %v, want StateSoftDeleted — the supersede did not land", got.State)
	}
	if got.ValidUntil.IsZero() {
		t.Error("persisted ValidUntil is zero — the supersede stamp did not land")
	}
}

// TestMutateEngram_ConcurrentSupersedeSameID is the -race reproduction from
// #858, reduced to the storage layer: the engine-level version was 2 goroutines
// x 50 EvolveAt on one id, and every layer above mutateEngram was incidental.
//
// Without the fix this reports a WARNING: DATA RACE — a write at
// batch.go's `eng.State = StateSoftDeleted` against the `oldState := eng.State`
// read in mutateEngram, both on the one struct GetEngram hands out.
//
// The cache is deliberately pre-warmed and the engram is never evicted, so both
// goroutines are guaranteed to receive the SAME pointer. A cold cache would
// give each its own decode and the race would not reproduce — which is why this
// is not simply "run two evolves".
func TestMutateEngram_ConcurrentSupersedeSameID(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("cache-ownership-race")

	eng := &Engram{Concept: "Bramble", Content: "contended record"}
	if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	if _, err := store.GetEngram(ctx, ws, eng.ID); err != nil {
		t.Fatalf("GetEngram (warm): %v", err)
	}

	const goroutines, iterations = 2, 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				b := store.NewBatch()
				if err := b.SupersedeEngram(ctx, ws, eng.ID, time.Now()); err != nil {
					b.Discard()
					continue
				}
				_ = b.Commit()
				// Re-warm: Commit invalidates the entry, and the race needs a
				// shared cached pointer to exist.
				_, _ = store.GetEngram(ctx, ws, eng.ID)
			}
		}()
	}
	close(start)
	wg.Wait()
}

// TestReadModifyWriteWriters_DoNotMutateTheCachedPointer is the behavioural
// half of STO-20, driven over rmwWriters — the SAME enumeration
// TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal uses.
//
// Reusing that table rather than writing a fresh list is deliberate. It was
// assembled by walking every erf.Encode/EncodeV2 call site in the package, it
// already carries the argument that there is no eighth writer, and it is
// maintained: a new read-modify-write writer added there inherits this check
// for free, which is exactly the direction #858 needed and did not have. The
// AST census (TestCachedEngramMutationCensus) catches the shape statically;
// this catches the behaviour, and the two fail for different reasons — a writer
// that reaches the cached struct through a helper the census cannot follow
// still fails here.
//
// # It only catches THREE of the seven, and that is not a defect to fix here
//
// Measured, by removing every Clone and re-running: only UpdateDigest and the
// two mutateEngram arms fail. SoftDelete, UpdateTagsLocked, UpdateConfidence
// and UpdateConfidenceWithContradiction all `cache.Delete` BEFORE their
// GetEngram, so the pointer this test is holding has been evicted and is not
// the object they mutate — they are unsafe for a DIFFERENT reader, the one that
// reads after their GetEngram re-caches. Do not read a green here as covering
// them; TestReadModifyWriteWriters_DoNotRaceAConcurrentReader is what does, and
// it goes red on all seven.
//
// Method: pre-warm the L1 cache, hold the pointer exactly as a concurrent
// recall does, snapshot it BY VALUE, run the writer, and require the held
// struct to be byte-identical afterwards. reflect.DeepEqual over the value
// snapshot catches field writes AND element writes into shared slices, which is
// why the snapshot is the whole struct rather than the two or three fields each
// writer happens to touch today.
func TestReadModifyWriteWriters_DoNotMutateTheCachedPointer(t *testing.T) {
	for _, w := range rmwWriters {
		t.Run(w.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			ws := store.VaultPrefix("rmw-cache-ownership")

			eng := &Engram{
				Concept:   "Sorrel",
				Content:   "a record several writers will touch",
				Tags:      []string{"alpha", "beta"},
				KeyPoints: []string{"first point"},
			}
			if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
				t.Fatalf("WriteEngram: %v", err)
			}

			// Warm the cache and hold the pointer a recall would be holding.
			held, err := store.GetEngram(ctx, ws, eng.ID)
			if err != nil {
				t.Fatalf("GetEngram (warm): %v", err)
			}
			before := *held // value snapshot, taken before the writer runs

			if err := w.apply(t, ctx, store, ws, eng.ID); err != nil {
				t.Fatalf("%s: %v", w.name, err)
			}

			if !reflect.DeepEqual(before, *held) {
				t.Errorf("%s mutated the shared L1-cache pointer in place (#858, STO-20).\n"+
					"  held before: %+v\n"+
					"  held after:  %+v\n"+
					"GetEngram returns the cache's own struct, documented read-only at L1Cache.Get. "+
					"Take a private copy first — `eng = eng.Clone()` — and mutate that. A per-engram "+
					"stripe lock does NOT make this safe: recall's readers do not take casLocks.",
					w.name, before, *held)
			}
		})
	}
}

// TestReadModifyWriteWriters_DoNotRaceAConcurrentReader is the OTHER half of
// STO-20's behavioural coverage, and it exists because the held-pointer test
// above is structurally blind to four of the seven writers.
//
// SoftDelete, UpdateTagsLocked, UpdateConfidence and
// UpdateConfidenceWithContradiction all call `ps.cache.Delete` BEFORE their
// GetEngram, to make that read authoritative against a racing DeleteEngram. A
// caller who grabbed the pointer beforehand is therefore holding an evicted
// object the writer never touches, and the held-pointer test passes on all four
// even with their Clone removed — verified, not assumed.
//
// That eviction does not make them safe, because GetEngram RE-CACHES what it
// decodes: the struct the writer then mutates is, from the instant GetEngram
// returns, the cache's own entry and reachable by any reader. Recall's readers
// take no lock (`engram.go` says so at UpdateTagsLocked's re-cache), so the
// stripe lock those four hold does not serialise them against a recall.
//
// This test therefore models the reader that actually loses: one that reads
// AFTER the writer's own GetEngram. It asserts nothing about values — the race
// detector is the assertion, and there is no sleep and no wall-clock deadline
// anywhere in it.
func TestReadModifyWriteWriters_DoNotRaceAConcurrentReader(t *testing.T) {
	for _, w := range rmwWriters {
		t.Run(w.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			ws := store.VaultPrefix("rmw-reader-race")

			eng := &Engram{
				Concept:   "Larkspur",
				Content:   "a record read while it is written",
				Tags:      []string{"alpha", "beta"},
				KeyPoints: []string{"first point"},
			}
			if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
				t.Fatalf("WriteEngram: %v", err)
			}

			var wg sync.WaitGroup
			stop := make(chan struct{})
			wg.Add(1)
			go func() { // exactly what a concurrent recall does: GetEngram, read fields
				defer wg.Done()
				var sink int
				for {
					select {
					case <-stop:
						_ = sink
						return
					default:
					}
					g, err := store.GetEngram(ctx, ws, eng.ID)
					if err != nil || g == nil {
						continue
					}
					sink += int(g.State) + int(g.Confidence*100) + len(g.Tags) +
						len(g.Summary) + len(g.KeyPoints) + int(g.UpdatedAt.UnixNano()&1)
				}
			}()

			for i := 0; i < 150; i++ {
				if err := w.apply(t, ctx, store, ws, eng.ID); err != nil {
					close(stop)
					wg.Wait()
					t.Fatalf("%s: %v", w.name, err)
				}
			}
			close(stop)
			wg.Wait()
		})
	}
}
