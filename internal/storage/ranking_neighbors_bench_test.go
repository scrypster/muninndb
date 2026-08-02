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
