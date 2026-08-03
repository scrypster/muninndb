package activation

import "testing"

// The in-tree default vault: ACT-R gate 0.1, w_sem 0.6 + w_fts 0.4 = 1.0.
// weakMax = min(2*0.1, 0.5*1.0) = 0.2; strongMin = 0.5.
const (
	testGate    = 0.1
	testCeiling = 1.0
)

func TestRelevanceBandEdges_DefaultVaultReproducesTheInTreeNumbers(t *testing.T) {
	weakMax, strongMin := RelevanceBandEdges(testGate, testCeiling)
	if weakMax != 0.2 {
		t.Errorf("weakMax = %v, want 0.2 (2 x the 0.1 COG-6 ACT-R gate)", weakMax)
	}
	// engine.go's measured note: "NO honest absolute score reaches 0.5 without
	// near-verbatim wording". On default weights the fraction must reproduce
	// that number exactly, or the derivation has drifted from its source.
	if strongMin != 0.5 {
		t.Errorf("strongMin = %v, want 0.5 (0.5 x the 0.6+0.4 default content ceiling)", strongMin)
	}
}

func TestRelevanceBandEdges_MoveWithTheVaultsOwnWeights(t *testing.T) {
	// A vault that halves its content channel gets a proportionally lower
	// strong floor — the point of expressing it as a FRACTION rather than a
	// constant (principle #11).
	_, strongMin := RelevanceBandEdges(testGate, 0.5)
	if strongMin != 0.25 {
		t.Errorf("strongMin = %v, want 0.25 for a 0.5 ceiling", strongMin)
	}
	// A vault with a high gate degenerates to weak/strong rather than
	// producing an inverted moderate band.
	weakMax, strongMin := RelevanceBandEdges(0.4, 0.5)
	if weakMax != strongMin {
		t.Errorf("weakMax %v should clamp to strongMin %v", weakMax, strongMin)
	}
	// weakMax == strongMin == 0.25: the moderate band is empty, and every row
	// falls cleanly to weak or strong rather than into an inverted gap.
	if band, _ := RelevanceBand(0.24, 0.4, 0.5); band != RelevanceWeak {
		t.Errorf("with a degenerate moderate band, 0.24 = %q, want weak", band)
	}
	if band, _ := RelevanceBand(0.25, 0.4, 0.5); band != RelevanceStrong {
		t.Errorf("with a degenerate moderate band, 0.25 = %q, want strong", band)
	}
}

func TestRelevanceBand_Table(t *testing.T) {
	tests := []struct {
		name      string
		abs       float64
		gate      float64
		ceiling   float64
		wantBand  string
		wantBasis string
	}{
		// The measured live case in #773: vector_score 0.09 -> score 1.0. On
		// the absolute scale that row is BELOW COG-26's noise ceiling.
		{"the #773 live case (abs 0.09)", 0.09, testGate, testCeiling, RelevanceWeak, ""},
		{"below the gate", 0.05, testGate, testCeiling, RelevanceWeak, ""},
		{"exactly at the gate", 0.1, testGate, testCeiling, RelevanceWeak, ""},
		{"just under weakMax", 0.199, testGate, testCeiling, RelevanceWeak, ""},
		{"exactly at weakMax", 0.2, testGate, testCeiling, RelevanceModerate, ""},
		{"just under strongMin", 0.499, testGate, testCeiling, RelevanceModerate, ""},
		{"exactly at strongMin", 0.5, testGate, testCeiling, RelevanceStrong, ""},
		{"saturated", 1.0, testGate, testCeiling, RelevanceStrong, ""},
		{"zero", 0.0, testGate, testCeiling, RelevanceWeak, ""},

		{"no content channel", 0.9, testGate, 0, RelevanceUncalibrated, BasisNoContentChannel},
		{"negative content channel", 0.9, testGate, -1, RelevanceUncalibrated, BasisNoContentChannel},
		{"no calibration gate", 0.9, 0, testCeiling, RelevanceUncalibrated, BasisNoCalibrationGate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			band, basis := RelevanceBand(tt.abs, tt.gate, tt.ceiling)
			if band != tt.wantBand || basis != tt.wantBasis {
				t.Errorf("RelevanceBand(%v, %v, %v) = (%q, %q), want (%q, %q)",
					tt.abs, tt.gate, tt.ceiling, band, basis, tt.wantBand, tt.wantBasis)
			}
		})
	}
}

// The anchor argument, pinned: a caller-supplied threshold must NOT be able to
// promote a below-noise row into "moderate". This is the whole reason D4
// anchors on the vault's resolved default gate rather than req.Threshold.
func TestRelevanceBand_CallerThresholdCannotPromoteABelowNoiseRow(t *testing.T) {
	const abs = 0.09 // the #773 live case, below COG-26's ~0.095 noise ceiling
	if band, _ := RelevanceBand(abs, testGate, testCeiling); band != RelevanceWeak {
		t.Fatalf("anchored on the vault gate: band = %q, want weak", band)
	}
	// If the anchor were the caller's threshold:0.01, weakMax would be 0.02 and
	// this exact row would read "moderate" — a row below the model's own
	// measured noise ceiling, dressed up by nothing but a request parameter.
	if band, _ := RelevanceBand(abs, 0.01, testCeiling); band != RelevanceModerate {
		t.Fatalf("guard assumption broken: anchoring on threshold:0.01 gives %q, "+
			"expected the demonstrably-wrong %q this design refuses to ship",
			band, RelevanceModerate)
	}
}

func TestFusionBandBasis(t *testing.T) {
	for mode, want := range map[string]string{
		FusionRRF:         BasisRRFFusion,
		FusionWeightedSum: BasisWeightedSumFusion,
		FusionACTR:        "",
		FusionCGDN:        "",
	} {
		if got := FusionBandBasis(mode); got != want {
			t.Errorf("FusionBandBasis(%q) = %q, want %q", mode, got, want)
		}
	}
}

// The zero value of Admission must be "not measured". Any injector that builds
// a ScoredEngram literal without phase-6 measurements gets the honest label for
// free rather than being banded from a zero-value ScoreComponents.
func TestAdmission_ZeroValueIsInjected(t *testing.T) {
	var a Admission
	if a != AdmissionInjected {
		t.Errorf("zero Admission = %v, want AdmissionInjected", a)
	}
	var row ScoredEngram
	if row.Admission != AdmissionInjected {
		t.Errorf("zero ScoredEngram.Admission = %v, want AdmissionInjected", row.Admission)
	}
}
