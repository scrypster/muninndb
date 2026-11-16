package hnsw

// Synthetic vector corpora with controlled cluster structure, shared by the
// island tests and the recall harness. Every vector here is random noise
// shaped by a seeded generator — no text, no embedding of any real content.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func cGauss(r *rand.Rand, dim int, scale float64) []float64 {
	v := make([]float64, dim)
	s := scale / math.Sqrt(float64(dim))
	for i := range v {
		v[i] = r.NormFloat64() * s
	}
	return v
}

func cAdd(a []float64, b ...[]float64) []float64 {
	out := make([]float64, len(a))
	copy(out, a)
	for _, x := range b {
		for i := range out {
			out[i] += x[i]
		}
	}
	return out
}

func cUnit(v []float64) []float32 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	n := math.Sqrt(s)
	out := make([]float32, len(v))
	for i := range v {
		out[i] = float32(v[i] / n)
	}
	return out
}

func c64(v []float32) []float64 {
	o := make([]float64, len(v))
	for i := range v {
		o[i] = float64(v[i])
	}
	return o
}

// cNear returns normalize(base + Gaussian noise of norm ≈ s).
func cNear(r *rand.Rand, base []float32, s float64) []float32 {
	return cUnit(cAdd(c64(base), cGauss(r, len(base), s)))
}

// corpus is a synthetic vault: vectors, ids, and the structure that generated
// them (topic per item, membership of the one large near-identical cluster).
type corpus struct {
	name      string
	dim       int
	vecs      [][]float32
	ids       [][16]byte
	topic     []int
	inCluster []bool
	clusterTp int       // topic hosting the near-identical cluster (-1 none)
	clusterB  []float32 // the cluster's base direction
}

// topicVectors draws n vectors from a topic model with a continuum of
// similarity: global anisotropy + domain + topic + noise, about 40 members per
// topic. With noise 1.1 at dim 384: same topic ≈ 0.69, different ≈ 0.29.
func topicVectors(r *rand.Rand, dim, n int, noise float64) (vecs [][]float32, topic []int) {
	global := cGauss(r, dim, 1.0)
	const nDom = 8
	doms := make([][]float64, nDom)
	for i := range doms {
		doms[i] = cGauss(r, dim, 0.9)
	}
	nTop := n / 40
	if nTop < 4 {
		nTop = 4
	}
	cents := make([][]float64, nTop)
	for i := range cents {
		cents[i] = cAdd(global, doms[i%nDom], cGauss(r, dim, 0.9))
	}
	for i := 0; i < n; i++ {
		t := r.Intn(nTop)
		vecs = append(vecs, cUnit(cAdd(cents[t], cGauss(r, dim, noise))))
		topic = append(topic, t)
	}
	return vecs, topic
}

// makeCorpus builds one shape:
//
//	uniform      i.i.d. random directions
//	topics       ~40-member topics (intra ≈ 0.69, inter ≈ 0.29 at dim 384)
//	topicsloose  the same with twice the noise
//	dupC         topics plus one cluster of C near-identical vectors (pairwise
//	             cosine ≈ 0.9988) inside a topic
//	mix          topics where ~17% of items have a near-duplicate partner
//	             (groups of 2–8, cosine 0.96–0.999) plus one 500-member cluster
func makeCorpus(shape string, dim, n int, seed int64) *corpus {
	r := rand.New(rand.NewSource(seed))
	c := &corpus{name: fmt.Sprintf("%s/N=%d", shape, n), dim: dim, clusterTp: -1}
	addCluster := func(size int) {
		host := r.Intn(len(c.vecs))
		c.clusterTp = c.topic[host]
		c.clusterB = cNear(r, c.vecs[host], 0.3)
		for i := 0; i < size; i++ {
			c.vecs = append(c.vecs, cNear(r, c.clusterB, 0.035))
			c.topic = append(c.topic, c.clusterTp)
			c.inCluster = append(c.inCluster, true)
		}
	}
	switch {
	case shape == "uniform":
		for i := 0; i < n; i++ {
			c.vecs = append(c.vecs, cUnit(cGauss(r, dim, 1)))
			c.topic = append(c.topic, -1)
		}
		c.inCluster = make([]bool, n)
	case shape == "topics":
		c.vecs, c.topic = topicVectors(r, dim, n, 1.1)
		c.inCluster = make([]bool, n)
	case shape == "topicsloose":
		c.vecs, c.topic = topicVectors(r, dim, n, 2.2)
		c.inCluster = make([]bool, n)
	case strings.HasPrefix(shape, "dup"):
		cs, err := strconv.Atoi(shape[3:])
		if err != nil {
			panic(shape)
		}
		c.vecs, c.topic = topicVectors(r, dim, n-cs, 1.1)
		c.inCluster = make([]bool, n-cs)
		addCluster(cs)
	case shape == "mix":
		base := n - 500
		c.vecs, c.topic = topicVectors(r, dim, base, 1.1)
		c.inCluster = make([]bool, base)
		dupItems := int(0.6 * (0.17*float64(n) - 500))
		for dupItems > 0 {
			src := r.Intn(len(c.vecs))
			g := 2 + r.Intn(7)
			s := 0.035 + r.Float64()*0.2
			for j := 0; j < g-1 && dupItems > 0; j++ {
				k := r.Intn(len(c.vecs))
				c.vecs[k] = cNear(r, c.vecs[src], s)
				c.topic[k] = c.topic[src]
				dupItems--
			}
		}
		addCluster(500)
	default:
		panic("unknown shape " + shape)
	}
	c.ids = make([][16]byte, len(c.vecs))
	for i := range c.ids {
		r.Read(c.ids[i][:])
	}
	// Shuffle insertion order so the cluster is not one block at the end.
	perm := r.Perm(len(c.vecs))
	nv := make([][]float32, len(perm))
	nt := make([]int, len(perm))
	nc := make([]bool, len(perm))
	for i, p := range perm {
		nv[i], nt[i], nc[i] = c.vecs[p], c.topic[p], c.inCluster[p]
	}
	c.vecs, c.topic, c.inCluster = nv, nt, nc
	return c
}

