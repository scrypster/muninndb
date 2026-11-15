package writetwin

import (
	"bytes"
	"context"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// ReplayOptions configures the read-only per-vault acceptance replay (v3
// §6.i, "S-pre"). Zero values take the defaults below.
type ReplayOptions struct {
	Days      int           // replay window; default 60
	Now       time.Time     // end of the window; default time.Now()
	Threshold Threshold     // the vault's resolved T (must be OK)
	BatchGap  time.Duration // pairs this close are labelled intra-batch; default 2s
	JudgeMax  int           // flagged pairs sampled for blind judging; default 100
	Seed      int64         // judging-sample seed; default 1
}

// ReplayPair is one flagged (new write, earlier head) pair.
type ReplayPair struct {
	NewID      storage.ULID
	HeadID     storage.ULID
	Similarity float64
	AgeSeconds float64
	Rule       string // "recent" | "batch" (≤ BatchGap apart: the batch approximation)
	NewCreated time.Time
}

// ReplayReport is what the replay prints. It carries ids and counts only —
// never content. Content goes only to the judging file the operator asks for.
type ReplayReport struct {
	Threshold       float64        `json:"threshold"`
	ThresholdSource string         `json:"threshold_source"`
	Days            int            `json:"days"`
	From            time.Time      `json:"from"`
	To              time.Time      `json:"to"`
	Writes          int            `json:"writes"`         // non-successor, vectored writes in the window
	WritesTotal     int            `json:"writes_total"`   // every engram created in the window
	Unvectored      int            `json:"unvectored"`     // in-window writes with no stored vector (not replayable)
	Successors      int            `json:"successors"`     // in-window declared successors (evolve/consolidate), never shown a block
	FlaggedWrites   int            `json:"flagged_writes"` // writes that would have carried a block
	FlaggedRecent   int            `json:"flagged_recent"` // … whose top pair is > BatchGap apart
	FlaggedBatch    int            `json:"flagged_batch"`  // … whose top pair is ≤ BatchGap apart
	Pairs           int            `json:"pairs"`          // named (write, head) pairs, ≤ 2 per write
	FlagsPer100     float64        `json:"flags_per_100_writes"`
	PerDay          map[string]int `json:"flags_per_day"` // UTC day → flagged writes
	PerDayMax       int            `json:"flags_per_day_max"`
	Guard           string         `json:"guard"`
	GuardShare      float64        `json:"guard_share"`
	GuardSample     int            `json:"guard_sample"`
	Verdict         Verdict        `json:"verdict"`
	Approximations  []string       `json:"approximations"`
}

type replayEngram struct {
	id         storage.ULID
	created    time.Time
	vec        []float32
	concept    string
	content    string
	successor  bool // declared at write time: an outgoing supersedes edge made with the engram
	supersedes []replayEdge
}

type replayEdge struct {
	from    storage.ULID // the superseder
	created time.Time
}

func ulidLess(a, b storage.ULID) bool { return bytes.Compare(a[:], b[:]) < 0 }

// Replay runs slice 1's exact rule over the last opt.Days of vault ws, read
// only. It returns the report and every flagged pair (the operator's input to
// JudgingSet). The rule, per write W in the window:
//
//   - candidates are writes E made before W and at most Window earlier, with
//     cosine(W, E) ≥ T — the ring rule, with the ring's 512 cap ignored (an
//     over-count, the conservative direction for the rate veto);
//   - each candidate is resolved to its chain head AS OF W (see headAsOf);
//     a head equal to W, or reached through a supersedes edge W itself made at
//     write time (an evolve), is skipped; heads are deduped, at most MaxShown;
//   - W is flagged when at least one head survives.
//
// Vectors are the stored 0x18 vectors (dequantized int8), which is what HNSW
// holds after a restart; a live process compares the float vectors of writes
// it embedded itself. The difference is ~1e-3 in cosine.
func Replay(ctx context.Context, store *storage.PebbleStore, ws [8]byte, opt ReplayOptions) (*ReplayReport, []ReplayPair, *Corpus, error) {
	opt = opt.withDefaults()
	from := opt.Now.Add(-time.Duration(opt.Days) * 24 * time.Hour)

	all, err := loadReplayEngrams(ctx, store, ws)
	if err != nil {
		return nil, nil, nil, err
	}
	rep := &ReplayReport{
		Threshold: opt.Threshold.Value, ThresholdSource: opt.Threshold.Source,
		Days: opt.Days, From: from, To: opt.Now, PerDay: map[string]int{},
		Approximations: []string{
			"a candidate forgotten (soft-deleted without a stamp) before W cannot be dated, so it is treated as live at W: flags over-count",
			"the 512-entry ring cap is not modelled: flags over-count on vaults writing more than 512 memories an hour",
			"hard-deleted memories are gone from the store and are not replayed",
			"stored vectors are int8-dequantized; the live ring compares float vectors (cosine differs by ~1e-3)",
			"intra-batch is approximated as writes within 2s of each other",
		},
	}

	var pairs []ReplayPair
	lo := 0 // sliding window start into all (sorted by created, id)
	for wi := range all {
		w := &all[wi]
		if w.created.Before(from) || w.created.After(opt.Now) {
			continue
		}
		rep.WritesTotal++
		if len(w.vec) == 0 {
			rep.Unvectored++
			continue
		}
		if w.successor {
			rep.Successors++
			continue
		}
		rep.Writes++
		for lo < wi && w.created.Sub(all[lo].created) > Window {
			lo++
		}
		type cand struct {
			head storage.ULID
			sim  float64
			eCre time.Time
		}
		var cands []cand
		for ei := lo; ei < wi; ei++ {
			e := &all[ei]
			if len(e.vec) != len(w.vec) {
				continue
			}
			sim := float64(hnsw.CosineSimilarity(w.vec, e.vec))
			if sim < opt.Threshold.Value {
				continue
			}
			head, ok := headAsOf(all, ei, w)
			if !ok {
				continue
			}
			cands = append(cands, cand{head: head, sim: sim, eCre: e.created})
		}
		if len(cands) == 0 {
			continue
		}
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })
		seen := map[storage.ULID]bool{}
		named := 0
		for _, c := range cands {
			if seen[c.head] || named >= MaxShown {
				continue
			}
			seen[c.head] = true
			named++
			rule := "recent"
			if w.created.Sub(c.eCre) <= opt.BatchGap {
				rule = "batch"
			}
			pairs = append(pairs, ReplayPair{NewID: w.id, HeadID: c.head, Similarity: c.sim,
				AgeSeconds: w.created.Sub(c.eCre).Seconds(), Rule: rule, NewCreated: w.created})
			if named == 1 {
				if rule == "batch" {
					rep.FlaggedBatch++
				} else {
					rep.FlaggedRecent++
				}
			}
		}
		rep.FlaggedWrites++
		rep.PerDay[w.created.UTC().Format("2006-01-02")]++
	}
	rep.Pairs = len(pairs)
	for _, n := range rep.PerDay {
		if n > rep.PerDayMax {
			rep.PerDayMax = n
		}
	}
	if rep.Writes > 0 {
		rep.FlagsPer100 = 100 * float64(rep.FlaggedWrites) / float64(rep.Writes)
	}

	rank1 := guardSampleFromAll(all, GuardMaxSample)
	rep.GuardSample = len(rank1)
	rep.Guard, rep.GuardShare = GuardVerdict(rank1, opt.Threshold.Value)
	rep.Verdict = Decide(rep.FlaggedWrites, rep.Writes, rep.Guard, 0, 0)
	return rep, pairs, &Corpus{all: all}, nil
}

