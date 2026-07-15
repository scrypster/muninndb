package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

// writeLeaseTestEngram stores a minimal active engram and returns its ID.
func writeLeaseTestEngram(t *testing.T, store *PebbleStore, ws [8]byte) ULID {
	t.Helper()
	id, err := store.WriteEngram(context.Background(), ws, &Engram{
		Concept: "work item",
		Content: "a unit of ongoing work",
		State:   StateActive,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	return id
}

func TestLeaseExpiryAndLiveness(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	tests := []struct {
		name       string
		lease      Lease
		wantLive   bool
		wantExpire bool
	}{
		{"unleased", Lease{}, false, true},
		{
			"fresh",
			Lease{Owner: "host:sess", Heartbeat: now.UnixNano(), TTLSeconds: 60},
			true, false,
		},
		{
			"within ttl",
			Lease{Owner: "host:sess", Heartbeat: now.Add(-30 * time.Second).UnixNano(), TTLSeconds: 60},
			true, false,
		},
		{
			"exactly at ttl is still live",
			Lease{Owner: "host:sess", Heartbeat: now.Add(-60 * time.Second).UnixNano(), TTLSeconds: 60},
			true, false,
		},
		{
			"past ttl",
			Lease{Owner: "host:sess", Heartbeat: now.Add(-61 * time.Second).UnixNano(), TTLSeconds: 60},
			false, true,
		},
		{
			"zero ttl is never live",
			Lease{Owner: "host:sess", Heartbeat: now.UnixNano(), TTLSeconds: 0},
			false, true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lease.Live(now); got != tc.wantLive {
				t.Errorf("Live() = %v, want %v", got, tc.wantLive)
			}
			if got := tc.lease.Expired(now); got != tc.wantExpire {
				t.Errorf("Expired() = %v, want %v", got, tc.wantExpire)
			}
		})
	}
}

func TestLeaseRoundtrip(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("lease-roundtrip")
	id := writeLeaseTestEngram(t, store, ws)

	// Absent lease reads back as the zero value, not an error.
	got, err := store.GetLease(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetLease (absent): %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected zero lease, got %+v", got)
	}

	want := Lease{Owner: "hostA:sess1", Heartbeat: time.Now().UnixNano(), TTLSeconds: 120}
	out, err := store.CompareAndSet(ctx, ws, id, CASCondition{}, CASMutation{Lease: &want})
	if err != nil {
		t.Fatalf("CompareAndSet put lease: %v", err)
	}
	if !out.Applied {
		t.Fatalf("expected lease put to apply")
	}
	got, err = store.GetLease(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if got != want {
		t.Fatalf("GetLease = %+v, want %+v", got, want)
	}

	// Clearing via an empty-owner mutation removes the sidecar.
	if _, err := store.CompareAndSet(ctx, ws, id, CASCondition{}, CASMutation{Lease: &Lease{}}); err != nil {
		t.Fatalf("CompareAndSet clear lease: %v", err)
	}
	got, err = store.GetLease(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetLease after clear: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected cleared lease, got %+v", got)
	}
}

func TestCompareAndSetStateGuard(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("cas-state")
	id := writeLeaseTestEngram(t, store, ws)

	active := StateActive
	completed := StateCompleted
	blocked := StateBlocked

	// Guard mismatch: engram is active, expect blocked -> not applied, current returned.
	out, err := store.CompareAndSet(ctx, ws, id,
		CASCondition{State: &blocked},
		CASMutation{State: &completed})
	if err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}
	if out.Applied {
		t.Fatalf("expected guard mismatch to not apply")
	}
	if out.State != StateActive {
		t.Fatalf("current state = %v, want active", out.State)
	}

	// Guard match: expect active -> transition to completed.
	out, err = store.CompareAndSet(ctx, ws, id,
		CASCondition{State: &active},
		CASMutation{State: &completed})
	if err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}
	if !out.Applied || out.State != StateCompleted {
		t.Fatalf("expected applied completed, got applied=%v state=%v", out.Applied, out.State)
	}

	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil || len(metas) == 0 || metas[0] == nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if metas[0].State != StateCompleted {
		t.Fatalf("persisted state = %v, want completed", metas[0].State)
	}
}

func TestCompareAndSetLeaseGuardDetectsChange(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("cas-lease")
	id := writeLeaseTestEngram(t, store, ws)

	first := Lease{Owner: "hostA:sess1", Heartbeat: 1000, TTLSeconds: 60}
	if _, err := store.CompareAndSet(ctx, ws, id, CASCondition{Lease: &Lease{}}, CASMutation{Lease: &first}); err != nil {
		t.Fatalf("initial claim: %v", err)
	}

	// A refresh advances the heartbeat.
	refreshed := Lease{Owner: "hostA:sess1", Heartbeat: 2000, TTLSeconds: 60}
	if _, err := store.CompareAndSet(ctx, ws, id, CASCondition{Lease: &first}, CASMutation{Lease: &refreshed}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// A reclaim attempt that guards the stale (pre-refresh) lease must fail —
	// full-lease equality catches the heartbeat change (guards against ABA).
	takeover := Lease{Owner: "hostB:sess9", Heartbeat: 3000, TTLSeconds: 60}
	out, err := store.CompareAndSet(ctx, ws, id, CASCondition{Lease: &first}, CASMutation{Lease: &takeover})
	if err != nil {
		t.Fatalf("reclaim CompareAndSet: %v", err)
	}
	if out.Applied {
		t.Fatalf("stale-guard reclaim must not apply")
	}
	if out.Lease != refreshed {
		t.Fatalf("current lease = %+v, want %+v", out.Lease, refreshed)
	}
}

func TestCompareAndSetConcurrentSingleWinner(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("cas-concurrent")
	id := writeLeaseTestEngram(t, store, ws)

	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := Lease{Owner: "agent", Heartbeat: int64(i + 1), TTLSeconds: 60}
			// Every goroutine expects the unleased state; exactly one can win.
			out, err := store.CompareAndSet(ctx, ws, id, CASCondition{Lease: &Lease{}}, CASMutation{Lease: &owner})
			if err != nil {
				t.Errorf("CompareAndSet: %v", err)
				return
			}
			if out.Applied {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d", winners)
	}
}
