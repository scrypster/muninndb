package storage

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// TestRMWWriters_RandomizedNoFieldRegresses is the randomized half of item 3.
// One goroutine per writer KIND, each the sole owner of its field(s), hammers
// a small key set in a seeded random order while a reader keeps the caches
// warm. Because each field has exactly one writer goroutine, "the last value
// its owner committed" is known exactly; at quiescence every field must equal
// it. A writer that rewrites the record from a copy read outside the stripe
// lock reverts some other owner's field, and this catches it on most seeds.
//
// Kept short on purpose (CI budget): 4 engrams x 8 writers x 40 ops.
func TestRMWWriters_RandomizedNoFieldRegresses(t *testing.T) {
	const (
		nIDs = 4
		nOps = 40
	)
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)

	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-stress")
	ids := make([]ULID, nIDs)
	for i := range ids {
		ids[i] = writeRMWFixture(t, ps, ws, fmt.Sprintf("trail segment %c survey", 'a'+i))
	}

	type last struct {
		relevance  float32
		trust      TrustLevel
		validUntil time.Time
		confidence float32
		touches    uint32
		tags       []string
		summary    string
		state      LifecycleState
	}
	final := make([]last, nIDs)
	for i := range final {
		final[i].relevance = 0.5
		final[i].trust = TrustInferred
		final[i].confidence = 0.6
		final[i].tags = []string{"trail"}
		final[i].state = StateActive
	}
	var mu sync.Mutex // guards final
	record := func(i int, f func(*last)) { mu.Lock(); f(&final[i]); mu.Unlock() }

	base := time.Unix(1_800_000_000, 0).UTC()
	writers := map[string]func(r *rand.Rand, i, op int) error{
		"relevance": func(r *rand.Rand, i, op int) error {
			v := float32(r.Intn(90)+5) / 100
			if err := ps.UpdateRelevance(ctx, ws, ids[i], v, 30); err != nil {
				return err
			}
			record(i, func(l *last) { l.relevance = v })
			return nil
		},
		"trust": func(r *rand.Rand, i, op int) error {
			v := []TrustLevel{TrustVerified, TrustInferred, TrustExternal, TrustUntrusted}[r.Intn(4)]
			if err := ps.UpdateTrust(ctx, ws, ids[i], v); err != nil {
				return err
			}
			record(i, func(l *last) { l.trust = v })
			return nil
		},
		"validUntil": func(r *rand.Rand, i, op int) error {
			v := base.Add(time.Duration(op+1) * time.Hour)
			if _, err := ps.StampValidUntil(ctx, ws, ids[i], v, false); err != nil {
				return err
			}
			record(i, func(l *last) { l.validUntil = v })
			return nil
		},
		"confidence": func(r *rand.Rand, i, op int) error {
			v := float32(r.Intn(90)+5) / 100
			if err := ps.UpdateConfidence(ctx, ws, ids[i], v); err != nil {
				return err
			}
			record(i, func(l *last) { l.confidence = v })
			return nil
		},
		"access": func(r *rand.Rand, i, op int) error {
			if err := ps.TouchAccess(ctx, ws, ids[i]); err != nil {
				return err
			}
			record(i, func(l *last) { l.touches++ })
			return nil
		},
		"tags": func(r *rand.Rand, i, op int) error {
			v := []string{"trail", fmt.Sprintf("crew-%d", op)}
			if err := ps.UpdateTags(ctx, ws, ids[i], v); err != nil {
				return err
			}
			record(i, func(l *last) { l.tags = v })
			return nil
		},
		"digest": func(r *rand.Rand, i, op int) error {
			v := fmt.Sprintf("survey note %d", op)
			if err := ps.UpdateDigest(ctx, ids[i], v, nil, "", ""); err != nil {
				return err
			}
			record(i, func(l *last) { l.summary = v })
			return nil
		},
		"state": func(r *rand.Rand, i, op int) error {
			var v LifecycleState
			switch r.Intn(3) {
			case 0:
				v = StateSoftDeleted
				if err := ps.SoftDelete(ctx, ws, ids[i]); err != nil {
					return err
				}
			case 1:
				v = StateActive
				if _, err := ps.CompareAndSet(ctx, ws, ids[i], CASCondition{}, CASMutation{State: &v}); err != nil {
					return err
				}
			default:
				v = StateArchived
				b := ps.NewBatch()
				if err := b.UpdateEngramState(ctx, ws, ids[i], v); err != nil {
					b.Discard()
					return err
				}
				if err := b.Commit(); err != nil {
					return err
				}
			}
			record(i, func(l *last) { l.state = v })
			return nil
		},
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() { // keep the L1 and metadata caches warm and contended
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = ps.GetEngrams(ctx, ws, ids)
			_, _ = ps.GetMetadata(ctx, ws, ids)
		}
	}()

	var wg sync.WaitGroup
	k := int64(0)
	for name, w := range writers {
		k++
		wg.Add(1)
		go func(name string, w func(*rand.Rand, int, int) error, r *rand.Rand) {
			defer wg.Done()
			for op := 0; op < nOps; op++ {
				if err := w(r, r.Intn(nIDs), op); err != nil {
					t.Errorf("%s op %d: %v", name, op, err)
					return
				}
			}
		}(name, w, rand.New(rand.NewSource(seed+k)))
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	for i, id := range ids {
		got, err := ps.GetEngramUncached(ctx, ws, id)
		if err != nil {
			t.Fatal(err)
		}
		want := final[i]
		var bad []string
		if got.Relevance != want.relevance {
			bad = append(bad, fmt.Sprintf("relevance %v want %v", got.Relevance, want.relevance))
		}
		if got.Trust != want.trust {
			bad = append(bad, fmt.Sprintf("trust %v want %v", got.Trust, want.trust))
		}
		if !got.ValidUntil.Equal(want.validUntil) {
			bad = append(bad, fmt.Sprintf("validUntil %v want %v", got.ValidUntil, want.validUntil))
		}
		if got.Confidence != want.confidence {
			bad = append(bad, fmt.Sprintf("confidence %v want %v", got.Confidence, want.confidence))
		}
		if got.AccessCount != want.touches {
			bad = append(bad, fmt.Sprintf("accessCount %d want %d", got.AccessCount, want.touches))
		}
		if fmt.Sprint(got.Tags) != fmt.Sprint(want.tags) {
			bad = append(bad, fmt.Sprintf("tags %v want %v", got.Tags, want.tags))
		}
		if got.Summary != want.summary {
			bad = append(bad, fmt.Sprintf("summary %q want %q", got.Summary, want.summary))
		}
		if got.State != want.state {
			bad = append(bad, fmt.Sprintf("state %v want %v", got.State, want.state))
		}
		if len(bad) > 0 {
			t.Errorf("engram %d: a field regressed below its owner's last committed value: %v", i, bad)
			continue
		}
		assertLifecycle(t, ps, ws, id, want.state)
		assertRelevanceBucket(t, ps, ws, id, 0.05, 0.15, 0.25, 0.35, 0.45, 0.5, 0.55, 0.65, 0.75, 0.85, 0.95)
	}
}
