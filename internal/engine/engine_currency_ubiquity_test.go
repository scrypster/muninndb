package engine

import "testing"

// TestCurrencyDeriveUbiquityCutpoint pins the #11 self-derived ubiquity cutpoint:
// it must find the ambient/content break in a vault's OWN tag-df distribution at
// ANY vault size, where the fixed 10% ratio could not. All distributions are
// synthetic df multisets (one entry per distinct tag).
func TestCurrencyDeriveUbiquityCutpoint(t *testing.T) {
	// helper: n copies of v
	rep := func(v int64, n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = v
		}
		return out
	}
	cat := func(parts ...[]int64) []int64 {
		var out []int64
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}

	cases := []struct {
		name string
		dfs  []int64
		want int64
	}{
		{
			// Real-vault-shaped: three ambient tags (374/367/239) far above the
			// content tags (43/31 and a long tail of 1-7). Cutpoint must land in
			// the 239->43 gap so the content tag (43) is KEPT.
			name: "real_vault_shape_ambient_break",
			dfs:  cat([]int64{374, 367, 239, 43, 31}, rep(7, 10), rep(3, 20), rep(1, 20)),
			want: 239,
		},
		{
			// One dominant ambient tag, everything else small content: only the
			// 300 tag is ubiquitous; df=3 content tags are kept.
			name: "single_ambient_tag",
			dfs:  cat([]int64{300}, rep(3, 6), rep(1, 6)),
			want: 300,
		},
		{
			// Small vault: a genuine chain's topic/struct tags at df=4 with a
			// higher filler tag (misc df=11). The 11->4 gap is only 2.75x (< the
			// 3x break) and the 4->1 gap leaves >25% of tags high, so NO ambient
			// class is found -> nothing filtered -> the df=4 chain tags survive.
			// This is the exact small-vault case the fixed ratio false-silenced.
			name: "small_vault_chain_tags_survive",
			dfs:  cat([]int64{11}, rep(4, 2), rep(1, 4)),
			want: currencyNoUbiquity,
		},
		{
			// Degenerate tiny vault: the chain tags (df=4) ARE the highest-df
			// tags, below the absolute floor (5) -> never ubiquitous.
			name: "degenerate_chain_tags_below_floor",
			dfs:  cat(rep(4, 2), rep(1, 4)),
			want: currencyNoUbiquity,
		},
		{
			// Smooth gradient: no multiplicative gap >= 3x anywhere -> no ambient
			// class -> filter nothing.
			name: "smooth_gradient_no_break",
			dfs:  []int64{50, 45, 40, 35, 30, 25, 20, 15, 10, 5},
			want: currencyNoUbiquity,
		},
		{
			// Too few distinct tags to derive a distribution.
			name: "too_few_tags",
			dfs:  []int64{100, 2, 1},
			want: currencyNoUbiquity,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := currencyDeriveUbiquityCutpoint(tc.dfs)
			if got != tc.want {
				t.Fatalf("currencyDeriveUbiquityCutpoint(%v) = %d, want %d", tc.dfs, got, tc.want)
			}
			// Sanity on the "real vault" case: the content tag (43) must be kept
			// and the ambient tag (239) filtered.
			if tc.name == "real_vault_shape_ambient_break" {
				if 43 >= got {
					t.Fatalf("content tag df=43 must be BELOW the cutpoint (%d) — it should be kept", got)
				}
				if 239 < got {
					t.Fatalf("ambient tag df=239 must be AT/ABOVE the cutpoint (%d) — it should be filtered", got)
				}
			}
		})
	}
}
