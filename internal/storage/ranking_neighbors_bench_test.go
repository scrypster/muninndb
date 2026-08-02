package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func openBenchStore(b *testing.B) *PebbleStore {
	b.Helper()
	dir, err := os.MkdirTemp("", "muninndb-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	b.Cleanup(func() { store.Close(); os.RemoveAll(dir) })
	return store
}

// BenchmarkPhase4Read compares the forward-only read against the COG-31 union
// in the shape phase4HebbianBoost uses it: 50 candidate ids out of a
// 200-engram vault, maxPerNode 20, repeated calls (so both caches are warm,
// which is the production steady state).
//
// It exists because the union's cost was mis-modelled once already. The design
// assumed "one extra bounded iterator, like the forward one" and left the
// reverse half UNCACHED — but the forward half is served from assocCache, so
// the reverse half was paying ~50 fresh Pebble seeks on every recall. Measured
// here at 10 edges/node: 11us forward-only vs 152us union, which pushed
// whole-recall p50 15-20% over the pre-committed budget. With revAssocCache
// and a map-free merge the union is ~41us. Keep this benchmark: it is the only
// thing that would catch that regression returning.
//
// Not in the CI gate (benchmarks do not run under `go test`).
func BenchmarkPhase4Read(b *testing.B) {
	for _, e := range []int{0, 2, 10} {
		store := openBenchStore(b)
		ctx := context.Background()
		ws := store.VaultPrefix("bench-rn")
		ids := make([]ULID, 200)
		for i := range ids {
			ids[i] = NewULID()
		}
		for i := range ids {
			for j := 1; j <= e; j++ {
				_ = store.WriteAssociation(ctx, ws, ids[i], ids[(i+j)%len(ids)], &Association{
					TargetID: ids[(i+j)%len(ids)], Weight: 0.5, RelType: RelRelatesTo,
				})
			}
		}
		cand := ids[:50]
		b.Run(fmt.Sprintf("edges%d/GetAssociations", e), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, _ = store.GetAssociations(ctx, ws, cand, 20)
			}
		})
		b.Run(fmt.Sprintf("edges%d/GetRankingNeighbors", e), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, _ = store.GetRankingNeighbors(ctx, ws, cand, 20)
			}
		})
	}
}

// buildForwardOnlyFan populates a vault with nCand candidate engrams, each
// holding `degree` forward edges, and returns the vault prefix and the
// candidate ids.
//
// The edges point at SINK engrams that are NEVER themselves candidates. That
// is the whole point of the fixture and it is easy to get wrong: a ring (or
// any fixture where candidates point at each other) gives every candidate
// inbound edges too, so len(rev) > 0 and mergeRankingNeighbors never reaches
// its len(rev) == 0 shortcut. Nothing points AT a candidate here, so the
// shortcut — and the copy taken there — is what the benchmark measures.
//
// The shape is asserted, not assumed, by
// TestForwardOnlyFanFixture_TakesTheMergeCopyShortcut.
func buildForwardOnlyFan(tb testing.TB, store *PebbleStore, vault string, nCand, degree int) ([8]byte, []ULID) {
	tb.Helper()
	ctx := context.Background()
	ws := store.VaultPrefix(vault)

	cand := make([]ULID, nCand)
	for i := range cand {
		cand[i] = NewULID()
	}
	sinks := make([]ULID, degree)
	for i := range sinks {
		sinks[i] = NewULID()
	}

	// #803/STO-12: every endpoint needs a live 0x01 record before any edge
	// between them is accepted. Done here, before the write loop, so the
	// benchmark's timed region is unchanged.
	seedEndpoints(tb, store, ws, cand...)
	seedEndpoints(tb, store, ws, sinks...)

	for i := range cand {
		for j := 0; j < degree; j++ {
			// Distinct, descending weights so the merge's two-pointer walk and
			// the cap see a realistic ordering rather than a flat tie.
			w := float32(degree-j) / float32(degree+1)
			if err := store.WriteAssociation(ctx, ws, cand[i], sinks[j], &Association{
				TargetID: sinks[j], Weight: w, RelType: RelRelatesTo,
			}); err != nil {
				tb.Fatalf("WriteAssociation: %v", err)
			}
		}
	}
	return ws, cand
}

