package cognitive

import (
	"context"
	"testing"
)

// TestContradictionSeverityKnownPairs verifies that known contradiction pairs
// return non-zero severity.
func TestContradictionSeverityKnownPairs(t *testing.T) {
	cases := []struct {
		relA, relB  uint16
		wantNonZero bool
	}{
		{1, 2, true},
		{2, 1, true},
		{8, 9, true},
		{1, 1, false},
		{3, 5, false},
	}
	for _, tt := range cases {
		got := ContradictionSeverity(tt.relA, tt.relB)
		if tt.wantNonZero && got <= 0 {
			t.Errorf("ContradictionSeverity(%d,%d) = %v, want > 0", tt.relA, tt.relB, got)
		}
		if !tt.wantNonZero && got != 0 {
			t.Errorf("ContradictionSeverity(%d,%d) = %v, want 0", tt.relA, tt.relB, got)
		}
	}
}

// TestContradictWorkerProcessBatchDirectNegation verifies that Supports + Contradicts
// on the same engram triggers FlagContradiction with severity 1.0.
func TestContradictWorkerProcessBatchDirectNegation(t *testing.T) {
	flagged := [][2][16]byte{}
	store := &stubContradictStore{
		onFlag: func(_ context.Context, _ [8]byte, a, b [16]byte) error {
			flagged = append(flagged, [2][16]byte{a, b})
			return nil
		},
	}
	cw := NewContradictWorker(store)
	ctx := context.Background()

	ws := [8]byte{1}
	engramID := [16]byte{10}
	targetA := [16]byte{11}
	targetB := [16]byte{12}

	item := ContradictItem{
		WS:       ws,
		EngramID: engramID,
		Associations: []ContradictAssoc{
			{EngramID: engramID, TargetID: targetA, TargetHash: 111, RelType: 1},
			{EngramID: engramID, TargetID: targetB, TargetHash: 222, RelType: 2},
		},
	}
	if err := cw.processBatch(ctx, []ContradictItem{item}); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(flagged) == 0 {
		t.Fatal("expected FlagContradiction to be called, got 0 calls")
	}
}

// TestContradictWorkerProcessBatchSameRelDifferentTarget verifies (COG-23)
// that an engram asserting the SAME relation type at two DIFFERENT targets
// is NOT flagged — same-RelType/different-target is an ordinary write shape
// (e.g. "references -> A" and "references -> B"), not a contradiction, and
// must never be synthesized into one regardless of TargetHash. Previously
// this test pinned the opposite (fabricated) behavior; that was pinning a
// bug, not a feature — see contradict.go's former same-RelType rule.
func TestContradictWorkerProcessBatchSameRelDifferentTarget(t *testing.T) {
	called := false
	store := &stubContradictStore{
		onFlag: func(_ context.Context, _ [8]byte, _, _ [16]byte) error {
			called = true
			return nil
		},
	}
	cw := NewContradictWorker(store)
	ctx := context.Background()

	item := ContradictItem{
		WS:       [8]byte{1},
		EngramID: [16]byte{1},
		Associations: []ContradictAssoc{
			{TargetID: [16]byte{2}, TargetHash: 100, RelType: 5},
			{TargetID: [16]byte{3}, TargetHash: 200, RelType: 5},
		},
	}
	if err := cw.processBatch(ctx, []ContradictItem{item}); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if called {
		t.Error("FlagContradiction called for same-RelType/different-target write (COG-23): must not be synthesized as a contradiction")
	}
}

// TestContradictWorkerOnFoundCallback verifies that OnFound is called when a
// contradiction is detected.
func TestContradictWorkerOnFoundCallback(t *testing.T) {
	store := &stubContradictStore{}
	cw := NewContradictWorker(store)
	ctx := context.Background()

	events := []ContradictionEvent{}
	item := ContradictItem{
		WS:       [8]byte{1},
		EngramID: [16]byte{1},
		Associations: []ContradictAssoc{
			{TargetID: [16]byte{2}, TargetHash: 10, RelType: 1},
			{TargetID: [16]byte{3}, TargetHash: 20, RelType: 2},
		},
		OnFound: func(e ContradictionEvent) { events = append(events, e) },
	}
	_ = cw.processBatch(ctx, []ContradictItem{item})
	if len(events) == 0 {
		t.Error("expected OnFound callback to be called")
	}
}

// TestContradictWorkerNoFalsePositives verifies that compatible relations
// on the same engram do not trigger FlagContradiction.
func TestContradictWorkerNoFalsePositives(t *testing.T) {
	called := false
	store := &stubContradictStore{
		onFlag: func(_ context.Context, _ [8]byte, _, _ [16]byte) error {
			called = true
			return nil
		},
	}
	cw := NewContradictWorker(store)
	ctx := context.Background()

	item := ContradictItem{
		WS:       [8]byte{1},
		EngramID: [16]byte{1},
		Associations: []ContradictAssoc{
			{TargetID: [16]byte{2}, TargetHash: 100, RelType: 5},
			{TargetID: [16]byte{3}, TargetHash: 100, RelType: 6},
		},
	}
	_ = cw.processBatch(ctx, []ContradictItem{item})
	if called {
		t.Error("compatible relations with same target hash should not trigger contradiction")
	}
}

// stubContradictStore is a test double for ContradictionStore.
type stubContradictStore struct {
	onFlag func(ctx context.Context, ws [8]byte, a, b [16]byte) error
}

func (s *stubContradictStore) FlagContradiction(ctx context.Context, ws [8]byte, a, b [16]byte) (bool, error) {
	if s.onFlag != nil {
		return true, s.onFlag(ctx, ws, a, b)
	}
	return true, nil
}
