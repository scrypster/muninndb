package keys

import "testing"

func TestWeightComplement_FullWeightRoundTrips(t *testing.T) {
	for _, w := range []float32{0, 0.3, 0.8, 0.9999999, 1.0} {
		got := WeightFromComplement(WeightComplement(w))
		if diff := got - w; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("weight %v round-trips to %v", w, got)
		}
	}
}
