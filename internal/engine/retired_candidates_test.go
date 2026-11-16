package engine

// Retired versions must not consume candidate slots, and a query worded like
// an OLD version must reach the CURRENT fact whatever the chain length. Every
// fixture is invented (trail maintenance, beekeeping); vectors are seeded
// random directions.

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// topicSpec is one cluster of the starvation fixture: rel relevant live
// memories around a topic plus one evolve chain of chainLen near-duplicate
// versions sitting right on it.
type topicSpec struct {
	rel, chainLen int
}

type builtTopic struct {
	rel   map[string]bool
	chain []string
	query []float32
}

func buildTopics(env *rcEnv, seed int64, bg int, specs []topicSpec) []builtTopic {
	env.t.Helper()
	r := rand.New(rand.NewSource(seed))
	var reqs []*mbp.WriteRequest
	for i := 0; i < bg; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "trail log " + strconv.Itoa(i),
			Content: "switchback inspection note number " + strconv.Itoa(i), Embedding: rcRand(r)})
	}
	env.writeBatch(reqs)
	out := make([]builtTopic, len(specs))
	for ti, sp := range specs {
		topic := rcRand(r)
		var rel []*mbp.WriteRequest
		for i := 0; i < sp.rel; i++ {
			rel = append(rel, &mbp.WriteRequest{Vault: "default", Concept: fmt.Sprintf("hive survey %d-%d", ti, i),
				Content: fmt.Sprintf("brood frame observation %d-%d", ti, i), Embedding: rcNear(r, topic, 0.8)})
		}
		bt := builtTopic{rel: map[string]bool{}}
		for _, id := range env.writeBatch(rel) {
			bt.rel[id] = true
		}
		if sp.chainLen > 0 {
			concept := fmt.Sprintf("apiary bookmark %d", ti)
			resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: concept,
				Content: concept + " version 0", Embedding: rcNear(r, topic, 0.25)})
			if err != nil {
				env.t.Fatal(err)
			}
			bt.chain = env.evolveChain(resp.ID, sp.chainLen, concept,
				func(i int) string { return concept + " version " + strconv.Itoa(i) },
				func(int) []float32 { return rcNear(r, topic, 0.25) })
		}
		bt.query = rcNear(rand.New(rand.NewSource(seed+int64(ti)+7)), topic, 0.25)
		out[ti] = bt
	}
	env.eng.waitWriteTimeIdle()
	return out
}

// TestRecall_RetiredVersionsDoNotConsumeVectorSlots is the 1B reproduction: a
// 2,000-memory vault (k = 45 per index), 20 relevant live memories around each
// of two topics, and an evolve chain of 50 resp. 500 near-duplicate versions
// on each topic. Before the fix the 50-chain took 44 of 45 vector slots (0 of
// 20 relevant returned) and the 500-chain took all of them (nothing returned
// at all, abstained superseded_only — even the head was lost).
func TestRecall_RetiredVersionsDoNotConsumeVectorSlots(t *testing.T) {
	// Hermetic (its own in-memory store and engine, no package state), so it
	// runs in parallel and its -race cost overlaps the package's other heavy tests.
	t.Parallel()
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()

	topics := buildTopics(env, 42, 1960, []topicSpec{{rel: 20, chainLen: 50}, {rel: 20, chainLen: 500}})

	for i, tp := range topics {
		t.Run(fmt.Sprintf("chain_%d", len(tp.chain)), func(t *testing.T) {
			resp := env.recallVec(tp.query, 21)
			got := 0
			head := tp.chain[len(tp.chain)-1]
			headSeen := false
			for _, a := range resp.Activations {
				if tp.rel[a.ID] {
					got++
				}
				if a.ID == head {
					headSeen = true
				}
			}
			if got != 20 {
				t.Errorf("topic %d (chain of %d): %d of 20 relevant live memories returned (results=%d, abstained=%q) — retired versions are consuming candidate slots",
					i, len(tp.chain), got, len(resp.Activations), resp.AbstainedReason)
			}
			if !headSeen {
				t.Errorf("topic %d: the chain head %s is missing from the results", i, head)
			}
			if resp.AbstainedReason != "" {
				t.Errorf("topic %d: abstained %q on a query with 20 relevant live memories", i, resp.AbstainedReason)
			}
		})
	}
}

