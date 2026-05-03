package cognitive

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// errActivator fails every SyntheticActivate call with an error.
type errActivator struct{}

func (e *errActivator) SyntheticActivate(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("synthetic activate error")
}

// partialErrActivator fails for a specific vault.
type partialErrActivator struct {
	mu       sync.Mutex
	failVault string
	calls     int
}

func (p *partialErrActivator) SyntheticActivate(_ context.Context, vault string, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if vault == p.failVault {
		return fmt.Errorf("fail vault")
	}
	return nil
}

func TestReplayMetricsIncrement(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{
		vaults: []string{"v1", "v2"},
		engrams: map[string][]ReplayEngram{
			"v1": {
				{ID: [16]byte{1}, Concept: "c1", Content: "content-1", Vault: "v1"},
				{ID: [16]byte{2}, Concept: "c2", Content: "content-2", Vault: "v1"},
			},
			"v2": {
				{ID: [16]byte{3}, Concept: "c3", Content: "content-3", Vault: "v2"},
			},
		},
	}
	cfg := ReplayConfig{
		Interval:     20 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	// Run a single cycle directly.
	rw.replayCycle(context.Background())

	m := rw.Metrics()
	if m.CyclesCompleted != 1 {
		t.Errorf("CyclesCompleted = %d, want 1", m.CyclesCompleted)
	}
	if m.EngramsReplayed != 3 {
		t.Errorf("EngramsReplayed = %d, want 3", m.EngramsReplayed)
	}
	if m.VaultsProcessed != 2 {
		t.Errorf("VaultsProcessed = %d, want 2", m.VaultsProcessed)
	}
	if m.Errors != 0 {
		t.Errorf("Errors = %d, want 0", m.Errors)
	}
	if m.LastCycleDuration <= 0 {
		t.Errorf("LastCycleDuration = %v, want > 0", m.LastCycleDuration)
	}
	if m.LastCycleAt.IsZero() {
		t.Error("LastCycleAt is zero, want non-zero")
	}

	// Run another cycle and verify counters accumulate.
	rw.replayCycle(context.Background())
	m2 := rw.Metrics()
	if m2.CyclesCompleted != 2 {
		t.Errorf("CyclesCompleted after 2 cycles = %d, want 2", m2.CyclesCompleted)
	}
	if m2.EngramsReplayed != 6 {
		t.Errorf("EngramsReplayed after 2 cycles = %d, want 6", m2.EngramsReplayed)
	}
	if m2.VaultsProcessed != 4 {
		t.Errorf("VaultsProcessed after 2 cycles = %d, want 4", m2.VaultsProcessed)
	}
}

func TestReplayMetricsErrors(t *testing.T) {
	activator := &errActivator{}
	store := &mockReplayStore{
		vaults: []string{"v1"},
		engrams: map[string][]ReplayEngram{
			"v1": {
				{ID: [16]byte{1}, Concept: "c1", Content: "content-1", Vault: "v1"},
				{ID: [16]byte{2}, Concept: "c2", Content: "content-2", Vault: "v1"},
			},
		},
	}
	cfg := ReplayConfig{
		Interval:     20 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	rw.replayCycle(context.Background())

	m := rw.Metrics()
	if m.CyclesCompleted != 1 {
		t.Errorf("CyclesCompleted = %d, want 1", m.CyclesCompleted)
	}
	if m.Errors != 2 {
		t.Errorf("Errors = %d, want 2", m.Errors)
	}
	if m.EngramsReplayed != 0 {
		t.Errorf("EngramsReplayed = %d, want 0 (all should have failed)", m.EngramsReplayed)
	}
	if m.VaultsProcessed != 1 {
		t.Errorf("VaultsProcessed = %d, want 1", m.VaultsProcessed)
	}
}

func TestReplayMetricsThreadSafety(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{
		vaults: []string{"v1"},
		engrams: map[string][]ReplayEngram{
			"v1": {
				{ID: [16]byte{1}, Concept: "c1", Content: "content-1", Vault: "v1"},
			},
		},
	}
	cfg := ReplayConfig{
		Interval:     10 * time.Millisecond,
		LearningRate: 0.005,
		MaxEngrams:   100,
	}
	rw := NewReplayWorker(cfg, activator, store)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	// Wait for at least one cycle to complete before starting concurrent reads.
	deadline := time.After(3 * time.Second)
	for {
		if rw.Metrics().CyclesCompleted >= 1 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-rw.Done()
			t.Fatal("timed out waiting for first replay cycle")
		case <-time.After(time.Millisecond):
		}
	}

	// Concurrently read metrics while cycles are still running.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m := rw.Metrics()
				// Sanity: exercises concurrent reads without data races.
				_ = m.CyclesCompleted
				_ = m.EngramsReplayed
				_ = m.Errors
				_ = m.VaultsProcessed
				_ = m.LastCycleDuration
				_ = m.LastCycleAt
			}
		}()
	}
	wg.Wait()

	cancel()
	<-rw.Done()

	// After shutdown, metrics should reflect completed cycles.
	m := rw.Metrics()
	if m.CyclesCompleted == 0 {
		t.Error("expected at least 1 cycle after running worker")
	}
	if m.EngramsReplayed == 0 {
		t.Error("expected at least 1 engram replayed")
	}
}
