package cognitive

import (
	"context"
	"sync"
	"testing"
)

// fakeContraConfStore is an in-memory double that satisfies both
// ContradictionStore and ConfidenceStore, so a single fake can drive the
// real ContradictWorker.processBatch and ConfidenceWorker.processBatch
// exactly as engine.go's write path does (minus the trigger notify, which
// is not under test here).
type fakeContraConfStore struct {
	mu         sync.Mutex
	confidence map[[16]byte]float32
	flagged    [][2][16]byte
}

func newFakeContraConfStore(a, b [16]byte, confA, confB float32) *fakeContraConfStore {
	return &fakeContraConfStore{
		confidence: map[[16]byte]float32{a: confA, b: confB},
	}
}

func (s *fakeContraConfStore) FlagContradiction(_ context.Context, _ [8]byte, a, b [16]byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flagged = append(s.flagged, [2][16]byte{a, b})
	return true, nil
}

func (s *fakeContraConfStore) GetConfidence(_ context.Context, _ [8]byte, id [16]byte) (float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confidence[id], nil
}

func (s *fakeContraConfStore) UpdateConfidence(_ context.Context, _ [8]byte, id [16]byte, c float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.confidence[id] = c
	return nil
}

// applyOnFound mirrors engine.go's Write()/BatchWrite() OnFound closure: for
// every ContradictionEvent found, submit an EvidenceContradiction update for
// BOTH engrams, then process that batch synchronously (the real worker would
// do this asynchronously off a channel; processBatch is called directly here
// for deterministic, fast, in-memory tests).
func applyOnFound(t *testing.T, confW *ConfidenceWorker, ws [8]byte, events []ContradictionEvent) {
	t.Helper()
	if len(events) == 0 {
		return
	}
	var updates []ConfidenceUpdate
	for _, ev := range events {
		updates = append(updates,
			ConfidenceUpdate{WS: ws, EngramID: ev.EngramA, Evidence: EvidenceContradiction, Source: "contradiction_detected"},
			ConfidenceUpdate{WS: ws, EngramID: ev.EngramB, Evidence: EvidenceContradiction, Source: "contradiction_detected"},
		)
	}
	if err := confW.processBatch(context.Background(), updates); err != nil {
		t.Fatalf("ConfidenceWorker.processBatch: %v", err)
	}
}

// TestWrite_TwoTargetsSameRelType_NoFabricatedContradiction is the COG-23
// pin. Storing one ordinary engram with two associations of the SAME
// RelType pointing at two DIFFERENT targets (e.g. "references -> A" and
// "references -> B") must produce zero contradiction flags and leave both
// targets' stored confidence untouched. Before the fix, contradict.go's
// same-RelType/different-target rule fabricated a 0.8-severity contradiction
// here — with no semantic signal consulted — which both persisted a 0x0A
// contradiction marker and dragged BOTH engrams' confidence down via
// EvidenceContradiction (weight 0.1).
//
// RED (pre-fix): this test fails — got 1 flag (not 0), and confidence for
// both A and B decreases from 0.5 (BayesianUpdate(0.5, 0.1) ≈ 0.12).
func TestWrite_TwoTargetsSameRelType_NoFabricatedContradiction(t *testing.T) {
	ws := [8]byte{1}
	engramID := [16]byte{9}
	a := [16]byte{11}
	b := [16]byte{12}

	store := newFakeContraConfStore(a, b, 0.5, 0.5)
	contraW := NewContradictWorker(store)
	confW := NewConfidenceWorker(store)

	var events []ContradictionEvent
	item := ContradictItem{
		WS:       ws,
		EngramID: engramID,
		Associations: []ContradictAssoc{
			// One ordinary write: engram -> A and engram -> B, both under
			// RelType 5 ("references"). Same relation type, different
			// targets — an entirely ordinary shape, not a contradiction.
			{EngramID: engramID, TargetID: a, TargetHash: 111, RelType: 5},
			{EngramID: engramID, TargetID: b, TargetHash: 222, RelType: 5},
		},
		OnFound: func(ev ContradictionEvent) { events = append(events, ev) },
	}
	if err := contraW.processBatch(context.Background(), []ContradictItem{item}); err != nil {
		t.Fatalf("ContradictWorker.processBatch: %v", err)
	}

	// t.Errorf (not Fatalf) so that, when this goes RED (fabrication restored),
	// execution continues through applyOnFound and the confidence-damage
	// assertions below ALSO fire — demonstrating both halves of the bug (the
	// fabricated flag AND the confidence penalty), not just the flag count.
	if len(events) != 0 {
		t.Errorf("fabricated contradiction (COG-23): got %d OnFound events for an ordinary same-RelType/different-target write, want 0: %+v", len(events), events)
	}
	if len(store.flagged) != 0 {
		t.Errorf("fabricated contradiction persisted (COG-23): FlagContradiction called %d times, want 0: %+v", len(store.flagged), store.flagged)
	}

	applyOnFound(t, confW, ws, events)

	gotA, _ := store.GetConfidence(context.Background(), ws, a)
	gotB, _ := store.GetConfidence(context.Background(), ws, b)
	if gotA != 0.5 {
		t.Errorf("engram A confidence damaged by fabricated contradiction (COG-23): got %v, want unchanged 0.5", gotA)
	}
	if gotB != 0.5 {
		t.Errorf("engram B confidence damaged by fabricated contradiction (COG-23): got %v, want unchanged 0.5", gotB)
	}
}

