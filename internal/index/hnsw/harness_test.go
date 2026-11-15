package hnsw

// Recall harness: the neighbour-selection rule (select.go) against the
// pre-fix nearest-only rule (Index.legacy), on synthetic corpora with
// controlled cluster structure, graded against exact brute-force search.
//
// Two gradings are printed side by side for every cell:
//
//	strict  |returned ∩ exact top-k| / k
//	jitter  a returned vector counts when its true distance is within
//	        jitterEps of the exact k-th distance
//
// The jitter grading is the POST HOC amendment to the pre-registered rule
// (decision record, "closed islands"): it was adopted after the strict rule
// failed on near-identical clusters. jitterEps is a measured quantity, not a
// tuned one: the smallest cosine distance between the same text embedded
// alone and inside a batch on the bundled bge-small model,
// 5.14e-4 (n = 72 pairs; median 7.0e-4, max 1.24e-3), measured by
// internal/plugin/embed TestMeasure_BatchCompositionJitter, rounded down.
// Two stored vectors closer than that cannot be ordered by anything the
// embedder guarantees.
//
// The fast variant runs in the default suite. The full table:
//
//	HNSW_HARNESS=1 go test -tags localassets -run TestHarness_Full -v ./internal/index/hnsw/
//
// optional HNSW_SIZES=2000,12000 HNSW_SHAPES=uniform,topics HNSW_NQ=200.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const jitterEps = 5.0e-4

type harnessCell struct {
	class                    string
	r10, r10j, r50, r50j     float64
	distPerQuery             float64
	n                        float64
	build                    time.Duration
	heapMB, deg0             float64
	exactPasses, closedNodes int
}

func (h *harnessCell) mean() harnessCell {
	o := *h
	o.r10, o.r10j, o.r50, o.r50j, o.distPerQuery = h.r10/h.n, h.r10j/h.n, h.r50/h.n, h.r50j/h.n, h.distPerQuery/h.n
	return o
}

// harnessBuild builds c with the given arm and reports build time, the heap
// retained by the index and the mean layer-0 out-degree.
func harnessBuild(t testing.TB, c *corpus, legacy bool) (*Index, time.Duration, float64, float64) {
	t.Helper()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var ws [8]byte
	copy(ws[:], "HARNESS1")
	start := time.Now()
	idx := New(nil, ws) // no database: build cost is the graph, not Pebble
	idx.legacy = legacy
	for i, v := range c.vecs {
		idx.Insert(c.ids[i], v)
	}
	bt := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	tot := 0
	for _, n := range idx.nodes {
		tot += len(n.layers[0])
	}
	heap := float64(int64(m1.HeapAlloc)-int64(m0.HeapAlloc)) / 1e6
	return idx, bt, heap, float64(tot) / float64(len(idx.nodes))
}

func harnessEval(idx *Index, c *corpus, qs []query) map[string]*harnessCell {
	vm := c.vecMap()
	out := map[string]*harnessCell{}
	for _, q := range qs {
		cell := out[q.class]
		if cell == nil {
			cell = &harnessCell{class: q.class}
			out[q.class] = cell
		}
		var dist int
		g10 := idx.search(q.q, 10, &dist)
		g50 := idx.search(q.q, 50, nil)
		w50, d50 := exactTopKD(c, q.q, 50, nil)
		w10, k10, k50 := w50[:10], d50[9], d50[len(d50)-1]
		cell.r10 += recallStrict(g10, w10)
		cell.r10j += recallWithin(c, vm, q.q, g10, 10, k10, jitterEps)
		cell.r50 += recallStrict(g50, w50)
		cell.r50j += recallWithin(c, vm, q.q, g50, 50, k50, jitterEps)
		cell.distPerQuery += float64(dist)
		cell.n++
	}
	return out
}

func harnessHeader() string {
	return fmt.Sprintf("%-6s %-12s %-14s %-7s %-6s %-6s %-6s %-6s %-7s %-8s %-8s %-6s %-6s %-6s",
		"N", "shape", "class", "arm", "r@10", "r@10j", "r@50", "r@50j", "dist/q", "build_s", "vec/s", "heapMB", "deg0", "closed")
}

