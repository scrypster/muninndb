package storage

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

// TestWriteContentionHarness is a MEASUREMENT, not a check: it runs only with
// MUNINN_CONTENTION_HARNESS=1. 16 mixed record writers x 400 ops each over K
// engrams, with and without UpdateEmbedding writers in the mix (4 of 16); it
// prints the non-embed writers' ops/s and p99 per-op latency.
func TestWriteContentionHarness(t *testing.T) {
	if os.Getenv("MUNINN_CONTENTION_HARNESS") != "1" {
		t.Skip("measurement harness; set MUNINN_CONTENTION_HARNESS=1")
	}
	// replog=true installs a stand-in for the cluster replication log:
	// ReplicationLog.Append is one Pebble Sync commit under a process-wide
	// mutex, and replicateBatch runs under the engram's stripe.
	for _, replog := range []bool{false, true} {
		for _, k := range []int{1, 64, 10000} {
			for _, withEmbed := range []bool{false, true} {
				opsPerSec, p99 := runContention(t, k, withEmbed, replog)
				t.Logf("replog=%-5v K=%-6d embed=%-5v non-embed ops/s=%8.0f  p99(non-embed)=%v", replog, k, withEmbed, opsPerSec, p99)
			}
		}
	}
}

func runContention(t *testing.T, k int, withEmbed, replog bool) (float64, time.Duration) {
	ps := openTestStore(t)
	if replog {
		var logMu sync.Mutex
		var seq uint64
		ps.repLogAppend = func(_ uint8, _, value []byte) error {
			logMu.Lock()
			defer logMu.Unlock()
			seq++
			return ps.db.Set([]byte(fmt.Sprintf("\xffreplog-%020d", seq)), value, pebble.Sync)
		}
	}
	ctx := context.Background()
	ws := ps.VaultPrefix(fmt.Sprintf("trail-contention-%d-%v", k, withEmbed))
	ids := make([]ULID, k)
	for i := range ids {
		id, err := ps.WriteEngram(ctx, ws, &Engram{Concept: "segment", Content: fmt.Sprintf("segment %d", i), Confidence: 0.6, Relevance: 0.5, Stability: 30})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i%11) / 11
	}
	kinds := []func(r *rand.Rand, id ULID) error{
		func(r *rand.Rand, id ULID) error { return ps.UpdateRelevance(ctx, ws, id, r.Float32(), 30) },
		func(r *rand.Rand, id ULID) error { return ps.UpdateTrust(ctx, ws, id, TrustLevel(r.Intn(4)+1)) },
		func(r *rand.Rand, id ULID) error { return ps.TouchAccess(ctx, ws, id) },
		func(r *rand.Rand, id ULID) error {
			_, err := ps.StampValidUntil(ctx, ws, id, time.Unix(1_800_000_000+int64(r.Intn(1000)), 0), false)
			return err
		},
		func(r *rand.Rand, id ULID) error { return ps.UpdateConfidence(ctx, ws, id, r.Float32()) },
	}
	embed := func(r *rand.Rand, id ULID) error { return ps.UpdateEmbedding(ctx, ws, id, vec) }

	const writers, ops = 16, 400
	var mu sync.Mutex
	var lat []time.Duration
	var lastNonEmbed time.Time
	nonEmbedOps := 0
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			isEmbed := withEmbed && g%4 == 0
			w := kinds[g%len(kinds)]
			local := make([]time.Duration, 0, ops)
			for i := 0; i < ops; i++ {
				id := ids[r.Intn(k)]
				t0 := time.Now()
				var err error
				if isEmbed {
					err = embed(r, id)
				} else {
					err = w(r, id)
				}
				if err != nil {
					t.Error(err)
					return
				}
				if !isEmbed {
					local = append(local, time.Since(t0))
				}
			}
			mu.Lock()
			lat = append(lat, local...)
			if !isEmbed {
				nonEmbedOps += ops
				if now := time.Now(); now.After(lastNonEmbed) {
					lastNonEmbed = now
				}
			}
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	p99 := lat[len(lat)*99/100]
	// Throughput of the NON-embed writers (embed writers fsync and would
	// otherwise dominate the wall clock of the mixed runs).
	return float64(nonEmbedOps) / lastNonEmbed.Sub(start).Seconds(), p99
}
