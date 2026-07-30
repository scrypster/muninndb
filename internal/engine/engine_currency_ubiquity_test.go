package engine

import "testing"

// TestCurrencyUbiquityCutpoint pins the #11 ubiquity cutpoint: the fraction cut
// (currencyUbiquityRatio*N) floored by an absolute minimum. The floor is what
// fixes the small-vault false-silence — a genuine chain's topic tag (df=4) is 25%
// of a 16-row vault but only 4 memories, so it must NOT be "ambient" — while the
// fraction still filters a genuinely ambient tag on a large vault.
func TestCurrencyUbiquityCutpoint(t *testing.T) {
	cases := []struct {
		name         string
		vaultSize    int64
		wantCutpoint int64
		// (df, isUbiquitous) probes at this vault size.
		probes map[int64]bool
	}{
		{
			// Small vault: the absolute floor dominates (max(0.1*16, 10) = 10),
			// so the df=4 chain tag is kept and a df=15 (~94%) tag is filtered.
			name: "small_vault_floor_dominates", vaultSize: 16, wantCutpoint: currencyUbiquityAbsMin,
			probes: map[int64]bool{4: false, 9: false, 10: true, 15: true},
		},
		{
			// Large vault: the fraction dominates (max(0.1*1357, 10) = 135), so a
			// ~3% content tag (43) is kept and a ~27% ambient tag (374) is filtered.
			name: "large_vault_fraction_dominates", vaultSize: 1357, wantCutpoint: 135,
			probes: map[int64]bool{43: false, 134: false, 135: true, 374: true},
		},
		{
			// Boundary where fraction == floor.
			name: "boundary_fraction_equals_floor", vaultSize: 100, wantCutpoint: currencyUbiquityAbsMin,
			probes: map[int64]bool{9: false, 10: true},
		},
		{
			// Empty/degenerate vault falls back to the floor (never a cutpoint of 0
			// that would call every tag ubiquitous).
			name: "empty_vault_floor", vaultSize: 0, wantCutpoint: currencyUbiquityAbsMin,
			probes: map[int64]bool{1: false, 10: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currencyUbiquityCutpoint(tc.vaultSize); got != tc.wantCutpoint {
				t.Fatalf("currencyUbiquityCutpoint(%d) = %d, want %d", tc.vaultSize, got, tc.wantCutpoint)
			}
			for df, want := range tc.probes {
				if got := currencyIsUbiquitous(df, tc.vaultSize); got != want {
					t.Fatalf("currencyIsUbiquitous(df=%d, N=%d) = %v, want %v", df, tc.vaultSize, got, want)
				}
			}
		})
	}
}