func harnessRow(n int, shape, arm string, c harnessCell) string {
	return fmt.Sprintf("%-6d %-12s %-14s %-7s %-6.3f %-6.3f %-6.3f %-6.3f %-7.0f %-8.2f %-8.0f %-6.1f %-6.1f %-6d",
		n, shape, c.class, arm, c.r10, c.r10j, c.r50, c.r50j, c.distPerQuery, c.build.Seconds(),
		float64(n)/c.build.Seconds(), c.heapMB, c.deg0, c.closedNodes)
}

// runHarness evaluates every (size, shape) for both arms and returns the mean
// cells keyed "N/shape/class/arm".
func runHarness(t testing.TB, dim int, sizes []int, shapes []string, nq int) map[string]harnessCell {
	res := map[string]harnessCell{}
	fmt.Println("HARNESS " + harnessHeader())
	for _, n := range sizes {
		for _, sh := range shapes {
			c := makeCorpus(sh, dim, n, 1000+int64(n))
			qs := makeQueries(c, nq, 77)
			for _, arm := range []string{"current", "new"} {
				idx, bt, heap, deg := harnessBuild(t, c, arm == "current")
				closed := closedAgainstEntry(idx)
				cells := harnessEval(idx, c, qs)
				classes := make([]string, 0, len(cells))
				for k := range cells {
					classes = append(classes, k)
				}
				sort.Strings(classes)
				for _, cl := range classes {
					m := cells[cl].mean()
					m.build, m.heapMB, m.deg0, m.closedNodes = bt, heap, deg, closed
					res[fmt.Sprintf("%d/%s/%s/%s", n, sh, cl, arm)] = m
					fmt.Println("HARNESS " + harnessRow(n, sh, arm, m))
				}
				runtime.KeepAlive(idx)
			}
		}
	}
	return res
}

// TestHarness_Fast is the default-suite variant: small N, low dimension. It
// asserts the amended rule's recall halves on what it can afford to build:
// clustered shapes reach jitter-equivalent recall@10 >= 0.95, and uniform and
// plain-topic data are no more than one point below the current rule.
func TestHarness_Fast(t *testing.T) {
	const n = 400
	res := runHarness(t, 64, []int{n}, []string{"uniform", "topics", "dup50"}, 20)
	get := func(shape, class, arm string) harnessCell {
		c, ok := res[fmt.Sprintf("%d/%s/%s/%s", n, shape, class, arm)]
		if !ok {
			t.Fatalf("missing cell %s/%s/%s", shape, class, arm)
		}
		return c
	}
	for _, cl := range []string{"ordinary", "cluster-topic", "cluster-aimed"} {
		if c := get("dup50", cl, "new"); c.r10j < 0.95 {
			t.Errorf("dup50 %s: jitter-equivalent recall@10 %.3f < 0.95", cl, c.r10j)
		}
	}
	if c := get("topics", "ordinary", "new"); c.r10j < 0.95 {
		t.Errorf("topics: jitter-equivalent recall@10 %.3f < 0.95", c.r10j)
	}
	for _, sh := range []string{"uniform", "topics"} {
		cur, nw := get(sh, "ordinary", "current"), get(sh, "ordinary", "new")
		if nw.r10 < cur.r10-0.01 || nw.r50 < cur.r50-0.01 {
			t.Errorf("%s: new rule recall@10/@50 %.3f/%.3f more than 1 point below current %.3f/%.3f",
				sh, nw.r10, nw.r50, cur.r10, cur.r50)
		}
		if nw.closedNodes != 0 {
			t.Errorf("%s: %d closed nodes under the new rule", sh, nw.closedNodes)
		}
	}
}

func envInts(name string, def []int) []int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	var out []int
	for _, x := range strings.Split(s, ",") {
		v, err := strconv.Atoi(strings.TrimSpace(x))
		if err == nil {
			out = append(out, v)
		}
	}
	return out
}

