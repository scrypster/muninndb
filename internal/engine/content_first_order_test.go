package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Content-first order: "evidence of match orders; evidence of association
// annotates" (COG-35). On the default ACT-R path the returned order is the
// absolute content score (content match x confidence); entity, Hebbian and
// predictive-activation evidence is reported per row and only breaks exact
// content ties; rows with no content score come after every scored row; the
// cut to `limit` happens once, after ordering, so the first N rows do not
// depend on `limit`.
//
// The engine here is FTS-only (no embedder), restartable over one Pebble
// directory, and has an auth store so a vault can be configured rrf or
// weighted_sum. Association evidence is primed DETERMINISTICALLY: the
// activation log is written directly (never via a timed recall), Hebbian edges
// via Link, and PAS transitions through a fixed in-memory transition store.
// Fixtures are invented (trail maintenance).
// ---------------------------------------------------------------------------

type cfEnv struct {
	t *testing.T
	// now is the one instant this env lives at: fixtures are written relative
	// to it, the activation log is primed at it, and scoring reads it instead
	// of the wall clock. weighted_sum scores recency and decay against `now`,
	// so with the wall clock a slow runner (about 3.5 s more between writing
	// the fixtures and recalling them, measured) moved a golden score in its
	// fourth decimal.
	now   time.Time
	dir   string
	store *storage.PebbleStore
	fts   *fts.Index
	as    *auth.Store
	eng   *Engine
	trans *cfTransitions
}

// cfTransitions is a fixed PAS transition table (src -> targets), standing in
// for the transition cache so a test decides exactly which rows PAS predicts.
type cfTransitions struct {
	mu sync.Mutex
	m  map[[16]byte][]storage.TransitionTarget
}

func (c *cfTransitions) GetTopTransitions(_ context.Context, _ [8]byte, src [16]byte, topK int) ([]storage.TransitionTarget, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]storage.TransitionTarget(nil), c.m[src]...)
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func (c *cfTransitions) set(src storage.ULID, dsts ...storage.ULID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ts []storage.TransitionTarget
	for _, d := range dsts {
		ts = append(ts, storage.TransitionTarget{ID: [16]byte(d), Count: 10})
	}
	c.m[[16]byte(src)] = ts
}

func newCFEnv(t *testing.T) *cfEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-content-first-*")
	if err != nil {
		t.Fatal(err)
	}
	c := &cfEnv{t: t, now: time.Now(), dir: dir, trans: &cfTransitions{m: map[[16]byte][]storage.TransitionTarget{}}}
	c.open()
	t.Cleanup(func() { c.close(); os.RemoveAll(dir) })
	return c
}

func (c *cfEnv) open() {
	c.t.Helper()
	db, err := storage.OpenPebble(c.dir, storage.DefaultOptions())
	if err != nil {
		c.t.Fatal(err)
	}
	c.store = storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	c.fts = fts.New(db)
	c.as = auth.NewStore(db)
	embedder := &noopEmbedder{}
	act := activation.New(c.store, &ftsAdapter{c.fts}, nil, embedder)
	act.SetTransitionStore(c.trans)
	act.SetScoreClockForTesting(func() time.Time { return c.now })
	trig := trigger.New(c.store, &ftsTrigAdapter{c.fts}, nil, embedder)
	c.eng = NewEngine(EngineConfig{Store: c.store, AuthStore: c.as, FTSIndex: c.fts, ActivationEngine: act, TriggerSystem: trig, Embedder: embedder})
}

func (c *cfEnv) close() {
	if c.eng == nil {
		return
	}
	c.eng.Stop()
	c.store.Close()
	c.eng = nil
}

// restart is a fresh process over the same persisted store: the in-memory
// activation log is empty afterwards. The PAS table is test state and survives.
func (c *cfEnv) restart() {
	c.t.Helper()
	c.close()
	c.open()
}