// Corpus is the replay's loaded vault, kept so JudgingSet can sample from it
// without a second scan. Opaque: it holds content, which only the judging
// file may carry.
type Corpus struct{ all []replayEngram }

func (opt ReplayOptions) withDefaults() ReplayOptions {
	if opt.Days <= 0 {
		opt.Days = 60
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	if opt.BatchGap <= 0 {
		opt.BatchGap = 2 * time.Second
	}
	if opt.JudgeMax <= 0 {
		opt.JudgeMax = 100
	}
	if opt.Seed == 0 {
		opt.Seed = 1
	}
	return opt
}

// declaredAtWrite is how close to a successor's own CreatedAt its supersedes
// edge must be to count as declared by the write itself (evolve writes the
// successor and the edge in one batch) rather than resolved later by a link.
const declaredAtWrite = 2 * time.Second

// headAsOf resolves candidate all[ei] to the head of its supersedes chain as
// it stood when w was written: only edges created before w count, except an
// edge w itself made at write time (an evolve), which marks the pair as
// already declared — skipped. ok=false means skip.
func headAsOf(all []replayEngram, ei int, w *replayEngram) (storage.ULID, bool) {
	cur := &all[ei]
	visited := map[storage.ULID]bool{cur.id: true}
	for {
		var next *replayEdge
		for k := range cur.supersedes {
			edge := &cur.supersedes[k]
			if edge.from == w.id {
				if edge.created.Sub(w.created) <= declaredAtWrite {
					return storage.ULID{}, false // w declared it: an evolve, not a twin
				}
				continue // a later resolution: as of w, not yet linked
			}
			if !edge.created.Before(w.created) {
				continue
			}
			if next != nil && next.from != edge.from {
				return storage.ULID{}, false // ambiguous fork at w: name nothing
			}
			next = edge
		}
		if next == nil {
			break
		}
		if visited[next.from] {
			return storage.ULID{}, false // cycle
		}
		visited[next.from] = true
		ni := findEngram(all, next.from)
		if ni < 0 {
			break // dangling: the chain ends at cur
		}
		cur = &all[ni]
	}
	if cur.id == w.id {
		return storage.ULID{}, false
	}
	return cur.id, true
}

func findEngram(all []replayEngram, id storage.ULID) int {
	// all is sorted by (created, id), not by id; a linear probe is fine for the
	// rare supersedes hop of an offline tool.
	for i := range all {
		if all[i].id == id {
			return i
		}
	}
	return -1
}

func loadReplayEngrams(ctx context.Context, store *storage.PebbleStore, ws [8]byte) ([]replayEngram, error) {
	var all []replayEngram
	err := store.ScanEngrams(ctx, ws, func(eng *storage.Engram) error {
		all = append(all, replayEngram{id: eng.ID, created: eng.CreatedAt, vec: eng.Embedding,
			concept: eng.Concept, content: eng.Content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].created.Equal(all[j].created) {
			return all[i].created.Before(all[j].created)
		}
		return ulidLess(all[i].id, all[j].id)
	})
	byID := make(map[storage.ULID]int, len(all))
	for i := range all {
		byID[all[i].id] = i
	}
	const chunk = 256
	for start := 0; start < len(all); start += chunk {
		end := start + chunk
		if end > len(all) {
			end = len(all)
		}
		ids := make([]storage.ULID, 0, end-start)
		for i := start; i < end; i++ {
			ids = append(ids, all[i].id)
		}
		fwd, err := store.GetAssociations(ctx, ws, ids, 0)
		if err != nil {
			return nil, err
		}
		for src, assocs := range fwd {
			si, ok := byID[src]
			if !ok {
				continue
			}
			for _, a := range assocs {
				if a.RelType != storage.RelSupersedes {
					continue
				}
				ti, ok := byID[a.TargetID]
				if !ok {
					continue
				}
				all[ti].supersedes = append(all[ti].supersedes, replayEdge{from: src, created: a.CreatedAt})
				if a.CreatedAt.Sub(all[si].created) <= declaredAtWrite {
					all[si].successor = true
				}
			}
		}
	}
	return all, nil
}

// guardSampleFromAll takes the most recent ≤max vectored, non-successor
// engrams and returns each one's nearest-earlier-neighbour cosine, brute
// force over every earlier engram (exact; the live guard approximates the
// same statistic through HNSW).
func guardSampleFromAll(all []replayEngram, max int) []float64 {
	var out []float64
	for i := len(all) - 1; i >= 0 && len(out) < max; i-- {
		s := &all[i]
		if len(s.vec) == 0 || s.successor {
			continue
		}
		best := math.NaN()
		for j := 0; j < i; j++ {
			if len(all[j].vec) != len(s.vec) {
				continue
			}
			c := float64(hnsw.CosineSimilarity(s.vec, all[j].vec))
			if math.IsNaN(best) || c > best {
				best = c
			}
		}
		out = append(out, best)
	}
	return out
}

// JudgeItem is one blind-judging row: full text of both memories, nothing
// that says whether it is a flag or a control, or how similar the vectors are.
type JudgeItem struct {
	Item    int       `json:"item"`
	Earlier JudgeText `json:"earlier"`
	Later   JudgeText `json:"later"`
	Ask     string    `json:"ask"`
}

// JudgeText is one memory's full text in a judging row.
type JudgeText struct {
	Concept string `json:"concept"`
	Content string `json:"content"`
}

// KeyItem maps a judging row back to what it was. Kept in a separate file so
// the judge never sees it.
type KeyItem struct {
	Item       int     `json:"item"`
	Kind       string  `json:"kind"` // "flag" | "control"
	NewID      string  `json:"new_id"`
	OtherID    string  `json:"other_id"`
	Similarity float64 `json:"similarity"`
	Rule       string  `json:"rule,omitempty"`
}

// JudgeAsk is the question every judging row carries.
const JudgeAsk = "Is the later memory a duplicate of the earlier one, an update/correction of it, or a distinct fact? Answer duplicate | update | distinct."

// JudgingSet samples up to opt.JudgeMax flagged writes (their top pair) plus
// an equal number of controls — for each sampled write, one earlier memory at
// rank 11–20 of its nearest neighbours — shuffled together. Full text is used:
// Gate 0's 900-character window shared the embedder's own truncation.
func JudgingSet(c *Corpus, pairs []ReplayPair, opt ReplayOptions) ([]JudgeItem, []KeyItem) {
	opt = opt.withDefaults()
	all := c.all
	rng := rand.New(rand.NewSource(opt.Seed))
	top := map[storage.ULID]ReplayPair{}
	var order []storage.ULID
	for _, p := range pairs {
		if _, ok := top[p.NewID]; !ok {
			top[p.NewID] = p
			order = append(order, p.NewID)
		}
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	if len(order) > opt.JudgeMax {
		order = order[:opt.JudgeMax]
	}
	idx := make(map[storage.ULID]int, len(all))
	for i := range all {
		idx[all[i].id] = i
	}
	type row struct {
		earlier, later int
		key            KeyItem
	}
	var rows []row
	for _, nid := range order {
		p := top[nid]
		wi, hi := idx[p.NewID], idx[p.HeadID]
		rows = append(rows, row{earlier: hi, later: wi, key: KeyItem{Kind: "flag", NewID: p.NewID.String(),
			OtherID: p.HeadID.String(), Similarity: p.Similarity, Rule: p.Rule}})
		if ci, sim, ok := controlFor(all, wi, rng); ok {
			rows = append(rows, row{earlier: ci, later: wi, key: KeyItem{Kind: "control", NewID: p.NewID.String(),
				OtherID: all[ci].id.String(), Similarity: sim}})
		}
	}
	rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	judge := make([]JudgeItem, len(rows))
	keys := make([]KeyItem, len(rows))
	for i, r := range rows {
		judge[i] = JudgeItem{Item: i + 1, Ask: JudgeAsk,
			Earlier: JudgeText{Concept: all[r.earlier].concept, Content: all[r.earlier].content},
			Later:   JudgeText{Concept: all[r.later].concept, Content: all[r.later].content}}
		r.key.Item = i + 1
		keys[i] = r.key
	}
	return judge, keys
}

// controlFor picks one earlier memory at rank 11–20 of all[wi]'s nearest
// earlier neighbours.
func controlFor(all []replayEngram, wi int, rng *rand.Rand) (int, float64, bool) {
	w := &all[wi]
	type nb struct {
		i   int
		sim float64
	}
	var nbs []nb
	for j := 0; j < wi; j++ {
		if len(all[j].vec) != len(w.vec) || len(w.vec) == 0 {
			continue
		}
		nbs = append(nbs, nb{j, float64(hnsw.CosineSimilarity(w.vec, all[j].vec))})
	}
	if len(nbs) < 11 {
		return 0, 0, false
	}
	sort.Slice(nbs, func(a, b int) bool { return nbs[a].sim > nbs[b].sim })
	hi := 20
	if len(nbs) < hi {
		hi = len(nbs)
	}
	pick := nbs[10+rng.Intn(hi-10)]
	return pick.i, pick.sim, true
}

// Score reads judged labels against the key and returns how many flagged rows
// were judged and how many of those were positives (duplicate or update),
// plus the same for controls (the judge's false-positive base rate).
func Score(keys []KeyItem, labels map[int]string) (flagJudged, flagPos, ctlJudged, ctlPos int) {
	for _, k := range keys {
		l, ok := labels[k.Item]
		if !ok {
			continue
		}
		pos := l == "duplicate" || l == "update"
		if k.Kind == "flag" {
			flagJudged++
			if pos {
				flagPos++
			}
		} else {
			ctlJudged++
			if pos {
				ctlPos++
			}
		}
	}
	return
}