// TestHarness_Full prints the full table (dim 384, the bundled model's
// dimension; package M / M0 / efConstruction / efSearch; plain Search). It
// asserts nothing: the amended rule is graded from its output, and an
// independent review reads both gradings.
func TestHarness_Full(t *testing.T) {
	if os.Getenv("HNSW_HARNESS") == "" {
		t.Skip("set HNSW_HARNESS=1 for the full recall table")
	}
	sizes := envInts("HNSW_SIZES", []int{2000, 12000})
	shapes := []string{"uniform", "topics", "topicsloose", "dup10", "dup34", "dup50", "dup500", "mix"}
	if s := os.Getenv("HNSW_SHAPES"); s != "" {
		shapes = strings.Split(s, ",")
	}
	nq := envInts("HNSW_NQ", []int{200})[0]
	runHarness(t, 384, sizes, shapes, nq)
}

// TestHarness_RetiredTrigger re-measures SearchLive's "walked through ef
// retired nodes" exact-pass trigger on the new graph: recall@10 against exact
// search over LIVE vectors, how often the exact pass fired, and distances per
// query, with the trigger on and off, plus the current rule for reference.
func TestHarness_RetiredTrigger(t *testing.T) {
	if os.Getenv("HNSW_HARNESS") == "" {
		t.Skip("set HNSW_HARNESS=1")
	}
	const n, dim = 12000, 384
	type cfg struct {
		name   string
		frac   float64
		chains int
	}
	fmt.Printf("RETIRED %-24s %-12s %-12s %-6s %-9s %-7s\n", "corpus", "arm", "class", "r@10", "exactPass", "dist/q")
	for _, cf := range []cfg{{"3000-retired(25%)", 0.25, 0}, {"90%-retired", 0.90, 0}, {"ten-500-chains", 0, 10}} {
		c := makeCorpus("topics", dim, n-cf.chains*500, 4242)
		r := newRand(4243)
		var chainBases [][]float32
		for k := 0; k < cf.chains; k++ {
			b := cNear(r, c.vecs[r.Intn(len(c.vecs))], 0.3)
			chainBases = append(chainBases, b)
			for i := 0; i < 500; i++ {
				c.vecs = append(c.vecs, cNear(r, b, 0.035))
				c.inCluster = append(c.inCluster, true)
				c.topic = append(c.topic, -1)
				var id [16]byte
				r.Read(id[:])
				c.ids = append(c.ids, id)
			}
		}
		retired := map[[16]byte]bool{}
		for i := range c.vecs {
			if c.inCluster[i] || (cf.frac > 0 && r.Float64() < cf.frac) {
				retired[c.ids[i]] = true
			}
		}
		perm := r.Perm(len(c.vecs))
		var qs []query
		for i := 0; i < 200; i++ {
			j := r.Intn(len(c.vecs))
			for c.inCluster[j] {
				j = r.Intn(len(c.vecs))
			}
			qs = append(qs, query{"ordinary", cNear(r, c.vecs[j], 0.6)})
		}
		for _, b := range chainBases {
			for i := 0; i < 20; i++ {
				qs = append(qs, query{"chain-aimed", cNear(r, b, 0.5)})
			}
		}
		live := func(i int) bool { return !retired[c.ids[i]] }
		isRet := func(id [16]byte) bool { return retired[id] }
		for _, arm := range []string{"current", "new", "new-noBeam"} {
			var ws [8]byte
			copy(ws[:], "HARNESS2")
			idx := New(nil, ws)
			idx.legacy = arm == "current"
			idx.noRetiredBeamTrigger = arm == "new-noBeam"
			for _, i := range perm {
				idx.Insert(c.ids[i], c.vecs[i])
			}
			type acc struct{ r, ex, d, n float64 }
			res := map[string]*acc{}
			for _, q := range qs {
				got, _, st, err := idx.SearchLive(context.Background(), q.q, 10, isRet, 16)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := exactTopK(c, q.q, 10, live)
				a := res[q.class]
				if a == nil {
					a = &acc{}
					res[q.class] = a
				}
				a.r += recallStrict(got, want)
				if st.ExactScan {
					a.ex++
				}
				a.d += float64(st.Distances)
				a.n++
			}
			for _, cl := range []string{"ordinary", "chain-aimed"} {
				if a := res[cl]; a != nil {
					fmt.Printf("RETIRED %-24s %-12s %-12s %-6.3f %4.0f/%-4.0f %-7.0f\n", cf.name, arm, cl, a.r/a.n, a.ex, a.n, a.d/a.n)
				}
			}
		}
	}
}
