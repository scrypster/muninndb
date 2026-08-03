package embed

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func TestAnisotropyCorpus_LoadsAndIsDiverse(t *testing.T) {
	entries, err := AnisotropyCorpus()
	if err != nil {
		t.Fatalf("AnisotropyCorpus: %v", err)
	}
	if len(entries) < 100 {
		t.Errorf("corpus has %d entries; too small for a stable sigma", len(entries))
	}
	domains := map[string]int{}
	for _, e := range entries {
		domains[e.Domain]++
	}
	if len(domains) < 10 {
		t.Errorf("corpus spans %d domains; a baseline over few domains characterizes the domains, not the model", len(domains))
	}
	// Every domain should be represented comparably, or the cross-domain pair
	// population is dominated by whichever domain is over-represented.
	for d, n := range domains {
		if n < 2 {
			t.Errorf("domain %q has %d entries", d, n)
		}
	}
	// Duplicate text would be a same-pair masquerading as a cross-domain pair.
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Text] {
			t.Errorf("duplicate corpus text: %q", e.Text)
		}
		seen[e.Text] = true
	}
}

// synthetic vectors: same-domain entries are made deliberately similar, so a
// baseline that fails to exclude them is measurably inflated.
func syntheticVectors(entries []CorpusEntry, seed int64) [][]float32 {
	r := rand.New(rand.NewSource(seed))
	const dim = 64
	base := map[string][]float32{}
	out := make([][]float32, len(entries))
	for i, e := range entries {
		if _, ok := base[e.Domain]; !ok {
			b := make([]float32, dim)
			for j := range b {
				b[j] = float32(r.NormFloat64())
			}
			base[e.Domain] = b
		}
		v := make([]float32, dim)
		for j := range v {
			// heavy shared component within a domain, small individual noise
			v[j] = base[e.Domain][j] + float32(r.NormFloat64())*0.2
		}
		out[i] = v
	}
	return out
}

// The property the exclusion exists for: including same-domain pairs raises the
// baseline. If AnisotropyBaseline ever stops excluding them, this fails.
func TestAnisotropyBaseline_ExcludesSameDomainPairs(t *testing.T) {
	entries, err := AnisotropyCorpus()
	if err != nil {
		t.Fatal(err)
	}
	vecs := syntheticVectors(entries, 42)

	got, err := AnisotropyBaseline(entries, vecs, 1.3)
	if err != nil {
		t.Fatalf("AnisotropyBaseline: %v", err)
	}

	// Recompute deliberately WITHOUT the exclusion. Giving every entry its own
	// unique domain means no pair is ever same-domain, so all N(N-1)/2 pairs
	// are counted — which is exactly what a corpus shipped without domain
	// labels would produce.
	flat := make([]CorpusEntry, len(entries))
	for i, e := range entries {
		flat[i] = CorpusEntry{Domain: fmt.Sprintf("unique-%d", i), Text: e.Text}
	}
	unfiltered, err := AnisotropyBaseline(flat, vecs, 1.3)
	if err != nil {
		t.Fatal(err)
	}

	if !(unfiltered.Mean > got.Mean) {
		t.Errorf("same-domain contamination did not raise the mean: filtered=%.4f unfiltered=%.4f\n"+
			"(the exclusion is what keeps related pairs out of an 'unrelated pair' statistic)",
			got.Mean, unfiltered.Mean)
	}
	if got.NPairs >= unfiltered.NPairs {
		t.Errorf("filtered pair count %d should be below unfiltered %d", got.NPairs, unfiltered.NPairs)
	}
}

func TestAnisotropyBaseline_IsScaleInvariant(t *testing.T) {
	entries, err := AnisotropyCorpus()
	if err != nil {
		t.Fatal(err)
	}
	vecs := syntheticVectors(entries, 7)
	a, err := AnisotropyBaseline(entries, vecs, 1.3)
	if err != nil {
		t.Fatal(err)
	}
	// Scale every vector by 10 — cosine must not move. This is the property
	// that makes the result independent of whether a backend returns unit
	// vectors (Ollama's /api/embed and /api/embeddings differ on exactly this).
	scaled := make([][]float32, len(vecs))
	for i, v := range vecs {
		s := make([]float32, len(v))
		for j, x := range v {
			s[j] = x * 10
		}
		scaled[i] = s
	}
	b, err := AnisotropyBaseline(entries, scaled, 1.3)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(a.Baseline-b.Baseline) > 1e-9 {
		t.Errorf("baseline moved under scaling: %.12f vs %.12f", a.Baseline, b.Baseline)
	}
}

func TestAnisotropyBaseline_KIsMonotone(t *testing.T) {
	entries, err := AnisotropyCorpus()
	if err != nil {
		t.Fatal(err)
	}
	vecs := syntheticVectors(entries, 11)
	var prev float64 = -2
	for _, k := range []float64{0, 1.0, 1.3, 2.0, 3.0} {
		r, err := AnisotropyBaseline(entries, vecs, k)
		if err != nil {
			t.Fatal(err)
		}
		if r.Baseline <= prev {
			t.Errorf("baseline not increasing in k: k=%v gave %.4f, previous %.4f", k, r.Baseline, prev)
		}
		prev = r.Baseline
	}
}

func TestAnisotropyBaseline_Rejects(t *testing.T) {
	entries := []CorpusEntry{{Domain: "a", Text: "x"}, {Domain: "b", Text: "y"}}
	if _, err := AnisotropyBaseline(entries, [][]float32{{1, 0}}, 1.3); err == nil {
		t.Error("mismatched entry/vector counts must be rejected")
	}
	if _, err := AnisotropyBaseline(entries, [][]float32{{1, 0}, {1, 0, 0}}, 1.3); err == nil {
		t.Error("ragged vector dimensions must be rejected")
	}
	if _, err := AnisotropyBaseline(entries, [][]float32{{0, 0}, {1, 0}}, 1.3); err == nil {
		t.Error("a zero vector has no direction and must be rejected, not silently scored")
	}
	single := []CorpusEntry{{Domain: "a", Text: "x"}, {Domain: "a", Text: "y"}}
	if _, err := AnisotropyBaseline(single, [][]float32{{1, 0}, {0, 1}}, 1.3); err == nil {
		t.Error("a corpus with no cross-domain pairs must be rejected rather than return a baseline over zero pairs")
	}
}

// Rescale is what consumes the baseline; pin the relationship so a change to
// one is not made without looking at the other.
func TestAnisotropyBaseline_FeedsRescale(t *testing.T) {
	entries, err := AnisotropyCorpus()
	if err != nil {
		t.Fatal(err)
	}
	r, err := AnisotropyBaseline(entries, syntheticVectors(entries, 3), 1.3)
	if err != nil {
		t.Fatal(err)
	}
	if Rescale(r.Baseline, r.Baseline) != 0 {
		t.Error("a cosine exactly at the baseline must rescale to 0")
	}
	if got := Rescale(1.0, r.Baseline); math.Abs(got-1.0) > 1e-9 {
		t.Errorf("a perfect cosine must survive rescaling: got %v", got)
	}
}
