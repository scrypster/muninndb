package embed

import (
	"math"
	"testing"
)

// TestMeanPool verifies mean pooling logic against a hand-calculated reference.
func TestMeanPool(t *testing.T) {
	// 3 tokens, dim=2.  Token 0 and 2 are active (mask=1), token 1 is padding (mask=0).
	hidden := []float32{
		1, 2,   // token 0
		10, 10, // token 1 (padding — excluded)
		3, 4,   // token 2
	}
	mask := []int64{1, 0, 1}

	got := meanPool(hidden, mask, 3, 2)

	// Expected: mean of tokens 0 and 2 = ([1+3]/2, [2+4]/2) = (2, 3)
	if diff := abs32(got[0] - 2.0); diff > 1e-5 {
		t.Errorf("dim 0: want 2.0, got %.6f", got[0])
	}
	if diff := abs32(got[1] - 3.0); diff > 1e-5 {
		t.Errorf("dim 1: want 3.0, got %.6f", got[1])
	}
}

// TestMeanPoolAllPadding verifies that an all-zero mask returns a zero vector without panic.
func TestMeanPoolAllPadding(t *testing.T) {
	hidden := []float32{1, 2, 3, 4}
	mask := []int64{0, 0}
	got := meanPool(hidden, mask, 2, 2)
	if got[0] != 0 || got[1] != 0 {
		t.Errorf("all-padding: expected [0 0], got %v", got)
	}
}

// TestL2Normalize verifies that the output vector has unit norm.
func TestL2Normalize(t *testing.T) {
	v := []float32{3, 4} // L2 norm = 5; expected output [0.6, 0.8]
	l2Normalize(v)

	norm := computeNorm(v)
	if diff := math.Abs(norm - 1.0); diff > 1e-6 {
		t.Errorf("expected unit norm, got %.8f", norm)
	}
	if diff := abs32(v[0] - 0.6); diff > 1e-5 {
		t.Errorf("v[0]: want 0.6, got %.6f", v[0])
	}
	if diff := abs32(v[1] - 0.8); diff > 1e-5 {
		t.Errorf("v[1]: want 0.8, got %.6f", v[1])
	}
}

// TestL2NormalizeZero verifies that a zero vector doesn't panic or produce NaN/Inf.
func TestL2NormalizeZero(t *testing.T) {
	v := []float32{0, 0, 0}
	l2Normalize(v)
	for i, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Errorf("v[%d] is NaN/Inf after zero-norm l2Normalize", i)
		}
	}
}

// --- clsPool tests (bge-small-en-v1.5 CLS-token extraction) ---

func TestClsPool(t *testing.T) {
	// 4 tokens, dim=3. CLS token is at position 0.
	hidden := []float32{
		1, 2, 3, // token 0: CLS — must be extracted
		9, 9, 9, // token 1: unused
		8, 8, 8, // token 2: unused
		7, 7, 7, // token 3: unused
	}
	got := clsPool(hidden, 3)

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("cls token: got %v, want [1 2 3]", got)
	}
	// Verify independence: modifying got must not affect hidden.
	got[0] = 99
	if hidden[0] == 99 {
		t.Error("clsPool returned a slice sharing the hidden buffer (must copy)")
	}
}

func TestClsPool_MinimalSequence(t *testing.T) {
	hidden := []float32{0.5, -0.5, 1.0}
	got := clsPool(hidden, 3)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []float32{0.5, -0.5, 1.0} {
		if abs32(got[i]-want) > 1e-6 {
			t.Errorf("dim %d: got %v, want %v", i, got[i], want)
		}
	}
}

func TestClsPool_AfterL2Normalize(t *testing.T) {
	hidden := []float32{
		3, 0, 4, // CLS: norm = 5 → normalized [0.6, 0, 0.8]
		1, 1, 1, // irrelevant padding token
	}
	vec := clsPool(hidden, 3)
	l2Normalize(vec)

	norm := computeNorm(vec)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Errorf("expected unit norm after CLS-pool + L2-normalize, got %.8f", norm)
	}
	if abs32(vec[0]-0.6) > 1e-5 {
		t.Errorf("dim 0: got %v, want 0.6", vec[0])
	}
	if abs32(vec[2]-0.8) > 1e-5 {
		t.Errorf("dim 2: got %v, want 0.8", vec[2])
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

func computeNorm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}
