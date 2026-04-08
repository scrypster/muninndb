package cognitive

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockReplayActivator records all SyntheticActivate calls.
type mockReplayActivator struct {
	mu    sync.Mutex
	calls []syntheticCall
}

type syntheticCall struct {
	vault   string
	context string
}

func (m *mockReplayActivator) SyntheticActivate(_ context.Context, vault string, queryContext string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, syntheticCall{vault: vault, context: queryContext})
	return nil
}

func (m *mockReplayActivator) getCalls() []syntheticCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]syntheticCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// mockReplayStore returns canned vaults and engrams.
type mockReplayStore struct {
	vaults  []string
	engrams map[string][]ReplayEngram
}

func (m *mockReplayStore) ListVaults(_ context.Context) ([]string, error) {
	return m.vaults, nil
}

func (m *mockReplayStore) RecentEngrams(_ context.Context, vault string, limit int) ([]ReplayEngram, error) {
	engrams := m.engrams[vault]
	if len(engrams) > limit {
		engrams = engrams[:limit]
	}
	return engrams, nil
}

func TestReplayWorkerStops(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{}
	cfg := ReplayConfig{
		Interval:     50 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   10,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rw.Run(ctx)
		close(done)
	}()

	// Cancel context and verify the worker exits promptly.
	cancel()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("replay worker did not stop after context cancellation")
	}
}

func TestReplayWorkerStopsViaStopChannel(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{}
	cfg := ReplayConfig{
		Interval:     50 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   10,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx := context.Background()
	go rw.Run(ctx)

	// Stop via Stop() method.
	rw.Stop()
	select {
	case <-rw.Done():
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("replay worker did not stop after Stop() call")
	}
}

func TestReplayCycleCallsActivate(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{
		vaults: []string{"vault-a", "vault-b"},
		engrams: map[string][]ReplayEngram{
			"vault-a": {
				{ID: [16]byte{1}, Concept: "concept-1", Content: "content-1", Vault: "vault-a"},
				{ID: [16]byte{2}, Concept: "concept-2", Content: "content-2", Vault: "vault-a"},
			},
			"vault-b": {
				{ID: [16]byte{3}, Concept: "concept-3", Content: "content-3", Vault: "vault-b"},
			},
		},
	}
	cfg := ReplayConfig{
		Interval:     20 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	// Wait for at least one cycle to complete.
	deadline := time.After(3 * time.Second)
	for {
		calls := activator.getCalls()
		if len(calls) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("expected at least 3 activate calls, got %d", len(activator.getCalls()))
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-rw.Done()

	calls := activator.getCalls()
	// Verify we got calls for all 3 engrams across both vaults.
	vaultACalls := 0
	vaultBCalls := 0
	for _, c := range calls {
		switch c.vault {
		case "vault-a":
			vaultACalls++
		case "vault-b":
			vaultBCalls++
		}
	}
	if vaultACalls < 2 {
		t.Errorf("expected at least 2 calls for vault-a, got %d", vaultACalls)
	}
	if vaultBCalls < 1 {
		t.Errorf("expected at least 1 call for vault-b, got %d", vaultBCalls)
	}

	// Verify the context strings are built correctly.
	firstCycle := calls[:3]
	found := false
	for _, c := range firstCycle {
		if c.vault == "vault-a" && c.context == "concept-1: content-1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected synthetic context 'concept-1: content-1' in first cycle calls: %v", firstCycle)
	}
}

func TestReplaySkipsEmptyVaults(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{
		vaults: []string{"empty-vault", "has-data"},
		engrams: map[string][]ReplayEngram{
			"has-data": {
				{ID: [16]byte{1}, Concept: "concept-1", Content: "content-1", Vault: "has-data"},
			},
			// empty-vault has no entries
		},
	}
	cfg := ReplayConfig{
		Interval:     20 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	// Wait for at least one cycle.
	deadline := time.After(3 * time.Second)
	for {
		calls := activator.getCalls()
		if len(calls) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected at least 1 activate call")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-rw.Done()

	// Verify no calls were made with the empty vault.
	calls := activator.getCalls()
	for _, c := range calls {
		if c.vault == "empty-vault" {
			t.Errorf("expected no calls for empty-vault, got: %+v", c)
		}
	}
}

func TestReplayContentTruncation(t *testing.T) {
	activator := &mockReplayActivator{}
	longContent := ""
	for i := 0; i < 50; i++ {
		longContent += fmt.Sprintf("word-%d ", i)
	}
	store := &mockReplayStore{
		vaults: []string{"test"},
		engrams: map[string][]ReplayEngram{
			"test": {
				{ID: [16]byte{1}, Concept: "long", Content: longContent, Vault: "test"},
			},
		},
	}
	cfg := ReplayConfig{
		Interval:     20 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		calls := activator.getCalls()
		if len(calls) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected at least 1 activate call")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-rw.Done()

	calls := activator.getCalls()
	// The context should be truncated: "long: <first 200 chars of content>"
	for _, c := range calls {
		// "long: " = 6 chars + 200 chars max content = 206 max
		if len(c.context) > 210 {
			t.Errorf("expected truncated context, got length %d", len(c.context))
		}
	}
}
