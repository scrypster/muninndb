package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/cognitive"
)

// TestWorkerStats_ReturnsWithoutPanic verifies that WorkerStats() returns without
// panicking. In the test environment, all cognitive workers are nil, so the
// response must identify them as unconfigured without losing legacy counters.
func TestWorkerStats_ReturnsWithoutPanic(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	// testEnv wires nil hebbianWorker, contradictWorker, and confidenceWorker.
	stats := eng.WorkerStats()

	// Existing counters remain zero for compatibility.
	if stats.Hebbian.Processed != 0 {
		t.Errorf("Hebbian.Processed = %d, want 0 (nil worker in test env)", stats.Hebbian.Processed)
	}
	if stats.Contradict.Processed != 0 {
		t.Errorf("Contradict.Processed = %d, want 0 (nil worker in test env)", stats.Contradict.Processed)
	}
	if stats.Confidence.Processed != 0 {
		t.Errorf("Confidence.Processed = %d, want 0 (nil worker in test env)", stats.Confidence.Processed)
	}
	if stats.Hebbian.Errors != 0 {
		t.Errorf("Hebbian.Errors = %d, want 0", stats.Hebbian.Errors)
	}
	for name, worker := range map[string]cognitive.WorkerStats{
		"hebbian": stats.Hebbian, "contradict": stats.Contradict, "confidence": stats.Confidence,
	} {
		if worker.Enabled {
			t.Errorf("%s.Enabled = true for nil worker", name)
		}
		if worker.Status != "disabled" {
			t.Errorf("%s.Status = %q, want disabled", name, worker.Status)
		}
	}
}

// TestUnsubscribe_InvalidID verifies that calling Unsubscribe with a non-existent
// subscription ID does not panic and returns nil.
// Engine.Unsubscribe delegates to trigger.TriggerSystem.Unsubscribe which calls
// sync.Map.Delete — a no-op for missing keys — so the result is always nil.
func TestUnsubscribe_InvalidID(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.Unsubscribe(ctx, "nonexistent-subscription-id")
	if err != nil {
		t.Errorf("Unsubscribe(nonexistent): expected nil error, got %v", err)
	}
}
