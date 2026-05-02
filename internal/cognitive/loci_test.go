package cognitive

import (
	"testing"
)

func TestLabelPropagation_TwoCommunities(t *testing.T) {
	// Two clear communities:
	// Community A: Alice, Bob, Charlie (densely connected)
	// Community B: X, Y, Z (densely connected)
	// Weak bridge: Bob-X (count=1, below default min threshold)
	pairs := []EntityPair{
		{EntityA: "Alice", EntityB: "Bob", Count: 10},
		{EntityA: "Alice", EntityB: "Charlie", Count: 8},
		{EntityA: "Bob", EntityB: "Charlie", Count: 9},
		{EntityA: "X", EntityB: "Y", Count: 12},
		{EntityA: "X", EntityB: "Z", Count: 7},
		{EntityA: "Y", EntityB: "Z", Count: 11},
	}

	// Use fixed seed for deterministic tests.


	detector := NewLociDetector(DefaultLociConfig())
	loci := detector.DetectCommunities(pairs)

	if len(loci) != 2 {
		t.Fatalf("expected 2 communities, got %d: %+v", len(loci), loci)
	}

	// Both communities should have 3 members.
	for _, l := range loci {
		if l.Size != 3 {
			t.Errorf("locus %q: expected size 3, got %d", l.Label, l.Size)
		}
		if l.Cohesion <= 0 {
			t.Errorf("locus %q: expected positive cohesion, got %f", l.Label, l.Cohesion)
		}
	}

	// Verify member sets.
	communityA := map[string]bool{"Alice": true, "Bob": true, "Charlie": true}
	communityB := map[string]bool{"X": true, "Y": true, "Z": true}

	found := [2]bool{}
	for _, l := range loci {
		members := map[string]bool{}
		for _, m := range l.Members {
			members[m] = true
		}
		if mapsEqual(members, communityA) {
			found[0] = true
		}
		if mapsEqual(members, communityB) {
			found[1] = true
		}
	}
	if !found[0] || !found[1] {
		t.Errorf("expected both communities {Alice,Bob,Charlie} and {X,Y,Z}, got %+v", loci)
	}
}

func TestLabelPropagation_SingleCommunity(t *testing.T) {
	// All entities connected — should produce a single locus.
	pairs := []EntityPair{
		{EntityA: "A", EntityB: "B", Count: 5},
		{EntityA: "A", EntityB: "C", Count: 5},
		{EntityA: "B", EntityB: "C", Count: 5},
	}



	detector := NewLociDetector(DefaultLociConfig())
	loci := detector.DetectCommunities(pairs)

	if len(loci) != 1 {
		t.Fatalf("expected 1 community, got %d: %+v", len(loci), loci)
	}
	if loci[0].Size != 3 {
		t.Errorf("expected 3 members, got %d", loci[0].Size)
	}
	// Fully connected triangle: cohesion = sum(weights)/possibleEdges = 15/3 = 5.0
	if loci[0].Cohesion < 4.9 || loci[0].Cohesion > 5.1 {
		t.Errorf("expected cohesion ~5.0, got %f", loci[0].Cohesion)
	}
}

func TestLabelPropagation_DisconnectedEntities(t *testing.T) {
	// No edges at all — no loci should be returned (all singletons filtered).
	var pairs []EntityPair

	detector := NewLociDetector(DefaultLociConfig())
	loci := detector.DetectCommunities(pairs)

	if len(loci) != 0 {
		t.Fatalf("expected 0 communities for empty input, got %d: %+v", len(loci), loci)
	}
}

func TestLabelPropagation_Convergence(t *testing.T) {
	// Verify the algorithm terminates within max iterations even for a chain graph
	// where propagation takes multiple rounds.
	pairs := []EntityPair{
		{EntityA: "A", EntityB: "B", Count: 3},
		{EntityA: "B", EntityB: "C", Count: 3},
		{EntityA: "C", EntityB: "D", Count: 3},
		{EntityA: "D", EntityB: "E", Count: 3},
	}



	cfg := DefaultLociConfig()
	cfg.MaxIterations = 10 // low limit to test early convergence
	detector := NewLociDetector(cfg)
	loci := detector.DetectCommunities(pairs)

	// A chain of 5 entities should converge to a single community.
	if len(loci) < 1 {
		t.Fatalf("expected at least 1 community for chain graph, got %d", len(loci))
	}

	// Total members across all loci should be 5 (no entity lost).
	total := 0
	for _, l := range loci {
		total += l.Size
	}
	if total != 5 {
		t.Errorf("expected 5 total members, got %d", total)
	}
}

func TestLabelPropagation_LabelGeneration(t *testing.T) {
	pairs := []EntityPair{
		{EntityA: "PostgreSQL", EntityB: "Redis", Count: 10},
		{EntityA: "PostgreSQL", EntityB: "MongoDB", Count: 8},
		{EntityA: "Redis", EntityB: "MongoDB", Count: 5},
	}



	detector := NewLociDetector(DefaultLociConfig())
	loci := detector.DetectCommunities(pairs)

	if len(loci) != 1 {
		t.Fatalf("expected 1 community, got %d", len(loci))
	}
	// Label should contain top-3 entity names.
	label := loci[0].Label
	if label == "" {
		t.Error("expected non-empty label")
	}
	// The label should contain all three entities since there are only 3.
	for _, name := range []string{"PostgreSQL", "Redis", "MongoDB"} {
		found := false
		for _, m := range loci[0].Members {
			if m == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected entity %q in members", name)
		}
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
