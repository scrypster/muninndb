package cognitive

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockConsolidationStore records calls and returns configurable episode data.
type mockConsolidationStore struct {
	mu       sync.Mutex
	episodes map[string][]string // seedID → concepts
	stored   []consolidationRecord
}

type consolidationRecord struct {
	vault   string
	summary string
}

func (m *mockConsolidationStore) GetEpisodeConcepts(_ context.Context, _ string, seedID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.episodes[seedID], nil
}

func (m *mockConsolidationStore) StoreConsolidation(_ context.Context, vault string, summary string, _ []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored = append(m.stored, consolidationRecord{vault: vault, summary: summary})
	return nil
}

func (m *mockConsolidationStore) getStored() []consolidationRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]consolidationRecord, len(m.stored))
	copy(out, m.stored)
	return out
}

func TestConsolidationSkipsSmallEpisodes(t *testing.T) {
	activator := &mockReplayActivator{}
	// Engram with seedID "01000000000000000000000000000000" (hex of [16]byte{1})
	store := &mockReplayStore{
		vaults: []string{"test-vault"},
		engrams: map[string][]ReplayEngram{
			"test-vault": {
				{ID: [16]byte{1}, Concept: "small-ep", Content: "content", Vault: "test-vault"},
			},
		},
	}
	cs := &mockConsolidationStore{
		episodes: map[string][]string{
			"01000000000000000000000000000000": {"concept-a", "concept-b"}, // only 2 members
		},
	}

	cfg := ReplayConfig{
		Interval:             20 * time.Millisecond,
		LearningRate:         0.005,
		MaxEngrams:           100,
		EnableConsolidation:  true,
		ConsolidationMinSize: 3,
	}
	rw := NewReplayWorker(cfg, activator, store)
	rw.SetConsolidationStore(cs)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	// Wait for at least one cycle with activation.
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

	// Episode had only 2 members, minSize is 3 — no consolidation should occur.
	stored := cs.getStored()
	if len(stored) != 0 {
		t.Errorf("expected 0 consolidation records for small episode, got %d: %+v", len(stored), stored)
	}
}

func TestConsolidationGeneratesSummary(t *testing.T) {
	activator := &mockReplayActivator{}
	store := &mockReplayStore{
		vaults: []string{"test-vault"},
		engrams: map[string][]ReplayEngram{
			"test-vault": {
				{ID: [16]byte{1}, Concept: "big-ep", Content: "content", Vault: "test-vault"},
			},
		},
	}
	cs := &mockConsolidationStore{
		episodes: map[string][]string{
			"01000000000000000000000000000000": {"meeting notes", "action items", "deadline reminder"},
		},
	}

	cfg := ReplayConfig{
		Interval:             20 * time.Millisecond,
		LearningRate:         0.005,
		MaxEngrams:           100,
		EnableConsolidation:  true,
		ConsolidationMinSize: 3,
	}
	rw := NewReplayWorker(cfg, activator, store)
	rw.SetConsolidationStore(cs)

	ctx, cancel := context.WithCancel(context.Background())
	go rw.Run(ctx)

	// Wait for at least one consolidation to be stored.
	deadline := time.After(3 * time.Second)
	for {
		stored := cs.getStored()
		if len(stored) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected at least 1 consolidation record")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	<-rw.Done()

	stored := cs.getStored()
	if len(stored) == 0 {
		t.Fatal("expected consolidation records, got none")
	}

	rec := stored[0]
	if rec.vault != "test-vault" {
		t.Errorf("expected vault 'test-vault', got %q", rec.vault)
	}
	if !strings.HasPrefix(rec.summary, "Episode summary: ") {
		t.Errorf("expected summary to start with 'Episode summary: ', got %q", rec.summary)
	}
	if !strings.Contains(rec.summary, "meeting notes") {
		t.Errorf("expected summary to contain 'meeting notes', got %q", rec.summary)
	}
	if !strings.Contains(rec.summary, "action items") {
		t.Errorf("expected summary to contain 'action items', got %q", rec.summary)
	}
	if !strings.Contains(rec.summary, "deadline reminder") {
		t.Errorf("expected summary to contain 'deadline reminder', got %q", rec.summary)
	}

	expectedSummary := "Episode summary: meeting notes; action items; deadline reminder"
	if rec.summary != expectedSummary {
		t.Errorf("summary mismatch:\n  got:  %q\n  want: %q", rec.summary, expectedSummary)
	}
}
