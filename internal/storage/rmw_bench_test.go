package storage

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/provenance"
)

// BenchmarkRMWWriters measures the single-key read-modify-write writers that
// took the engram stripe lock in the storage-hardening pass (and TouchAccess,
// whose body changed). Uncontended: the lock's cost is one uncontended
// mutex pair per call; the rest is the Pebble read and commit both versions
// already paid. Compare against the same file on the parent commit.
func BenchmarkRMWWriters(b *testing.B) {
	ctx := context.Background()
	setup := func(b *testing.B) (*PebbleStore, [8]byte, ULID) {
		ps := openTestStore(b)
		ws := ps.VaultPrefix("trail-bench")
		id, err := ps.WriteEngram(ctx, ws, &Engram{
			Concept: "bench segment", Content: "a trail segment used by the write benchmarks",
			Confidence: 0.6, Relevance: 0.5, Stability: 30,
		})
		if err != nil {
			b.Fatal(err)
		}
		return ps, ws, id
	}
	b.Run("UpdateRelevance", func(b *testing.B) {
		ps, ws, id := setup(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := ps.UpdateRelevance(ctx, ws, id, float32(i%90+5)/100, 30); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UpdateTrust", func(b *testing.B) {
		ps, ws, id := setup(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := ps.UpdateTrust(ctx, ws, id, TrustLevel(i%4+1)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("StampValidUntil", func(b *testing.B) {
		ps, ws, id := setup(b)
		base := time.Unix(1_800_000_000, 0)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := ps.StampValidUntil(ctx, ws, id, base.Add(time.Duration(i)*time.Second), false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UpdateMetadata", func(b *testing.B) {
		ps, ws, id := setup(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := ps.UpdateMetadata(ctx, ws, id, &EngramMeta{
				State: StateActive, Confidence: 0.6, Relevance: 0.5, Stability: 30,
				AccessCount: uint32(i), UpdatedAt: time.Now(), LastAccess: time.Now(),
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("TouchAccess", func(b *testing.B) {
		ps, ws, id := setup(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := ps.TouchAccess(ctx, ws, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UpdateDigest", func(b *testing.B) {
		ps, _, id := setup(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := ps.UpdateDigest(ctx, id, "segment summary", nil, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("BatchUpdateEngramState", func(b *testing.B) {
		ps, ws, id := setup(b)
		states := []LifecycleState{StateActive, StateArchived}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			batch := ps.NewBatch()
			if err := batch.UpdateEngramState(ctx, ws, id, states[i%2]); err != nil {
				b.Fatal(err)
			}
			if err := batch.Commit(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ProvenanceSubmit", func(b *testing.B) {
		ps := openTestStore(b)
		ws := ps.VaultPrefix("trail-bench")
		id := NewULID()
		e := provenance.ProvenanceEntry{Timestamp: time.Now(), Operation: "bench"}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ps.provWork.Submit(ws, id, e)
		}
		b.StopTimer()
		ps.provWork.Drain()
	})
}
