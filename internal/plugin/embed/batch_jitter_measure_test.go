//go:build localassets

package embed

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"testing"
)

// Invented fixture domain: apiary inspection notes. Nothing here is drawn from
// any real vault.
func jitterTexts() []string {
	frames := []string{"brood", "honey", "pollen", "foundation", "drawn comb"}
	hives := []string{"north row", "orchard stand", "hilltop yard", "creek apiary"}
	var out []string
	for i := 0; i < 24; i++ {
		out = append(out, fmt.Sprintf("Hive %d at the %s: queen seen on frame %d, %s frames %d of 10, "+
			"temperament calm, added a super and checked the mite board (count %d).",
			i+1, hives[i%len(hives)], 1+i%9, frames[i%len(frames)], 3+i%7, i*3%17))
	}
	return out
}

// TestMeasure_BatchCompositionJitter measures how far the SAME text's vector
// moves with its batch-mates on the bundled model: each text embedded alone
// against the same text inside a 24-text batch and inside a shuffled 8-text
// batch. The cosine DISTANCE between the two vectors of one text is the
// resolution below which two stored vectors cannot be told apart by anything
// the embedder guarantees. The HNSW recall harness uses the minimum of this
// distribution as its "within embedder jitter" equivalence
// (internal/index/hnsw/harness_test.go, jitterEps).
//
// A measurement, not a gate: set MUNINN_MEASURE_EMBED_JITTER=1.
func TestMeasure_BatchCompositionJitter(t *testing.T) {
	if os.Getenv("MUNINN_MEASURE_EMBED_JITTER") == "" {
		t.Skip("measurement: set MUNINN_MEASURE_EMBED_JITTER=1")
	}
	p := newLaneProvider(t)
	ctx := context.Background()
	texts := jitterTexts()
	const dim = localModelDim
	alone := make([][]float32, len(texts))
	for i, s := range texts {
		v, err := p.EmbedBatch(ctx, []string{s})
		if err != nil {
			t.Fatal(err)
		}
		alone[i] = v
	}
	full, err := p.EmbedBatch(ctx, texts)
	if err != nil {
		t.Fatal(err)
	}
	perm := rand.New(rand.NewSource(1)).Perm(len(texts))
	small := make([][]float32, len(texts))
	for b := 0; b < len(perm); b += 8 {
		var batch []string
		for _, i := range perm[b : b+8] {
			batch = append(batch, texts[i])
		}
		v, err := p.EmbedBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		for j, i := range perm[b : b+8] {
			small[i] = v[j*dim : (j+1)*dim]
		}
	}
	var dists []float64
	for i := range texts {
		dists = append(dists, 1-jitterCos(alone[i], full[i*dim:(i+1)*dim]))
		dists = append(dists, 1-jitterCos(alone[i], small[i]))
		dists = append(dists, 1-jitterCos(full[i*dim:(i+1)*dim], small[i]))
	}
	sort.Float64s(dists)
	q := func(f float64) float64 { return dists[int(f*float64(len(dists)-1))] }
	t.Logf("same-text distance across batch compositions, n=%d pairs: min=%.2e p10=%.2e median=%.2e p90=%.2e max=%.2e",
		len(dists), dists[0], q(0.10), q(0.5), q(0.9), dists[len(dists)-1])
}

func jitterCos(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