func (c *cfEnv) configure(vault string, fusion string) {
	c.t.Helper()
	var p *auth.PlasticityConfig
	if fusion != "" {
		f := fusion
		p = &auth.PlasticityConfig{ScoringFusion: &f}
	}
	if err := c.as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true, Plasticity: p}); err != nil {
		c.t.Fatal(err)
	}
}

// write stores and indexes one memory at the store level, linking it to the
// given (invented) entities. age orders ULIDs: a larger age is an older ULID.
func (c *cfEnv) write(vault, concept, content string, age time.Duration, entities ...string) storage.ULID {
	c.t.Helper()
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	created := c.now.Add(-age)
	id, err := c.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: concept, Content: content, Confidence: 1,
		CreatedAt: created, UpdatedAt: created, LastAccess: created,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.fts.IndexEngram(ws, [16]byte(id), concept, "", content, nil); err != nil {
		c.t.Fatal(err)
	}
	if err := c.store.WriteVaultName(ws, vault); err != nil {
		c.t.Fatal(err)
	}
	for _, name := range entities {
		if err := c.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: name, Type: "place", Source: "inline"}, "inline"); err != nil {
			c.t.Fatal(err)
		}
		if err := c.store.WriteEntityEngramLink(ctx, ws, id, name); err != nil {
			c.t.Fatal(err)
		}
	}
	return id
}

func (c *cfEnv) link(vault string, a, b storage.ULID, w float32) {
	c.t.Helper()
	if _, err := c.eng.Link(context.Background(), &mbp.LinkRequest{Vault: vault, SourceID: a.String(), TargetID: b.String(), Weight: w}); err != nil {
		c.t.Fatal(err)
	}
}

// primeLog records `ids` as the most recent activation of the vault — the
// state a normal recall that returned them leaves behind — synchronously.
func (c *cfEnv) primeLog(vault string, ids ...storage.ULID) {
	c.t.Helper()
	c.eng.activation.WaitLogIdle()
	ws := c.store.ResolveVaultPrefix(vault)
	c.eng.activation.AssocLog().Record(activation.LogEntry{VaultID: wsVaultID(ws), At: c.now, EngramIDs: ids, Scores: make([]float64, len(ids))})
}

type cfRow struct {
	id                      string
	score, abs              float32
	entity, hebbian, transi float32
}

// cfAgrees reports whether an explain row agrees with a recall row. The returned
// score and the content score carry no clock, so they must agree EXACTLY. The
// three association annotations decay with wall-clock time, and explain runs
// after recall: on a loaded machine the gap is long enough to move a float32
// (measured: hebbian 1.000 vs 0.9986 across a 2.5 s gap), which made a
// bit-for-bit comparison flake. They annotate and never order (COG-35), so
// they are held to a tolerance wide enough for a slow run and far too narrow
// to hide a real disagreement.
func cfAgrees(recall, explain cfRow) bool {
	const tol = 0.05
	near := func(a, b float32) bool {
		d := a - b
		if d < 0 {
			d = -d
		}
		return d <= tol
	}
	return recall.id == explain.id && recall.score == explain.score && recall.abs == explain.abs &&
		near(recall.entity, explain.entity) && near(recall.hebbian, explain.hebbian) && near(recall.transi, explain.transi)
}

func (r cfRow) String() string {
	return fmt.Sprintf("{%s score=%.4f abs=%.4f ent=%.3f heb=%.3f pas=%.3f}", r.id[len(r.id)-6:], r.score, r.abs, r.entity, r.hebbian, r.transi)
}

func (r cfRow) assoc() float32 { return r.entity + r.hebbian + r.transi }

func (c *cfEnv) recall(vault, query string, limit int, readOnly bool) []cfRow {
	c.t.Helper()
	resp, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{query}, MaxResults: limit, ReadOnly: readOnly,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if !readOnly {
		c.eng.activation.WaitLogIdle()
	}
	out := make([]cfRow, 0, len(resp.Activations))
	for _, a := range resp.Activations {
		sc := a.ScoreComponents
		out = append(out, cfRow{a.ID, a.Score, sc.AbsoluteScore, sc.EntityBoost, sc.HebbianBoost, sc.TransitionBoost})
	}
	return out
}