// TestRecall_RetiredPostingsDoNotStarveLiveKeywordSlots is the FTS half: a
// superseded version keeps its postings (COG-28 needs them), and 60 versions
// that each match the query term in their CONCEPT out-score live documents that
// match it only in content. In a 1,100-memory vault the keyword cut is ~33, so
// before the fix every keyword slot went to a retired version.
func TestRecall_RetiredPostingsDoNotStarveLiveKeywordSlots(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()

	var reqs []*mbp.WriteRequest
	for i := 0; i < 1100; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "culvert survey " + strconv.Itoa(i),
			Content: "drainage check at marker " + strconv.Itoa(i) + " found the ditch clear"})
	}
	env.writeBatch(reqs)
	var live []*mbp.WriteRequest
	for i := 0; i < 20; i++ {
		live = append(live, &mbp.WriteRequest{Vault: "default", Concept: "shelter note " + strconv.Itoa(i),
			Content: "the lantern at shelter " + strconv.Itoa(i) + " needs a new mantle"})
	}
	liveIDs := map[string]bool{}
	for _, id := range env.writeBatch(live) {
		liveIDs[id] = true
	}
	resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "lantern lantern roster",
		Content: "lantern lantern rota version 0"})
	if err != nil {
		t.Fatal(err)
	}
	chain := env.evolveChain(resp.ID, 60, "lantern lantern roster",
		func(i int) string { return "lantern lantern rota version " + strconv.Itoa(i) }, nil)
	env.eng.waitWriteTimeIdle()

	out, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"lantern"}, MaxResults: 25})
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	headSeen := false
	for _, a := range out.Activations {
		if liveIDs[a.ID] {
			got++
		}
		if a.ID == chain[len(chain)-1] {
			headSeen = true
		}
	}
	if got != 20 {
		t.Errorf("%d of 20 live keyword matches returned (results=%d) — retired versions' postings took the keyword slots", got, len(out.Activations))
	}
	if !headSeen {
		t.Errorf("the chain head is missing from the keyword results")
	}
}

// oldWordingChain writes a chain of n versions in which ONLY version 0 carries
// the query's words, plus unrelated background, and returns the version ids.
func oldWordingChain(env *rcEnv, n int) []string {
	env.t.Helper()
	var bg []*mbp.WriteRequest
	for i := 0; i < 30; i++ {
		bg = append(bg, &mbp.WriteRequest{Vault: "default", Concept: "trail crew " + strconv.Itoa(i),
			Content: "waterbar rebuilt below the ridge at post " + strconv.Itoa(i)})
	}
	env.writeBatch(bg)
	resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "hive routine",
		Content: "Smoker fuel is pine needles packed loosely before each inspection."})
	if err != nil {
		env.t.Fatal(err)
	}
	ids := env.evolveChain(resp.ID, n, "hive routine",
		func(i int) string { return "Revision " + strconv.Itoa(i) + " of the routine: rotate frames weekly." }, nil)
	env.eng.waitWriteTimeIdle()
	return ids
}

const oldWordingQuery = "smoker fuel pine needles"

// TestVersionHead_OldVersionEvidenceSurvivesChainPastCap is C: a query that
// matches ONLY version 0 must return the current head carrying version 0's
// evidence, whatever the chain length. Before, the walk stopped at 8 hops, so
// at length 10+ the evidence was thrown away (abstained superseded_only, or the
// head surfaced only through an incidental weak match) and every such recall
// logged depth-cap WARNs — ~15 per recall on a 500-version chain.
func TestVersionHead_OldVersionEvidenceSurvivesChainPastCap(t *testing.T) {
	for _, n := range []int{10, 50, 500} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			env := openRCEnv(t, vfs.NewMem())
			defer env.stop()
			ids := oldWordingChain(env, n)
			head := ids[len(ids)-1]

			buf := captureWarn(t)
			resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 10})
			if err != nil {
				t.Fatal(err)
			}
			var row *mbp.ActivationItem
			for i := range resp.Activations {
				if resp.Activations[i].ID == head {
					row = &resp.Activations[i]
				}
			}
			if row == nil {
				t.Fatalf("chain of %d: head %s not returned (results=%d, abstained=%q)", n, head, len(resp.Activations), resp.AbstainedReason)
			}
			if row.SubstitutedFor != ids[0] {
				t.Errorf("chain of %d: substituted_for = %q, want version 0 %s", n, row.SubstitutedFor, ids[0])
			}
			if row.SubstitutionBasis == nil {
				t.Errorf("chain of %d: substitution_basis missing on a substituted row", n)
			}
			if row.ChainTruncated {
				t.Errorf("chain of %d: chain_truncated set on a fully walked chain", n)
			}
			if w := buf.String(); strings.Contains(w, "depth cap") || strings.Contains(w, "walk limit") {
				t.Errorf("chain of %d: walk-limit WARN logged: %q", n, w)
			}
		})
	}
}

