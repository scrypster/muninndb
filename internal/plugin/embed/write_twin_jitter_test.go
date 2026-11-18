//go:build localassets

package embed

import (
	"context"
	"fmt"
	"math"
	"testing"
)

func twinCos(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return d / math.Sqrt(na*nb)
}

// TestWriteTwin_QuantizationJitterTwinStillClearsThreshold pins the property
// the write-time twin check (COG-36) relies on with the bundled int8 model:
// a text's vector depends slightly on its batch-mates, and an exact copy never
// scores 1.0 — but that jitter is two orders of magnitude smaller than the
// distance between the twin threshold and a clear twin, so a twin embedded in
// a DIFFERENT batch composition than its partner still clears T.
//
// A batch is padded to its longest text, so a text alone and the same text in
// a batch of longer ones are also padded to different lengths. Measured on
// Linux amd64 (the CI runtime), the same-text jitter 1-self was 0.0042 with
// every batch padded to the full window and 0.0054 padded to fit, against a
// margin from a clear twin to T of about 0.055: ten times smaller, not the
// hundred once claimed here. The premise below is stated against that margin
// rather than a fixed cosine, so it fails if jitter ever approaches T.
//
// Invented fixture domain: trail maintenance logs.
func TestWriteTwin_QuantizationJitterTwinStillClearsThreshold(t *testing.T) {
	p := newLaneProvider(t)
	ctx := context.Background()
	thr, ok := WriteTwinHint("bge-small-en-v1.5")
	if !ok {
		t.Fatal("no hint registered for the bundled model")
	}
	// Concept + " " + Content, exactly as the retroactive processor builds it.
	older := "water bar spacing On the ridge spur the crew sets a rock water bar every thirty meters, angled at thirty degrees."
	newer := "water bar spacing The ridge spur crew sets rock water bars every thirty meters, angled thirty degrees."

	filler := func(n, off int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("Trail log %d: the volunteer crew cleared blowdowns past switchback %d and flagged a washout on the lower loop.", i+off, i+off)
		}
		return out
	}
	vecAt := func(batch []string, idx int) []float32 {
		v, err := p.EmbedBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		return v[idx*localModelDim : (idx+1)*localModelDim]
	}

	// Same text, three batch compositions: never assumed identical, never 1.0.
	alone := vecAt([]string{older}, 0)
	inEight := vecAt(append(filler(7, 0), older), 7)
	self := twinCos(alone, inEight)
	t.Logf("self-cosine alone vs in a batch of 8: %.5f", self)

	// The twin pair in matched and mismatched compositions.
	same := twinCos(vecAt([]string{older}, 0), vecAt([]string{newer}, 0))
	if jitter, margin := 1-self, same-thr; jitter*5 > margin {
		t.Fatalf("same-text jitter across batch compositions (%.5f) is more than a fifth of the clear twin's margin over T (%.4f)", jitter, margin)
	}
	cross := twinCos(vecAt(append(filler(7, 0), older), 7), vecAt(append(filler(3, 40), newer), 3))
	t.Logf("twin cosine, both alone: %.5f; different batch compositions: %.5f; T=%.2f", same, cross, thr)
	if same < thr+0.01 {
		t.Fatalf("fixture sanity: the twin pair must clear T with margin when embedded alike (%.4f vs %.2f)", same, thr)
	}
	if cross < thr {
		t.Fatalf("a clear twin (%.4f alike) fell under T=%.2f (%.4f) only because its batch-mates differed", same, thr, cross)
	}
	if d := same - cross; d > 0.005 || d < -0.005 {
		t.Fatalf("batch-composition jitter moved the twin cosine by %.4f (> 0.005)", d)
	}
}