func cfIndex(rows []cfRow, id storage.ULID) int {
	for i, r := range rows {
		if r.id == id.String() {
			return i
		}
	}
	return -1
}

// cfAssertContentOrder fails when a scored row (absolute score > 0) sits above
// a scored row with a strictly greater absolute score, or when a row with no
// content score sits above any scored row.
func cfAssertContentOrder(t *testing.T, label string, rows []cfRow) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if prev.abs == 0 && cur.abs > 0 {
			t.Errorf("%s: row %d has no content score but sits above scored row %d\n%v", label, i-1, i, rows)
			return
		}
		if cur.abs > prev.abs {
			t.Errorf("%s: row %d (abs %.4f, assoc %.3f) outranks row %d with the stronger content match (abs %.4f)\n%v",
				label, i-1, prev.abs, prev.assoc(), i, cur.abs, rows)
			return
		}
	}
}

// cfAssertNotAboveStronger fails when the row `id` sits above any row with a
// strictly greater absolute content score.
func cfAssertNotAboveStronger(t *testing.T, label string, rows []cfRow, id storage.ULID) {
	t.Helper()
	i := cfIndex(rows, id)
	for j := i + 1; j < len(rows); j++ {
		if rows[j].abs > rows[i].abs {
			t.Errorf("%s: the weaker match (abs %.4f, assoc %.3f) at rank %d outranks a stronger content match (abs %.4f) at rank %d\n%v",
				label, rows[i].abs, rows[i].assoc(), i, rows[j].abs, j, rows)
			return
		}
	}
}

// --- fixture ---------------------------------------------------------------

const cfQuery = "culvert outlet riprap drainage"

// cfFix is a graded set of matches for cfQuery: five strong rows (all four
// terms; the strongest is an entity-boost seed carrying two rare entities),
// eleven middle rows (two terms), and three weak rows (one term) each carrying
// one kind of association evidence, plus an unrelated partner the activation
// log names and an injectee that shares the rare entities but matches nothing.
type cfFix struct {
	strong   []storage.ULID
	middle   []storage.ULID
	weakEnt  storage.ULID // weak match sharing the seed's rare entities
	weakHeb  storage.ULID // weak match with a Hebbian edge to `partner`
	weakPAS  storage.ULID // weak match PAS-predicted from `partner`
	partner  storage.ULID // matches nothing; primed into the activation log
	injectee storage.ULID // matches nothing; shares the rare entities
}

func cfBuild(c *cfEnv, vault string) cfFix {
	var f cfFix
	h := time.Hour
	ents := []string{"Kestrel Saddle", "Harrowmoor Spur"}
	f.strong = append(f.strong,
		c.write(vault, "culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two", 40*h, ents...),
		c.write(vault, "culvert outlet riprap drainage check", "the culvert outlet riprap and drainage were checked", 39*h),
		c.write(vault, "riprap at the culvert outlet", "riprap placed at the culvert outlet for drainage", 38*h),
		c.write(vault, "drainage culvert outlet notes", "culvert outlet drainage riprap notes from the crew", 37*h),
		c.write(vault, "outlet drainage", "drainage at the culvert outlet needs riprap", 36*h),
	)
	middles := [][2]string{
		{"culvert outlet riprap", "the culvert outlet riprap was cleared of leaves"},
		{"culvert outlet drainage", "culvert outlet drainage is slow after storms"},
		{"outlet riprap drainage", "riprap drainage at an outlet upstream of the bridge"},
		{"riprap drainage culvert", "riprap helps culvert drainage on the steep pitch"},
		{"outlet cleanup", "the outlet riprap near the culvert was raked"},
		{"drainage survey", "drainage survey of each culvert outlet on the spur"},
		{"riprap sizing", "riprap sizing for the outlet apron drainage"},
		{"culvert apron", "culvert apron riprap was reset below the outlet"},
		{"outlet erosion", "erosion at the culvert outlet means poor drainage"},
		{"culvert flagging", "flagging marks the culvert outlet riprap"},
		{"drainage riprap order", "order more riprap for culvert drainage work"},
	}
	for i, m := range middles {
		f.middle = append(f.middle, c.write(vault, m[0], m[1], time.Duration(30-i)*h))
	}
	f.weakEnt = c.write(vault, "riprap stone supply", "riprap stone for drainage work", 12*h, ents...)
	f.weakHeb = c.write(vault, "drainage dip", "a drainage dip was cut above a culvert on the saddle", 11*h)
	f.weakPAS = c.write(vault, "drainage stone inventory", "drainage stones by an outlet", 10*h)
	f.partner = c.write(vault, "footbridge decking", "the footbridge decking was replaced with treated planks", 50*h)
	f.injectee = c.write(vault, "saddle campsite", "campsite permits for the saddle area", 5*h, ents...)
	for i := 0; i < 6; i++ {
		c.write(vault, fmt.Sprintf("trail log %d", i), fmt.Sprintf("trail crew log entry number %d about brushing", i), time.Duration(60+i)*h)
	}
	c.link(vault, f.partner, f.weakHeb, 1.0)
	c.trans.set(f.partner, f.weakPAS)
	return f
}