// A fork PAST the former cap is still refused, by name. Before, the walk never
// reached it (it stopped at 8 hops) and the refusal was the generic
// superseded_only; now the walk reaches the fork and says ambiguous_version.
// The fork is written straight through the store: the evolve/link fork guards
// on this trunk refuse to create one, which is the point of them.
func TestVersionHead_ForkPastFormerCapIsAmbiguous(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()
	ids := oldWordingChain(env, 12)

	rival, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "hive routine rival", Content: "A rival branch: rotate frames fortnightly."})
	if err != nil {
		t.Fatal(err)
	}
	forkAt := env.mustULID(ids[10])
	if err := env.store.WriteAssociation(env.ctx, env.ws, env.mustULID(rival.ID), forkAt, &storage.Association{
		TargetID: forkAt, RelType: storage.RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()

	resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Activations {
		if a.SubstitutedFor != "" {
			t.Fatalf("a forked chain substituted %s — recall must not pick a branch", a.ID)
		}
	}
	if len(resp.Activations) == 0 && resp.AbstainedReason != activation.AbstainAmbiguousVersion {
		t.Errorf("abstained_reason = %q, want %q", resp.AbstainedReason, activation.AbstainAmbiguousVersion)
	}
}

// A chain whose head was forgotten keeps superseded_only, however long it is.
func TestVersionHead_ForgottenHeadOfLongChainStaysSupersededOnly(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()
	ids := oldWordingChain(env, 12)
	if _, err := env.eng.Forget(env.ctx, &mbp.ForgetRequest{Vault: "default", ID: ids[len(ids)-1]}); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()
	resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resp.Activations {
		if a.SubstitutedFor != "" {
			t.Fatalf("substituted %s for a chain whose head was forgotten", a.ID)
		}
	}
	if len(resp.Activations) == 0 && resp.AbstainedReason != activation.AbstainSupersededOnly {
		t.Errorf("abstained_reason = %q, want %q", resp.AbstainedReason, activation.AbstainSupersededOnly)
	}
}

// Historical recall still sees retired versions as ordinary candidates:
// include_invalid returns version 0 itself, and as_of inside version 2's window
// returns version 2. Neither path may route the version away as a "retired
// slot-taker".
func TestRecall_HistoricalViewsStillReturnRetiredVersions(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()

	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	first, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "hive routine",
		Content: "Smoker fuel is pine needles packed loosely before each inspection.", ValidFrom: &base})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.ID}
	cur := first.ID
	for i := 1; i < 12; i++ {
		nid, err := env.eng.EvolveAt(env.ctx, "default", cur, "Revision "+strconv.Itoa(i)+" of the routine: rotate frames weekly, burlap smoker fuel.",
			"rev", nil, "hive routine", nil, nil, base.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		cur = nid.String()
		ids = append(ids, cur)
	}
	// Background written AFTER the chain, enough that the candidate cut is a
	// real cut (k ≈ 33): the newest-first decay pool then holds background
	// notes, so the only way version 0 can come back is as an ordinary index
	// candidate — which is what this test pins.
	var bg []*mbp.WriteRequest
	for i := 0; i < 1100; i++ {
		bg = append(bg, &mbp.WriteRequest{Vault: "default", Concept: "culvert survey " + strconv.Itoa(i),
			Content: "drainage check at marker " + strconv.Itoa(i)})
	}
	env.writeBatch(bg)
	env.eng.waitWriteTimeIdle()

	inc, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 20, IncludeInvalid: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range inc.Activations {
		if a.ID == ids[0] {
			found = true
		}
	}
	if !found {
		t.Errorf("include_invalid did not return version 0 %s (got %d rows)", ids[0], len(inc.Activations))
	}

	asOf := base.Add(2*time.Hour + 30*time.Minute)
	hist, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"burlap smoker fuel"}, MaxResults: 20, AsOf: &asOf})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, a := range hist.Activations {
		if a.ID == ids[2] {
			found = true
		}
		if a.ID == ids[len(ids)-1] {
			t.Errorf("as_of returned the current head, which did not exist yet at that instant")
		}
	}
	if !found {
		t.Errorf("as_of %s did not return version 2 %s (got %d rows)", asOf, ids[2], len(hist.Activations))
	}
}