type query struct {
	class string
	q     []float32
}

// makeQueries: "ordinary" queries near a random non-cluster item; for shapes
// with a cluster, "cluster-topic" queries near a non-cluster member of the
// cluster's topic and "cluster-aimed" queries near the cluster itself.
func makeQueries(c *corpus, nq int, seed int64) []query {
	r := rand.New(rand.NewSource(seed))
	pick := func(pred func(i int) bool) int {
		for {
			i := r.Intn(len(c.vecs))
			if !c.inCluster[i] && pred(i) {
				return i
			}
		}
	}
	var qs []query
	for i := 0; i < nq; i++ {
		qs = append(qs, query{"ordinary", cNear(r, c.vecs[pick(func(int) bool { return true })], 0.6)})
	}
	if c.clusterTp >= 0 {
		for i := 0; i < nq; i++ {
			j := pick(func(k int) bool { return c.topic[k] == c.clusterTp })
			qs = append(qs, query{"cluster-topic", cNear(r, c.vecs[j], 0.6)})
		}
		for i := 0; i < nq; i++ {
			qs = append(qs, query{"cluster-aimed", cNear(r, c.clusterB, 0.5)})
		}
	}
	return qs
}

// exactTopK is brute-force ground truth: the k nearest by cosine distance
// (ties by id, for determinism), restricted to items live(i) when live != nil.
// It also returns the k-th distance.
func exactTopK(c *corpus, q []float32, k int, live func(int) bool) ([][16]byte, float64) {
	ids, d := exactTopKD(c, q, k, live)
	if len(d) == 0 {
		return ids, 0
	}
	return ids, d[len(d)-1]
}

// exactTopKD is exactTopK returning every distance, ascending.
func exactTopKD(c *corpus, q []float32, k int, live func(int) bool) ([][16]byte, []float64) {
	type s struct {
		i int
		d float64
	}
	all := make([]s, 0, len(c.vecs))
	for i, v := range c.vecs {
		if live != nil && !live(i) {
			continue
		}
		all = append(all, s{i, 1 - float64(CosineSimilarity(q, v))})
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].d != all[b].d {
			return all[a].d < all[b].d
		}
		return string(c.ids[all[a].i][:]) < string(c.ids[all[b].i][:])
	})
	if len(all) > k {
		all = all[:k]
	}
	out := make([][16]byte, len(all))
	ds := make([]float64, len(all))
	for i, x := range all {
		out[i] = c.ids[x.i]
		ds[i] = x.d
	}
	return out, ds
}

// recallStrict is |got ∩ want| / |want|.
func recallStrict(got []ScoredID, want [][16]byte) float64 {
	if len(want) == 0 {
		return 1
	}
	w := make(map[[16]byte]bool, len(want))
	for _, x := range want {
		w[x] = true
	}
	h := 0
	for _, g := range got {
		if w[g.ID] {
			h++
		}
	}
	return float64(h) / float64(len(want))
}