// cfPremise fails the test (not as a RED) when the fixture does not carry the
// evidence the assertion is about — otherwise a pass would be vacuous.
func cfPremise(t *testing.T, rows []cfRow, id storage.ULID, what string, get func(cfRow) float32) cfRow {
	t.Helper()
	i := cfIndex(rows, id)
	if i < 0 {
		t.Fatalf("premise: %s row not returned\n%v", what, rows)
	}
	if get(rows[i]) <= 0 {
		t.Fatalf("premise: %s row carries no %s evidence\n%v", what, what, rows)
	}
	if rows[i].abs <= 0 {
		t.Fatalf("premise: %s row is not a scored content match\n%v", what, rows)
	}
	return rows[i]
}

// TestContentFirst_EntityBoostDoesNotOutrankStrongerMatch — a weaker content
// match carrying a large entity boost must not outrank a stronger content
// match; it keeps its entity_boost field. RED on the trunk: the boost (0.19)
// is larger than the gap to every middle row.
func TestContentFirst_EntityBoostDoesNotOutrankStrongerMatch(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-entity"
	f := cfBuild(c, vault)
	rows := c.recall(vault, cfQuery, 40, true)
	cfPremise(t, rows, f.weakEnt, "entity", func(r cfRow) float32 { return r.entity })
	cfAssertNotAboveStronger(t, "entity boost", rows, f.weakEnt)
	if r := rows[cfIndex(rows, f.weakEnt)]; r.entity <= 0 {
		t.Errorf("the boosted row lost its entity_boost field: %v", r)
	}
}

// TestContentFirst_HebbianBoostDoesNotOutrankStrongerMatch — the same for a
// row whose only advantage is a Hebbian edge to a recently activated memory.
func TestContentFirst_HebbianBoostDoesNotOutrankStrongerMatch(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-hebbian"
	f := cfBuild(c, vault)
	c.primeLog(vault, f.partner)
	rows := c.recall(vault, cfQuery, 40, true)
	cfPremise(t, rows, f.weakHeb, "hebbian", func(r cfRow) float32 { return r.hebbian })
	cfAssertNotAboveStronger(t, "hebbian boost", rows, f.weakHeb)
}

// TestContentFirst_PASBoostDoesNotOutrankStrongerMatch — the same for a row
// predicted by a predictive-activation transition from the last activation.
func TestContentFirst_PASBoostDoesNotOutrankStrongerMatch(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-pas"
	f := cfBuild(c, vault)
	c.primeLog(vault, f.partner)
	rows := c.recall(vault, cfQuery, 40, true)
	cfPremise(t, rows, f.weakPAS, "predictive-activation", func(r cfRow) float32 { return r.transi })
	cfAssertNotAboveStronger(t, "predictive-activation boost", rows, f.weakPAS)
}