// recallFingerprint is what restart invariance compares: ids, order, absolute
// scores and the substitution attribution.
type rcRow struct {
	ID, SubstitutedFor string
	Score              float64
	Truncated          bool
}

func fingerprint(resp *mbp.ActivateResponse) (rows []rcRow, abstained string) {
	for _, a := range resp.Activations {
		rows = append(rows, rcRow{ID: a.ID, SubstitutedFor: a.SubstitutedFor, Score: float64(a.Score), Truncated: a.ChainTruncated})
	}
	return rows, resp.AbstainedReason
}

// TestRecall_RetiredSetRestartInvariance: the retired set is derived state, so
// a fresh process must produce byte-identical recalls — vector, keyword and
// old-wording substitution — over a vault with chains, forgotten and restored
// memories.
func TestRecall_RetiredSetRestartInvariance(t *testing.T) {
	fs := vfs.NewMem()
	env := openRCEnv(t, fs)
	topics := buildTopics(env, 11, 300, []topicSpec{{rel: 20, chainLen: 60}})
	ids := oldWordingChain(env, 20)
	// Forget a few relevant memories, restore one.
	n := 0
	var forgotten []string
	for id := range topics[0].rel {
		if n == 4 {
			break
		}
		n++
		if _, err := env.eng.Forget(env.ctx, &mbp.ForgetRequest{Vault: "default", ID: id}); err != nil {
			t.Fatal(err)
		}
		forgotten = append(forgotten, id)
	}
	sort.Strings(forgotten)
	if _, err := env.eng.Restore(env.ctx, "default", forgotten[0]); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()

	type probe struct {
		name string
		req  *mbp.ActivateRequest
	}
	probes := []probe{
		{"vector", &mbp.ActivateRequest{Vault: "default", Context: []string{"qqzzxq"}, Embedding: topics[0].query, MaxResults: 25, ReadOnly: true}},
		{"old-wording", &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 10, ReadOnly: true}},
		{"keyword", &mbp.ActivateRequest{Vault: "default", Context: []string{"brood frame observation"}, MaxResults: 25, ReadOnly: true}},
	}
	run := func(e *rcEnv) (map[string][]rcRow, map[string]string, int) {
		rows := map[string][]rcRow{}
		abst := map[string]string{}
		for _, p := range probes {
			resp, err := e.eng.Activate(e.ctx, p.req)
			if err != nil {
				t.Fatal(err)
			}
			rows[p.name], abst[p.name] = fingerprint(resp)
		}
		view, err := e.store.RetiredIDs(e.ctx, e.ws)
		if err != nil {
			t.Fatal(err)
		}
		return rows, abst, view.Len()
	}
	beforeRows, beforeAbst, beforeRetired := run(env)
	if sub := beforeRows["old-wording"]; len(sub) == 0 || sub[0].SubstitutedFor != ids[0] {
		t.Fatalf("fixture sanity: old-wording probe did not substitute the head before restart: %+v", sub)
	}
	env.stop()

	env2 := openRCEnv(t, fs)
	defer env2.stop()
	afterRows, afterAbst, afterRetired := run(env2)

	if beforeRetired != afterRetired {
		t.Errorf("retired set size %d before restart, %d after", beforeRetired, afterRetired)
	}
	for _, p := range probes {
		if !reflect.DeepEqual(beforeRows[p.name], afterRows[p.name]) || beforeAbst[p.name] != afterAbst[p.name] {
			t.Errorf("%s probe differs across restart:\n before %+v (%q)\n after  %+v (%q)",
				p.name, beforeRows[p.name], beforeAbst[p.name], afterRows[p.name], afterAbst[p.name])
		}
	}
}

