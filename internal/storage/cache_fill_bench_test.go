package storage

import (
	"context"
	"fmt"
	"testing"
)

// Hot-read-path benchmarks for the stale-fill guard: GetEngram hit/miss and
// a 50-id GetEngrams batch all-hit / all-miss / mixed, the batch under a
// pinned snapshot the way recall's phase 6 calls it. The miss variants drop
// the entries with cache.Delete inside the timed loop; that cost is the same
// before and after the guard.

func benchStoreWithEngrams(b *testing.B, n int) (*PebbleStore, [8]byte, []ULID) {
	b.Helper()
	s := openTestStore(b)
	// Large enough that nothing is evicted during the benchmark.
	s.cache = NewL1Cache(10_000)
	ws := s.VaultPrefix("bench-cache-fill")
	ids := make([]ULID, n)
	for i := range ids {
		id, err := s.WriteEngram(context.Background(), ws, &Engram{
			Concept:    fmt.Sprintf("signal box lever %d", i),
			Content:    "regreased the interlocking frame and checked the point rodding runs",
			Confidence: 0.8,
			Stability:  30,
		})
		if err != nil {
			b.Fatal(err)
		}
		ids[i] = id
	}
	return s, ws, ids
}

func BenchmarkCacheFill_GetEngramHit(b *testing.B) {
	s, ws, ids := benchStoreWithEngrams(b, 1)
	ctx := context.Background()
	if _, err := s.GetEngram(ctx, ws, ids[0]); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetEngram(ctx, ws, ids[0]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheFill_GetEngramMiss(b *testing.B) {
	s, ws, ids := benchStoreWithEngrams(b, 1)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.cache.Delete(ws, ids[0])
		if _, err := s.GetEngram(ctx, ws, ids[0]); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGetEngrams50(b *testing.B, warmEvery int) {
	s, ws, ids := benchStoreWithEngrams(b, 50)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for j, id := range ids {
			if warmEvery == 0 || j%warmEvery != 0 {
				s.cache.Delete(ws, id)
			} else if _, ok := s.cache.GetNoStamp(ws, id); !ok {
				if _, err := s.GetEngram(ctx, ws, id); err != nil {
					b.Fatal(err)
				}
			}
		}
		sctx, snap := s.NewSnapshotContext(ctx)
		b.StartTimer()
		if _, err := s.GetEngrams(sctx, ws, ids); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		snap.Close()
		b.StartTimer()
	}
}

func BenchmarkCacheFill_GetEngrams50_AllHit(b *testing.B)  { benchGetEngrams50(b, 1) }
func BenchmarkCacheFill_GetEngrams50_AllMiss(b *testing.B) { benchGetEngrams50(b, 0) }
func BenchmarkCacheFill_GetEngrams50_Mixed(b *testing.B)   { benchGetEngrams50(b, 2) }
