package rankorder

import "testing"

func TestBefore_NewestFirst(t *testing.T) {
	older := [16]byte{0x01, 0x00}
	newer := [16]byte{0x01, 0x01}
	if !Before(newer, older) || Before(older, newer) {
		t.Fatalf("tie rule must put the newer (higher) ULID first")
	}
	if Before(newer, newer) {
		t.Fatalf("Before must be irreflexive")
	}
	if !NewestFirst() {
		t.Fatalf("shipped build must scan newest-first")
	}
}