// deriveRetired is the reference the maintained set is checked against: every
// engram the store holds whose RECORD (read through GetEngram, not the 0x0B
// index the set is built from) is soft-deleted or archived.
func deriveRetired(t testing.TB, env *rcEnv) map[storage.ULID]bool {
	t.Helper()
	out := map[storage.ULID]bool{}
	err := env.store.ScanEngrams(env.ctx, env.ws, func(eng *storage.Engram) error {
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			out[eng.ID] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertRetiredMatches(t testing.TB, env *rcEnv, step string) {
	t.Helper()
	want := deriveRetired(t, env)
	view, err := env.store.RetiredIDs(env.ctx, env.ws)
	if err != nil {
		t.Fatal(err)
	}
	if view.Len() != len(want) {
		t.Errorf("%s: retired set holds %d, store has %d retired", step, view.Len(), len(want))
	}
	for id := range want {
		if !view.Contains([16]byte(id)) {
			t.Errorf("%s: %s is retired in the store but not in the set", step, id)
		}
	}
}

// TestRetiredSet_MaintainedByLifecycleOps walks every lifecycle operation the
// engine exposes and checks, after each, that the maintained set equals a
// fresh derivation from the records — including that a restored or hard-
// deleted memory is NOT left behind (the dangerous direction: a live memory
// the set calls retired is invisible to recall).
func TestRetiredSet_MaintainedByLifecycleOps(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()
	ctx := env.ctx
	w := func(c string) string {
		resp, err := env.eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: c, Content: c + " notes for the bee yard"})
		if err != nil {
			t.Fatal(err)
		}
		return resp.ID
	}
	a, b, c, d := w("queen marking"), w("swarm trap"), w("varroa count"), w("honey super")
	// Build the set BEFORE any retirement so every step below exercises the
	// incremental hooks, not the lazy build.
	assertRetiredMatches(t, env, "initial")

	var aNext string
	steps := []struct {
		name string
		do   func()
		id   func() string
		want bool
	}{
		{"evolve retires the predecessor", func() {
			nid, err := env.eng.Evolve(ctx, "default", a, "queen marking v2", "rev", nil, "queen marking")
			if err != nil {
				t.Fatal(err)
			}
			aNext = nid.String()
		}, func() string { return a }, true},
		{"evolve leaves the successor live", func() {}, func() string { return aNext }, false},
		{"soft forget retires", func() {
			if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: b}); err != nil {
				t.Fatal(err)
			}
		}, func() string { return b }, true},
		{"restore un-retires", func() {
			if _, err := env.eng.Restore(ctx, "default", b); err != nil {
				t.Fatal(err)
			}
		}, func() string { return b }, false},
		{"archive retires", func() {
			if err := env.eng.UpdateLifecycleState(ctx, "default", c, "archived"); err != nil {
				t.Fatal(err)
			}
		}, func() string { return c }, true},
		{"un-archive un-retires", func() {
			if err := env.eng.UpdateLifecycleState(ctx, "default", c, "active"); err != nil {
				t.Fatal(err)
			}
		}, func() string { return c }, false},
		// Link(relation=supersedes) STAMPS the target's ValidUntil but leaves
		// it active (the validity-refused shape); it is not retired. Named
		// residual: such a target still competes for a candidate slot.
		{"link supersedes stamps but does not retire the target", func() {
			if _, err := env.eng.Link(ctx, &mbp.LinkRequest{Vault: "default", SourceID: c, TargetID: d,
				RelType: uint16(storage.RelSupersedes), Weight: 1}); err != nil {
				t.Fatal(err)
			}
		}, func() string { return d }, false},
		{"soft forget then hard forget leaves nothing", func() {
			if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: b}); err != nil {
				t.Fatal(err)
			}
			if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: b, Hard: true}); err != nil {
				t.Fatal(err)
			}
		}, func() string { return b }, false},
		{"hard delete of a retired predecessor removes it", func() {
			if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: a, Hard: true}); err != nil {
				t.Fatal(err)
			}
		}, func() string { return a }, false},
	}
	for _, s := range steps {
		s.do()
		view, err := env.store.RetiredIDs(ctx, env.ws)
		if err != nil {
			t.Fatal(err)
		}
		if got := view.Contains([16]byte(env.mustULID(s.id()))); got != s.want {
			t.Errorf("%s: retired=%v, want %v", s.name, got, s.want)
		}
		assertRetiredMatches(t, env, s.name)
	}

	// Hold something in the set going into the clear, so the clear has
	// something to drop.
	if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: c}); err != nil {
		t.Fatal(err)
	}
	assertRetiredMatches(t, env, "before clear")
	if err := env.eng.ClearVault(ctx, "default"); err != nil {
		t.Fatal(err)
	}
	view, err := env.store.RetiredIDs(ctx, env.ws)
	if err != nil {
		t.Fatal(err)
	}
	if view.Len() != 0 {
		t.Errorf("vault clear: retired set still holds %d", view.Len())
	}
	// A memory written after the clear, then forgotten, is tracked by the
	// rebuilt set.
	e := w("hive scale")
	if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: e}); err != nil {
		t.Fatal(err)
	}
	assertRetiredMatches(t, env, "after clear")
}