func cfIDs(rows []cfRow, n int) string {
	if len(rows) < n {
		n = len(rows)
	}
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = rows[i].id
	}
	return fmt.Sprint(ids)
}

// TestContentFirst_FirstNRowsDoNotDependOnLimit — with entity links and a
// primed activation log, the first 10 rows of limit=100 equal limit=10 and the
// first 10 of limit=50 (ids and order), for read-only and for normal recalls
// (each normal recall rewrites the log the next one reads). RED on the trunk:
// phase 6 cut at `limit` BEFORE the entity boost, so a row past the cut could
// be lifted into the first ten only when the limit was large enough to keep it.
// Content-first order alone already makes this pass; the deferred cut is pinned
// by TestContentFirst_TieAtTheCutDoesNotDependOnLimit (adversarial review).
func TestContentFirst_FirstNRowsDoNotDependOnLimit(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-limit"
	f := cfBuild(c, vault)
	for _, readOnly := range []bool{true, false} {
		c.primeLog(vault, f.partner)
		r100 := c.recall(vault, cfQuery, 100, readOnly)
		r10 := c.recall(vault, cfQuery, 10, readOnly)
		r50 := c.recall(vault, cfQuery, 50, readOnly)
		if len(r10) != 10 {
			t.Fatalf("premise: limit=10 returned %d rows", len(r10))
		}
		if a, b := cfIDs(r100, 10), cfIDs(r10, 10); a != b {
			t.Errorf("read_only=%v: first 10 of limit=100 != limit=10\n100: %v\n 10: %v", readOnly, r100[:10], r10)
		}
		if a, b := cfIDs(r50, 10), cfIDs(r10, 10); a != b {
			t.Errorf("read_only=%v: first 10 of limit=50 != limit=10\n50: %v\n10: %v", readOnly, r50[:10], r10)
		}
	}
}

// TestContentFirst_TieAtTheCutDoesNotDependOnLimit — the case only the
// deferred cut decides: two memories tie EXACTLY on content at the limit
// boundary, and only the older one shares a rare entity with the top match.
// Its entity evidence exists only after the entity boost runs, so a phase-6
// cut at `limit` broke the tie newest-first BEFORE that evidence existed, and
// limit=6 returned the other twin while limit=100 put the associated one
// sixth. RED with actReq.DeferTruncation = false.
func TestContentFirst_TieAtTheCutDoesNotDependOnLimit(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-cut-tie"
	h := time.Hour
	c.write(vault, "culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two", 50*h, "Kestrel Saddle")
	c.write(vault, "culvert outlet riprap", "the culvert outlet riprap was reset", 40*h)
	c.write(vault, "outlet riprap drainage", "riprap drainage at the outlet upstream", 39*h)
	c.write(vault, "culvert drainage riprap", "culvert drainage riprap after storms", 38*h)
	c.write(vault, "culvert outlet drainage", "culvert outlet drainage is slow", 37*h)
	twinOld := c.write(vault, "riprap drainage", "riprap drainage below the bench", 30*h, "Kestrel Saddle")
	twinNew := c.write(vault, "riprap drainage", "riprap drainage below the bench", 20*h)
	c.write(vault, "drainage notes", "drainage was checked in autumn", 15*h)
	for i := 0; i < 12; i++ {
		c.write(vault, fmt.Sprintf("brushing log %d", i), fmt.Sprintf("crew brushed the corridor on day %d", i), time.Duration(100+i)*h)
	}
	all := c.recall(vault, cfQuery, 100, true)
	io, in := cfIndex(all, twinOld), cfIndex(all, twinNew)
	if io < 0 || in < 0 || all[io].abs != all[in].abs || all[io].entity <= 0 {
		t.Fatalf("premise: the twins must tie exactly and the older must carry entity evidence\n%v", all)
	}
	if io != 5 || in != 6 {
		t.Fatalf("premise: the twins must sit just below the five seeds (ranks 6 and 7), associated one first: %v", all)
	}
	for _, limit := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		got := c.recall(vault, cfQuery, limit, true)
		if a, b := cfIDs(got, limit), cfIDs(all, limit); a != b {
			t.Errorf("limit=%d: first rows differ from the first %d of limit=100\nlimit %d: %v\nlimit 100: %v", limit, limit, limit, got, all[:limit])
		}
	}
}