// BenchmarkPhase4Read_ForwardOnlyFan measures the arm that BenchmarkPhase4Read
// does not have: 50 candidates that hold forward edges and NO inbound ones.
//
// This is the only shape in which mergeRankingNeighbors' len(rev) == 0
// shortcut runs with a non-empty list, so it is the only shape that pays for
// the copy taken there (afacf8b, COG-31). BenchmarkPhase4Read's arms both miss
// it: at edges > 0 it builds a ring, so every node has inbound edges and
// len(rev) > 0; at edges == 0 no node has any edge, so the shortcut returns
// nil without copying. A figure quoted from either arm is measuring machine
// noise, which is how ~1us came to be recorded for a cost that is ~4-13us.
//
// Degrees 2/10/20 bracket the maxPerNode 20 cap phase4HebbianBoost uses, and
// the allocation counters are the load-bearing half: the copy's cost scales
// with the number of edges returned, so it is largest exactly at the cap.
//
// Not in the CI gate (benchmarks do not run under `go test`).
func BenchmarkPhase4Read_ForwardOnlyFan(b *testing.B) {
	for _, degree := range []int{2, 10, 20} {
		store := openBenchStore(b)
		ctx := context.Background()
		ws, cand := buildForwardOnlyFan(b, store, fmt.Sprintf("bench-fan-%d", degree), 50, degree)

		b.Run(fmt.Sprintf("fanOut%d/GetAssociations", degree), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = store.GetAssociations(ctx, ws, cand, 20)
			}
		})
		b.Run(fmt.Sprintf("fanOut%d/GetRankingNeighbors", degree), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = store.GetRankingNeighbors(ctx, ws, cand, 20)
			}
		})
	}
}

// BenchmarkRankingReverseEdges_DirectionalInbound measures the cost of one
// COLD GetRankingNeighbors call for a HUB engram whose inbound edges are
// DIRECTIONAL — the shape a project or spec node acquires when every memory
// points at it with RelBelongsToProject / RelReferences.
//
// It exists to record the scaling cliff that revAssocScanCap does NOT bound.
// The cap counts ACCEPTED edges; an edge failing BidirectionalForRanking is
// skipped without consuming a slot, so a directional hub is scanned in full
// and the work is O(inbound degree), not O(cap). Zero edges are returned for
// that cost.
//
// The symmetric arm is the control: those edges DO consume cap slots, so it
// stays flat as degree grows. coldFwdOnly is the pre-COG-31 baseline.
//
// Not in the CI gate (benchmarks do not run under `go test`).
func BenchmarkRankingReverseEdges_DirectionalInbound(b *testing.B) {
	for _, n := range []int{0, 1000, 5000} {
		for _, arm := range []struct {
			name string
			rel  RelType
		}{{"directional", RelBelongsToProject}, {"symmetric", RelCoActivated}} {
			store := openBenchStore(b)
			ctx := context.Background()
			ws := store.VaultPrefix("bench-hub")
			hub := NewULID()
			for i := 0; i < n; i++ {
				src := NewULID()
				_ = store.WriteAssociation(ctx, ws, src, hub, &Association{
					TargetID: hub, Weight: float32(i%100) / 100.0, RelType: arm.rel,
				})
			}
			ids := []ULID{hub}
			b.Run(fmt.Sprintf("%sInbound%d/cold", arm.name, n), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					store.revAssocCache.Purge()
					store.assocCache.Purge()
					b.StartTimer()
					_, _ = store.GetRankingNeighbors(ctx, ws, ids, 20)
				}
			})
			if arm.name == "directional" {
				b.Run(fmt.Sprintf("%sInbound%d/coldFwdOnly", arm.name, n), func(b *testing.B) {
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						store.assocCache.Purge()
						b.StartTimer()
						_, _ = store.GetAssociations(ctx, ws, ids, 20)
					}
				})
			}
		}
	}
}
