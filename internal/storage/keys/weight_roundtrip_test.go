package keys

import (
	"encoding/binary"
	"testing"
)

func TestWeightComplement_FullWeightRoundTrips(t *testing.T) {
	for _, w := range []float32{0, 0.3, 0.8, 0.9999999, 1.0} {
		got := WeightFromComplement(WeightComplement(w))
		if diff := got - w; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("weight %v round-trips to %v", w, got)
		}
	}
}

// legacyWeightComplement reproduces the ORIGINAL (pre-fix) encoder verbatim so
// the byte-compatibility contract is pinned against real history, not against
// whatever the current implementation happens to produce. Undefined at 1.0 by
// design — that is the one input the fixed encoder is allowed to differ on.
func legacyWeightComplement(weight float32) [4]byte {
	w := uint32(weight * float32(4294967295))
	c := uint32(4294967295) - w
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], c)
	return buf
}

// The fixed encoder must be BYTE-IDENTICAL to the legacy one for every weight
// in (0,1). The first attempt at the 1.0 fix used a float64 multiplier that
// disagreed with legacy by one integer step at essentially every interior
// weight — which would have made every recomputed-key delete (Hebbian update,
// decay, engram-delete cascade) miss every pre-fix key on every existing
// vault: metadata silently reset, permanent duplicate edges, unbounded decay
// key growth. Caught by adversarial review; this pin makes the regression
// unreintroducible.
func TestWeightComplement_ByteCompatibleWithLegacyKeys(t *testing.T) {
	// Fixed table across the operating range, plus a deterministic sweep.
	for _, w := range []float32{1e-7, 0.001, 0.01, 0.05, 0.1, 0.3, 0.5, 0.7, 0.8, 0.9, 0.95, 0.99, 0.999, 0.99999994} {
		if got, want := WeightComplement(w), legacyWeightComplement(w); got != want {
			t.Errorf("weight %v: fixed encoder %v != legacy bytes %v — pre-fix on-disk keys at this "+
				"weight become unreachable to recomputed-key deletes", w, got, want)
		}
	}
	for i := 1; i < 100000; i++ {
		w := float32(i) / 100001.0
		if got, want := WeightComplement(w), legacyWeightComplement(w); got != want {
			t.Fatalf("sweep: weight %v diverges from legacy bytes", w)
		}
	}
	// And decode->re-encode must be an exact identity across the range decay
	// depends on (the float64 attempt failed this for ~54%% of weights below
	// 0.01).
	for i := 1; i < 100000; i++ {
		w := float32(i) / 100001.0
		if re := WeightComplement(WeightFromComplement(WeightComplement(w))); re != WeightComplement(w) {
			t.Fatalf("decode->re-encode not an identity at weight %v", w)
		}
	}
}
