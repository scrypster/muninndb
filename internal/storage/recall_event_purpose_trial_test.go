package storage

import (
	"context"
	"testing"
	"time"
)

// The purpose gate is a CLOSED allowlist: widening it must be a reviewable
// diff, and an unknown purpose must be refused loudly rather than logged
// quietly. These pin both directions for the cognition-trial purpose.
//
// PRIVACY: synthetic IDs and query text only.

func TestScanRecallEvents_TrialPurposeIsAllowed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("trial-purpose-vault")

	if err := store.WriteRecallEvent(ctx, ws, NewULID(), &RecallEvent{
		Context:    []string{"kiln firing schedule"},
		Threshold:  0.1,
		SurfacedAt: time.Now().UnixNano(),
		Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.7}},
	}); err != nil {
		t.Fatalf("WriteRecallEvent: %v", err)
	}

	seen := 0
	if err := store.ScanRecallEvents(ctx, ws, RecallPurposeTrial, func(ULID, *RecallEvent) error {
		seen++
		return nil
	}); err != nil {
		t.Fatalf("ScanRecallEvents(trial): %v", err)
	}
	if seen != 1 {
		t.Errorf("scanned %d events, want 1", seen)
	}
}

func TestScanRecallEvents_UnknownPurposeStillRefused(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("trial-purpose-vault")
	err := store.ScanRecallEvents(context.Background(), ws, RecallEventPurpose("cognition_trial"), func(ULID, *RecallEvent) error {
		t.Fatal("callback must not run for a refused purpose")
		return nil
	})
	if err == nil {
		t.Error("a purpose outside the allowlist must be refused — the gate is closed, " +
			"and a near-miss spelling is exactly what it exists to catch")
	}
}