// TestWrite_GenuineMatrixContradiction_StillFlags is the positive control:
// a genuine Supports(1) <-> Contradicts(2) pair from one engram's
// associations must still flag, at the matrix's severity (1.0), with the
// confidence penalty still applied. This proves the R2 fix does not gut
// real detection — only the fabricated same-RelType rule is removed.
func TestWrite_GenuineMatrixContradiction_StillFlags(t *testing.T) {
	ws := [8]byte{1}
	engramID := [16]byte{9}
	a := [16]byte{11}
	b := [16]byte{12}

	store := newFakeContraConfStore(a, b, 0.5, 0.5)
	contraW := NewContradictWorker(store)
	confW := NewConfidenceWorker(store)

	var events []ContradictionEvent
	item := ContradictItem{
		WS:       ws,
		EngramID: engramID,
		Associations: []ContradictAssoc{
			{EngramID: engramID, TargetID: a, RelType: 1}, // Supports
			{EngramID: engramID, TargetID: b, RelType: 2}, // Contradicts
		},
		OnFound: func(ev ContradictionEvent) { events = append(events, ev) },
	}
	if err := contraW.processBatch(context.Background(), []ContradictItem{item}); err != nil {
		t.Fatalf("ContradictWorker.processBatch: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("genuine Supports/Contradicts pair: got %d flags, want exactly 1: %+v", len(events), events)
	}
	if events[0].Severity != 1.0 {
		t.Errorf("genuine Supports/Contradicts severity = %v, want 1.0 (direct negation, matrix)", events[0].Severity)
	}
	if len(store.flagged) != 1 {
		t.Fatalf("FlagContradiction called %d times, want exactly 1", len(store.flagged))
	}

	applyOnFound(t, confW, ws, events)

	gotA, _ := store.GetConfidence(context.Background(), ws, a)
	gotB, _ := store.GetConfidence(context.Background(), ws, b)
	if gotA >= 0.5 {
		t.Errorf("engram A confidence did not drop after a genuine contradiction: got %v, want < 0.5", gotA)
	}
	if gotB >= 0.5 {
		t.Errorf("engram B confidence did not drop after a genuine contradiction: got %v, want < 0.5", gotB)
	}
}

// TestWrite_GenuinePrecededByFollowedByContradiction_StillFlags covers the
// matrix's second pair (0.9, "incompatible relation types") — count and
// severity asserted, same as the direct-negation control above.
func TestWrite_GenuinePrecededByFollowedByContradiction_StillFlags(t *testing.T) {
	ws := [8]byte{1}
	engramID := [16]byte{9}
	a := [16]byte{11}
	b := [16]byte{12}

	store := newFakeContraConfStore(a, b, 0.5, 0.5)
	contraW := NewContradictWorker(store)

	var events []ContradictionEvent
	item := ContradictItem{
		WS:       ws,
		EngramID: engramID,
		Associations: []ContradictAssoc{
			{EngramID: engramID, TargetID: a, RelType: 8}, // PrecededBy
			{EngramID: engramID, TargetID: b, RelType: 9}, // FollowedBy
		},
		OnFound: func(ev ContradictionEvent) { events = append(events, ev) },
	}
	if err := contraW.processBatch(context.Background(), []ContradictItem{item}); err != nil {
		t.Fatalf("ContradictWorker.processBatch: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("genuine PrecededBy/FollowedBy pair: got %d flags, want exactly 1: %+v", len(events), events)
	}
	if events[0].Severity != 0.9 {
		t.Errorf("genuine PrecededBy/FollowedBy severity = %v, want 0.9 (matrix)", events[0].Severity)
	}
}

// TestWrite_ExplicitContradictsLink_StillFlags mirrors the shape
// Engine.Link builds at engine.go's explicit-RelContradicts-link path: a
// pseudo pair {RelContradicts(2) -> target, RelSupports(1) -> source}. This
// is the "an explicit user 'X contradicts Y' link" case named in COG-23 and
// must keep flagging at severity 1.0 through the same matrix path.
func TestWrite_ExplicitContradictsLink_StillFlags(t *testing.T) {
	ws := [8]byte{1}
	source := [16]byte{9}
	target := [16]byte{10}

	store := newFakeContraConfStore(source, target, 0.5, 0.5)
	contraW := NewContradictWorker(store)

	var events []ContradictionEvent
	item := ContradictItem{
		WS:          ws,
		EngramID:    source,
		ConceptHash: 0,
		Associations: []ContradictAssoc{
			{EngramID: source, TargetID: target, TargetHash: 0, RelType: 2}, // RelContradicts
			{EngramID: target, TargetID: source, TargetHash: 0, RelType: 1}, // RelSupports
		},
		OnFound: func(ev ContradictionEvent) { events = append(events, ev) },
	}
	if err := contraW.processBatch(context.Background(), []ContradictItem{item}); err != nil {
		t.Fatalf("ContradictWorker.processBatch: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("explicit RelContradicts link: got %d flags, want exactly 1: %+v", len(events), events)
	}
	if events[0].Severity != 1.0 {
		t.Errorf("explicit RelContradicts link severity = %v, want 1.0", events[0].Severity)
	}
}