// TestRetiredSet_ConcurrentRecallAndEvolve runs recalls against a vault while
// one goroutine evolves a chain and another forgets and restores memories; the
// set must equal the store afterwards. Its value is under -race.
func TestRetiredSet_ConcurrentRecallAndEvolve(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()
	ctx := env.ctx
	r := rand.New(rand.NewSource(3))
	topic := rcRand(r)
	var reqs []*mbp.WriteRequest
	for i := 0; i < 120; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "frame " + strconv.Itoa(i),
			Content: "frame inspection " + strconv.Itoa(i), Embedding: rcNear(r, topic, 0.8)})
	}
	ids := env.writeBatch(reqs)
	head, err := env.eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: "yard bookmark", Content: "yard bookmark 0", Embedding: rcNear(r, topic, 0.25)})
	if err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()
	if _, err := env.store.RetiredIDs(ctx, env.ws); err != nil { // build first: exercise the hooks
		t.Fatal(err)
	}
	vecs := make([][]float32, 60)
	for i := range vecs {
		vecs[i] = rcNear(r, topic, 0.25)
	}
	q := rcNear(r, topic, 0.25)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		cur := head.ID
		for i := 1; i < len(vecs); i++ {
			nid, err := env.eng.Evolve(ctx, "default", cur, "yard bookmark "+strconv.Itoa(i), "rev", vecs[i], "yard bookmark")
			if err != nil {
				t.Error(err)
				return
			}
			cur = nid.String()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			id := ids[i]
			if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "default", ID: id}); err != nil {
				t.Error(err)
				return
			}
			if i%2 == 0 {
				// A refusal here ("not soft-deleted") is a PRE-EXISTING L1-cache
				// defect, not this set: a concurrent recall reading under its
				// pre-forget Pebble snapshot re-caches the engram's old ACTIVE
				// copy, and Restore decides from the cache. The set reads the
				// authoritative record and is checked against the store below,
				// so the refusal is logged, not failed.
				if _, err := env.eng.Restore(ctx, "default", id); err != nil {
					t.Logf("restore %s refused under concurrent recall: %v", id, err)
				}
			}
		}
	}()
	var rwg sync.WaitGroup
	for g := 0; g < 3; g++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := env.eng.Activate(ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"yard bookmark"}, Embedding: q, MaxResults: 10, ReadOnly: true}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	rwg.Wait()
	env.eng.waitWriteTimeIdle()
	assertRetiredMatches(t, env, "after concurrent evolve/forget/restore")
}

// countingHNSW / countingFTS record which search entry point phase 2 used.
type countingHNSW struct {
	inner      activation.HNSWIndex
	mu         sync.Mutex
	plain, liv int
}

func (c *countingHNSW) Search(ctx context.Context, ws [8]byte, vec []float32, k int) ([]activation.ScoredID, error) {
	c.mu.Lock()
	c.plain++
	c.mu.Unlock()
	return c.inner.Search(ctx, ws, vec, k)
}