// recallWithin counts a returned item as a hit when its true distance to the
// query is within eps of the exact k-th distance — i.e. it is
// indistinguishable from a true top-k item at the resolution of the embedder
// that would have produced it. eps = 0 reduces to strict recall up to exact
// ties. Distances are recomputed from the corpus, never taken from the score.
func recallWithin(c *corpus, vecOf map[[16]byte][]float32, q []float32, got []ScoredID, k int, kth, eps float64) float64 {
	h := 0
	for i, g := range got {
		if i >= k {
			break
		}
		v := vecOf[g.ID]
		if v == nil {
			continue
		}
		if 1-float64(CosineSimilarity(q, v)) <= kth+eps {
			h++
		}
	}
	if h > k {
		h = k
	}
	return float64(h) / float64(k)
}

// recallCollapsed grades with every member of the corpus's near-identical
// cluster collapsed to ONE representative key: E = the distinct keys of the
// exact top-k, G = the distinct keys of the returned top-k, recall = |G ∩ E| /
// |E|. Swapping one near-duplicate for another is not a miss, and no epsilon
// is needed — the grading the adversarial review proposed as primary.
func recallCollapsed(c *corpus, inCluster map[[16]byte]bool, got []ScoredID, want [][16]byte, k int) float64 {
	var cluster [16]byte
	cluster[0] = 0xFF // a key no item can have alongside a real cluster member
	key := func(id [16]byte) [16]byte {
		if inCluster[id] {
			return cluster
		}
		return id
	}
	e := map[[16]byte]bool{}
	for _, id := range want {
		e[key(id)] = true
	}
	g := map[[16]byte]bool{}
	for i, x := range got {
		if i >= k {
			break
		}
		g[key(x.ID)] = true
	}
	h := 0
	for x := range g {
		if e[x] {
			h++
		}
	}
	if len(e) == 0 {
		return 1
	}
	return float64(h) / float64(len(e))
}

func (c *corpus) clusterSet() map[[16]byte]bool {
	m := map[[16]byte]bool{}
	for i, id := range c.ids {
		if c.inCluster[i] {
			m[id] = true
		}
	}
	return m
}

func (c *corpus) vecMap() map[[16]byte][]float32 {
	m := make(map[[16]byte][]float32, len(c.vecs))
	for i, id := range c.ids {
		m[id] = c.vecs[i]
	}
	return m
}

func memPebble(t testing.TB) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// buildCorpus stores and inserts every vector of c into a fresh index on db,
// in corpus order, then waits for persistence.
func buildCorpus(t testing.TB, c *corpus, db *pebble.DB, ws [8]byte, configure func(*Index)) *Index {
	t.Helper()
	idx := New(db, ws)
	if configure != nil {
		configure(idx)
	}
	for i, v := range c.vecs {
		if db != nil {
			if err := idx.StoreVector(c.ids[i], v); err != nil {
				t.Fatal(err)
			}
		}
		idx.Insert(c.ids[i], v)
	}
	idx.Close()
	return idx
}

// closedAgainstEntry counts the nodes (with vectors) from which no path of
// layer-0 edges leads back to the entry point — the members of closed
// islands. A greedy walk that enters such a node can never leave its island.
func closedAgainstEntry(idx *Index) int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return countClosed(idx.nodes, idx.entryPoint)
}

func countClosed(nodes map[[16]byte]*HNSWNode, entry [16]byte) int {
	rev := make(map[[16]byte][][16]byte, len(nodes))
	withVec := 0
	for id, n := range nodes {
		if len(n.vec) == 0 {
			continue
		}
		withVec++
		for _, nb := range n.getLayer(0) {
			rev[nb] = append(rev[nb], id)
		}
	}
	if nodes[entry] == nil {
		return withVec
	}
	seen := map[[16]byte]bool{entry: true}
	q := [][16]byte{entry}
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		for _, p := range rev[cur] {
			if !seen[p] {
				seen[p] = true
				q = append(q, p)
			}
		}
	}
	reach := 0
	for id := range seen {
		if n := nodes[id]; n != nil && len(n.vec) > 0 {
			reach++
		}
	}
	return withVec - reach
}

func topIDs(t testing.TB, idx *Index, q []float32, k int) [][16]byte {
	t.Helper()
	r, err := idx.Search(context.Background(), q, k)
	if err != nil {
		t.Fatal(err)
	}
	out := make([][16]byte, len(r))
	for i, x := range r {
		out[i] = x.ID
	}
	return out
}

func sameIDs(a, b [][16]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