// TestContentFirst_ScoreIsTheContentScoreAndExplainAgrees — `score` is
// non-increasing down the list, equals the absolute content score on a scored
// row (0 on a row with no content score), and muninn_explain reports the same
// score and components as the recall row.
func TestContentFirst_ScoreIsTheContentScoreAndExplainAgrees(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-score"
	f := cfBuild(c, vault)
	c.primeLog(vault, f.partner)
	rows := c.recall(vault, cfQuery, 40, true)
	for i, r := range rows {
		if i > 0 && r.score > rows[i-1].score {
			t.Errorf("score increases at row %d: %.4f > %.4f\n%v", i, r.score, rows[i-1].score, rows)
		}
		if r.score != r.abs {
			t.Errorf("row %d: score %.4f is not its content score %.4f (assoc %.3f)\n%v", i, r.score, r.abs, r.assoc(), rows)
		}
	}
	for _, id := range []storage.ULID{f.weakEnt, f.weakHeb, f.weakPAS, f.strong[0]} {
		i := cfIndex(rows, id)
		if i < 0 {
			t.Fatalf("premise: row %s not returned", id)
		}
		ex, err := c.eng.Explain(context.Background(), vault, id.String(), []string{cfQuery}, nil)
		if err != nil {
			t.Fatal(err)
		}
		r := rows[i]
		got := cfRow{r.id, float32(ex.FinalScore), ex.Components.AbsoluteScore, ex.Components.EntityBoost, ex.Components.HebbianBoost, ex.Components.TransitionBoost}
		if !cfAgrees(r, got) {
			t.Errorf("explain disagrees with the recall row\nrecall  %v\nexplain %v", r, got)
		}
	}
}

// TestContentFirst_InjectedRowsComeAfterScoredRows — a row with no content
// score (injected by the entity boost) comes after every scored row; when the
// scored rows fill the limit none is returned; when they do not, it is still
// returned (the association feature is not deleted) and carries its boost.
func TestContentFirst_InjectedRowsComeAfterScoredRows(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-inject"
	f := cfBuild(c, vault)
	all := c.recall(vault, cfQuery, 100, true)
	inj := cfIndex(all, f.injectee)
	if inj < 0 {
		t.Fatalf("premise / feature kept: the entity-linked injectee must still be returned when the limit has room\n%v", all)
	}
	if all[inj].abs != 0 || all[inj].entity <= 0 {
		t.Fatalf("premise: the injectee must be an unscored row carrying an entity boost: %v", all[inj])
	}
	cfAssertContentOrder(t, "limit=100", all)

	scored := 0
	for _, r := range all {
		if r.abs > 0 {
			scored++
		}
	}
	if scored < 10 {
		t.Fatalf("premise: need at least 10 scored rows, have %d", scored)
	}
	for _, r := range c.recall(vault, cfQuery, 10, true) {
		if r.abs == 0 {
			t.Errorf("limit=10 with %d scored rows available returned an unscored injected row %v", scored, r)
		}
	}
}