func (c *countingHNSW) SearchLive(ctx context.Context, ws [8]byte, vec []float32, k int, retired func([16]byte) bool, budget int) ([]activation.ScoredID, []activation.ScoredID, error) {
	c.mu.Lock()
	c.liv++
	c.mu.Unlock()
	return c.inner.(activation.LiveHNSWIndex).SearchLive(ctx, ws, vec, k, retired, budget)
}

// TestRecall_VaultWithoutRetiredUsesPlainSearch pins "no regression for vaults
// with no retired versions" structurally: until something is retired, phase 2
// calls the unchanged Search — the retired-aware path is not merely equivalent
// there, it is not taken. Once a version is retired, it is.
func TestRecall_VaultWithoutRetiredUsesPlainSearch(t *testing.T) {
	var counter *countingHNSW
	env := openRCEnvWith(t, vfs.NewMem(), func(in activation.HNSWIndex) activation.HNSWIndex {
		counter = &countingHNSW{inner: in}
		return counter
	})
	defer env.stop()

	r := rand.New(rand.NewSource(5))
	var reqs []*mbp.WriteRequest
	for i := 0; i < 40; i++ {
		reqs = append(reqs, &mbp.WriteRequest{Vault: "default", Concept: "post " + strconv.Itoa(i), Content: "trail post " + strconv.Itoa(i), Embedding: rcRand(r)})
	}
	ids := env.writeBatch(reqs)
	env.eng.waitWriteTimeIdle()
	q := rcRand(r)
	env.recallVec(q, 10)
	if counter.liv != 0 || counter.plain != 1 {
		t.Fatalf("no retired engrams: Search=%d SearchLive=%d, want 1/0", counter.plain, counter.liv)
	}
	if _, err := env.eng.Evolve(env.ctx, "default", ids[0], "trail post 0 rebuilt", "rev", rcRand(r), "post 0"); err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()
	env.recallVec(q, 10)
	if counter.liv != 1 {
		t.Fatalf("one retired version: SearchLive=%d, want 1", counter.liv)
	}
}

// A walk that genuinely runs out of its limit — only possible through ACTIVE
// nodes joined by hand-written RelSupersedes edges, since every evolve
// intermediate is retired and counted into the limit — still substitutes,
// still flags chain_truncated on every recall, but WARNs once per chain per
// process instead of once per recall (the flood a production server showed).
func TestVersionHead_WalkLimitWarnsOncePerChain(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()
	ids := oldWordingChain(env, 2) // v0 retired by a real evolve; ids[1] active
	prev := ids[1]
	for i := 0; i < 12; i++ {
		resp, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "hand link " + strconv.Itoa(i),
			Content: "Hand-written successor " + strconv.Itoa(i) + " for the yard ledger."})
		if err != nil {
			t.Fatal(err)
		}
		// These memories are near-duplicates of each other, so the novelty
		// worker links each new one to an earlier one with `refines`, weighted
		// by their similarity (1.0 for some pairs). Let it finish before the
		// hand-written edge goes in, and give that edge a weight no similarity
		// lands on: an edge's RelType is not in its key, so `refines` and
		// `supersedes` at one weight on one pair are the same key (STO-15). This
		// test used to write at weight 1 without waiting, and lost about one run
		// in ten to that collision.
		env.eng.waitWriteTimeIdle()
		dst := env.mustULID(prev)
		if err := env.store.WriteAssociation(env.ctx, env.ws, env.mustULID(resp.ID), dst, &storage.Association{
			TargetID: dst, RelType: storage.RelSupersedes, Weight: 0.9, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		prev = resp.ID
	}
	env.eng.waitWriteTimeIdle()

	buf := captureWarn(t)
	for round := 0; round < 3; round++ {
		resp, err := env.eng.Activate(env.ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{oldWordingQuery}, MaxResults: 10, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		truncated := false
		for _, a := range resp.Activations {
			if a.SubstitutedFor == ids[0] && a.ChainTruncated {
				truncated = true
			}
		}
		if !truncated {
			t.Fatalf("round %d: expected a truncated substitution for %s, got %+v (abstained=%q)", round, ids[0], resp.Activations, resp.AbstainedReason)
		}
	}
	if n := strings.Count(buf.String(), "chain walk limit"); n != 1 {
		t.Errorf("walk-limit WARN logged %d times over 3 recalls of one chain, want exactly 1", n)
	}
}
