package embed

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// The reproducible half of the noise-baseline story (#719).
//
// noiseBaselineRegistry carries a per-model constant b = μ+kσ of the cosine
// distribution over *unrelated* text. Those constants decide when recall
// abstains, so where they come from matters as much as what they are: a value
// derived from one deployment's corpus characterizes that corpus as much as it
// characterizes the model.
//
// This file supplies the model-only half — a fixed corpus that ships with the
// repo, and the statistic computed over it — so a baseline can be re-derived by
// anyone, for any embedder, and reviewed as a diff rather than trusted.

//go:embed anisotropy_corpus.jsonl
var anisotropyCorpusRaw []byte

// CorpusEntry is one line of the calibration corpus.
//
// Domain exists to be excluded, not to be embedded: see AnisotropyBaseline.
type CorpusEntry struct {
	Domain string `json:"domain"`
	Text   string `json:"text"`
}

// AnisotropyCorpus returns the embedded calibration corpus in file order.
//
// The corpus is deliberately mundane general knowledge across unrelated
// domains, with no proper nouns tied to any organization, so that it can ship
// in a public repo and so that no deployment's own vocabulary leaks into a
// constant that ships to every deployment.
func AnisotropyCorpus() ([]CorpusEntry, error) {
	var out []CorpusEntry
	sc := bufio.NewScanner(bytes.NewReader(anisotropyCorpusRaw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e CorpusEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("calibration corpus: %w", err)
		}
		if e.Domain == "" || e.Text == "" {
			return nil, fmt.Errorf("calibration corpus: entry missing domain or text")
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("calibration corpus: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("calibration corpus is empty")
	}
	return out, nil
}

// AnisotropyResult is a model characterization plus the context needed to
// reproduce or invalidate it.
//
// The provenance fields are not decoration. Two callers can both honestly say
// "bge-small-en-v1.5" and be entitled to different constants: pooling, whether
// an instruction prefix is prepended, truncation, and quantization all move the
// distribution. A registry keyed on a model name alone silently merges them.
type AnisotropyResult struct {
	Model      string  `json:"model"`
	Dim        int     `json:"dim"`
	NPairs     int     `json:"n_pairs"`
	Mean       float64 `json:"mean"`
	StdDev     float64 `json:"stddev"`
	K          float64 `json:"k"`
	Baseline   float64 `json:"baseline"` // μ + kσ — the value a registry entry carries
	CorpusSize int     `json:"corpus_size"`
	// Percentiles of the unrelated-pair distribution, for judging how much of
	// it a given k actually excludes.
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// AnisotropyBaseline computes μ+kσ of the cosine distribution over unrelated
// pairs of the corpus.
//
// vectors must be parallel to entries. Vectors need not be normalized; cosine
// normalizes internally, which also means the result is invariant to whether a
// backend returns unit vectors (Ollama's two embed endpoints differ on exactly
// this for the same model and input).
//
// Same-domain pairs are EXCLUDED. A corpus of N sentences over D domains does
// not yield N(N-1)/2 unrelated pairs — the within-domain pairs are related by
// construction and sit measurably higher, so counting them inflates the
// baseline and makes recall abstain more than the operator asked for. The
// contamination grows with the square of per-domain depth, so it cannot be
// dismissed as small in general even where it is small for one corpus.
func AnisotropyBaseline(entries []CorpusEntry, vectors [][]float32, k float64) (AnisotropyResult, error) {
	if len(entries) != len(vectors) {
		return AnisotropyResult{}, fmt.Errorf("anisotropy: %d entries but %d vectors", len(entries), len(vectors))
	}
	if len(entries) < 2 {
		return AnisotropyResult{}, fmt.Errorf("anisotropy: need at least 2 corpus entries, got %d", len(entries))
	}
	dim := len(vectors[0])
	unit := make([][]float64, len(vectors))
	for i, v := range vectors {
		if len(v) != dim {
			return AnisotropyResult{}, fmt.Errorf("anisotropy: vector %d has dim %d, expected %d", i, len(v), dim)
		}
		u, err := normalize(v)
		if err != nil {
			return AnisotropyResult{}, fmt.Errorf("anisotropy: vector %d: %w", i, err)
		}
		unit[i] = u
	}

	var cosines []float64
	for i := 0; i < len(unit); i++ {
		for j := i + 1; j < len(unit); j++ {
			if entries[i].Domain == entries[j].Domain {
				continue // related by construction
			}
			cosines = append(cosines, dot(unit[i], unit[j]))
		}
	}
	if len(cosines) == 0 {
		return AnisotropyResult{}, fmt.Errorf("anisotropy: corpus has no cross-domain pairs (every entry shares one domain)")
	}

	mean := 0.0
	for _, c := range cosines {
		mean += c
	}
	mean /= float64(len(cosines))
	varsum := 0.0
	for _, c := range cosines {
		d := c - mean
		varsum += d * d
	}
	sd := math.Sqrt(varsum / float64(len(cosines)))

	sorted := append([]float64(nil), cosines...)
	sort.Float64s(sorted)

	return AnisotropyResult{
		Dim:        dim,
		NPairs:     len(cosines),
		Mean:       mean,
		StdDev:     sd,
		K:          k,
		Baseline:   mean + k*sd,
		CorpusSize: len(entries),
		P95:        percentile(sorted, 0.95),
		P99:        percentile(sorted, 0.99),
		Max:        sorted[len(sorted)-1],
	}, nil
}

func normalize(v []float32) ([]float64, error) {
	sum := 0.0
	out := make([]float64, len(v))
	for i, x := range v {
		f := float64(x)
		out[i] = f
		sum += f * f
	}
	n := math.Sqrt(sum)
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return nil, fmt.Errorf("zero or non-finite vector")
	}
	for i := range out {
		out[i] /= n
	}
	return out, nil
}

func dot(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// percentile returns the p-quantile of an ascending slice using nearest-rank.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}