// TestContentFirst_ExactContentTies — association evidence breaks an EXACT
// content tie (and nothing else), then newest-first. RED on the trunk: the
// Hebbian-boosted twin outranked the strictly stronger match too.
func TestContentFirst_ExactContentTies(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-ties"
	h := time.Hour
	stronger := c.write(vault, "waterbar drainage on switchback four", "waterbar drainage on switchback four and five", 30*h)
	twinOld := c.write(vault, "waterbar on switchback", "waterbar on switchback", 20*h) // older, associated
	twinNew := c.write(vault, "waterbar on switchback", "waterbar on switchback", 10*h) // newer, no evidence
	plainOld := c.write(vault, "switchback waterbar", "the switchback waterbar", 9*h)
	plainNew := c.write(vault, "switchback waterbar", "the switchback waterbar", 8*h)
	partner := c.write(vault, "footbridge decking", "the footbridge decking was replaced", 50*h)
	// Unrelated memories so the query terms are rare enough to carry coverage.
	for i := 0; i < 20; i++ {
		c.write(vault, fmt.Sprintf("brushing log %d", i), fmt.Sprintf("crew brushed the corridor on day %d", i), time.Duration(100+i)*h)
	}
	c.link(vault, partner, twinOld, 1.0)
	c.primeLog(vault, partner)

	rows := c.recall(vault, "waterbar drainage switchback", 10, true)
	pos := func(id storage.ULID) int { return cfIndex(rows, id) }
	for _, id := range []storage.ULID{stronger, twinOld, twinNew, plainOld, plainNew} {
		if pos(id) < 0 {
			t.Fatalf("premise: row %s not returned\n%v", id, rows)
		}
	}
	if rows[pos(twinOld)].abs != rows[pos(twinNew)].abs || rows[pos(plainOld)].abs != rows[pos(plainNew)].abs {
		t.Fatalf("premise: twins must tie exactly on content\n%v", rows)
	}
	if rows[pos(twinOld)].hebbian <= 0 {
		t.Fatalf("premise: the older twin must carry Hebbian evidence\n%v", rows)
	}
	if !(rows[pos(stronger)].abs > rows[pos(twinOld)].abs) {
		t.Fatalf("premise: the stronger match must be strictly stronger\n%v", rows)
	}
	if pos(stronger) > pos(twinOld) {
		t.Errorf("association evidence lifted a tied twin over a strictly stronger match\n%v", rows)
	}
	if pos(twinOld) > pos(twinNew) {
		t.Errorf("association evidence must break an exact content tie: associated older twin below the newer one\n%v", rows)
	}
	if pos(plainNew) > pos(plainOld) {
		t.Errorf("an exact tie with no association evidence must break newest-first\n%v", rows)
	}
}

// TestContentFirst_ReturnedSetIsTheTopContentMatches — when the scored rows
// fill the limit, the rows returned are exactly the top-N content matches;
// only their order relative to the trunk changes, plus the rows the trunk let
// association evidence displace (injected rows, boosted weak matches).
func TestContentFirst_ReturnedSetIsTheTopContentMatches(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-set"
	f := cfBuild(c, vault)
	c.primeLog(vault, f.partner)
	all := c.recall(vault, cfQuery, 100, true)
	scored := make([]cfRow, 0, len(all))
	for _, r := range all {
		if r.abs > 0 {
			scored = append(scored, r)
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].abs > scored[j].abs })
	for _, n := range []int{5, 10, 15} {
		got := c.recall(vault, cfQuery, n, true)
		if len(got) != n {
			t.Fatalf("limit=%d returned %d rows", n, len(got))
		}
		gotSet := map[string]bool{}
		for _, r := range got {
			gotSet[r.id] = true
		}
		// Rows strictly above the N-th content score must all be present;
		// rows at the boundary score may be any tied subset.
		boundary := scored[n-1].abs
		for _, r := range scored {
			if r.abs > boundary && !gotSet[r.id] {
				t.Errorf("limit=%d: a top-%d content match (abs %.4f) is missing\nwant top %v\ngot %v", n, n, r.abs, scored[:n], got)
			}
		}
		for _, r := range got {
			if r.abs < boundary {
				t.Errorf("limit=%d: row %v below the %d-th content score %.4f was returned\n%v", n, r, n, boundary, got)
			}
		}
	}
}

// --- unchanged: differential against the trunk -------------------------------

// cfGoldenPath holds the trunk's output for the differential below. It was
// generated on the trunk commit (CF_UPDATE_GOLDEN=1) before content-first order
// existed and must not be regenerated to make a failure go away.
//
// It was regenerated once, deliberately, when the vault count stopped counting
// a vault's first write twice: the trunk output had scored entity rarity
// against 28 engrams in a vault holding 27. Exactly six lines moved, all in the
// weighted_sum vault, all on the three entity-linked memories, each by -0.0011.
const cfGoldenPath = "testdata/content_first_differential.golden"

func cfDescribe(c *cfEnv, vault string, q string, limit int) string {
	c.t.Helper()
	resp, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{q}, MaxResults: limit, ReadOnly: true,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	ws := c.store.ResolveVaultPrefix(vault)
	var b strings.Builder
	fmt.Fprintf(&b, "%s | %q | limit=%d | abstained=%v %s\n", vault, q, limit, resp.Abstained, resp.AbstainedReason)
	for i, a := range resp.Activations {
		id, _ := storage.ParseULID(a.ID)
		eng, err := c.store.GetEngram(storage.ContextWithNoAccessCacheStamp(context.Background()), ws, id)
		if err != nil {
			c.t.Fatal(err)
		}
		fmt.Fprintf(&b, "  %2d %-40s score=%.4f abs=%.4f band=%s/%s\n", i, eng.Concept, a.Score, a.ScoreComponents.AbsoluteScore, a.RelevanceBand, a.RelevanceBandBasis)
	}
	return b.String()
}

// TestContentFirst_DifferentialAgainstTrunk — rrf and weighted_sum vaults
// (with association evidence primed: entity links, a Hebbian edge, a PAS
// transition and a recent activation), an ACT-R vault with NO association
// evidence, and nonsense / content-free queries on all three return exactly
// what the trunk returned: ids (by concept), order, scores, bands, abstention.
func TestContentFirst_DifferentialAgainstTrunk(t *testing.T) {
	c := newCFEnv(t)
	vaults := []struct{ name, fusion string }{{"trail-diff-rrf", "rrf"}, {"trail-diff-ws", "weighted_sum"}, {"trail-diff-plain", ""}}
	fixes := map[string]cfFix{}
	for _, v := range vaults {
		c.configure(v.name, v.fusion)
		if v.fusion == "" {
			// ACT-R with no association evidence at all: no entities, no
			// edges, no log — the content order is the trunk's order.
			h := time.Hour
			c.write(v.name, "culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two", 40*h)
			c.write(v.name, "riprap at the culvert outlet", "riprap placed at the culvert outlet for drainage", 38*h)
			c.write(v.name, "culvert drainage", "culvert drainage is slow after storms", 29*h)
			c.write(v.name, "outlet riprap", "riprap at an outlet upstream of the bridge", 28*h)
			c.write(v.name, "riprap supply", "riprap stone supply for the season", 12*h)
			c.write(v.name, "footbridge decking", "the footbridge decking was replaced with treated planks", 50*h)
			continue
		}
		fixes[v.name] = cfBuild(c, v.name)
	}
	// A fresh process, then the log prime: weighted_sum reads the cache clock,
	// so no stamping load may happen between the restart and the recalls. The
	// scoring clock is pinned (cfEnv.now), so time spent here cannot move a
	// score either.
	c.restart()
	for name, f := range fixes {
		c.primeLog(name, f.partner)
	}
	queries := []string{cfQuery, "riprap", "culvert outlet", "footbridge decking planks", "zyqx vrembolt quillet", "the of and"}
	var b strings.Builder
	for _, v := range vaults {
		for _, q := range queries {
			for _, limit := range []int{5, 20} {
				b.WriteString(cfDescribe(c, v.name, q, limit))
			}
		}
	}
	got := b.String()
	if os.Getenv("CF_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden written:\n%s", got)
		return
	}
	want, err := os.ReadFile(cfGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Errorf("differs from the trunk at line %d\ntrunk: %s\nnow:   %s", i+1, w, g)
			}
		}
	}
}
