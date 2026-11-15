package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
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
// Honest clock, increment 1 — engine-level REDs.
//
// "Cold" here is a real restart: the engine and store are closed and reopened
// over the same Pebble directory, so the L1 read cache (whose timestamp ACT-R
// scoring used to read) is empty. "Warm" is the same store with every memory
// loaded through the cache. The persisted store is identical in both.
// Fixtures are invented (trail maintenance).
// ---------------------------------------------------------------------------

// hcEnv is an engine over a Pebble directory that can be restarted in place.
type hcEnv struct {
	t     *testing.T
	dir   string
	store *storage.PebbleStore
	fts   *fts.Index
	eng   *Engine
}

func newHCEnv(t *testing.T) *hcEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-honest-clock-*")
	if err != nil {
		t.Fatal(err)
	}
	h := &hcEnv{t: t, dir: dir}
	h.open()
	t.Cleanup(func() { h.close(); os.RemoveAll(dir) })
	return h
}

func (h *hcEnv) open() {
	h.t.Helper()
	db, err := storage.OpenPebble(h.dir, storage.DefaultOptions())
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	h.fts = fts.New(db)
	embedder := &noopEmbedder{}
	act := activation.New(h.store, &ftsAdapter{h.fts}, nil, embedder)
	trig := trigger.New(h.store, &ftsTrigAdapter{h.fts}, nil, embedder)
	h.eng = NewEngine(EngineConfig{Store: h.store, FTSIndex: h.fts, ActivationEngine: act, TriggerSystem: trig, Embedder: embedder})
}

func (h *hcEnv) close() {
	if h.eng == nil {
		return
	}
	h.eng.Stop()
	h.store.Close()
	h.eng = nil
}

// restart is a fresh process over the same persisted store.
func (h *hcEnv) restart() {
	h.t.Helper()
	h.close()
	h.open()
}

// write stores an engram of the given age directly at the store level (no
// cache stamp) and indexes it, exactly like coldEngram.
func (h *hcEnv) write(vault, concept, content string, age time.Duration, accessCount uint32, lastAccessAge time.Duration) storage.ULID {
	h.t.Helper()
	ws := h.store.ResolveVaultPrefix(vault)
	created := time.Now().Add(-age)
	la := created
	if lastAccessAge > 0 {
		la = time.Now().Add(-lastAccessAge)
	}
	id, err := h.store.WriteEngram(context.Background(), ws, &storage.Engram{
		Concept: concept, Content: content, Confidence: 1,
		CreatedAt: created, UpdatedAt: created, LastAccess: la, AccessCount: accessCount,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.fts.IndexEngram(ws, [16]byte(id), concept, "", content, nil); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.WriteVaultName(ws, vault); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// prime loads every given engram through the L1 cache (a stamping load), the
// state a long-running daemon's non-read-only traffic leaves behind.
func (h *hcEnv) prime(vault string, ids []storage.ULID) {
	h.t.Helper()
	ws := h.store.ResolveVaultPrefix(vault)
	for _, id := range ids {
		if _, err := h.store.GetEngram(context.Background(), ws, id); err != nil {
			h.t.Fatal(err)
		}
		if h.store.EngramLastAccessNs(ws, id) == 0 {
			h.t.Fatalf("prime: %s not stamped", id)
		}
	}
}

type hcRow struct {
	id         string
	score, abs float32
}

func (h *hcEnv) recall(vault, query string, mode string) []hcRow {
	h.t.Helper()
	resp, err := h.eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{query}, MaxResults: 10, ReadOnly: true, Mode: mode,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]hcRow, 0, len(resp.Activations))
	for _, a := range resp.Activations {
		out = append(out, hcRow{a.ID, a.Score, a.ScoreComponents.AbsoluteScore})
	}
	return out
}

func hcHas(rows []hcRow, id storage.ULID) bool {
	for _, r := range rows {
		if r.id == id.String() {
			return true
		}
	}
	return false
}

// hcFixture: one 400-day-old never-accessed memory plus fresher neighbours.
func hcFixture(h *hcEnv, vault string) (target storage.ULID, all []storage.ULID) {
	day := 24 * time.Hour
	target = h.write(vault, "waterbar drainage on switchback four",
		"the waterbar on switchback four drains to the left side of the tread", 400*day, 0, 0)
	all = append(all, target)
	all = append(all, h.write(vault, "blowdown on segment two", "crew cleared blowdown on segment two with a crosscut saw", 2*day, 0, 0))
	all = append(all, h.write(vault, "waterbar inventory", "eleven waterbars counted between the trailhead and the ridge", 90*day, 0, 0))
	all = append(all, h.write(vault, "bridge decking", "the footbridge decking was replaced with treated planks", 30*day, 0, 0))
	return target, all
}

// TestHonestClock_OldExactMatchReturnedCold — RED before this increment: a
// never-accessed 400-day-old exact match came back [] from a fresh process.
func TestHonestClock_OldExactMatchReturnedCold(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-cold"
	target, _ := hcFixture(h, vault)
	h.restart()
	rows := h.recall(vault, "waterbar drainage on switchback four", "")
	if !hcHas(rows, target) {
		t.Fatalf("a 400-day-old exact match was not returned from a cold engine: %v", rows)
	}
	if rows[0].id != target.String() {
		t.Errorf("the exact match is not rank 1 cold: %v", rows)
	}
}

// TestHonestClock_WarmEqualsCold — the same persisted store queried from a
// cold process and with a fully primed cache returns identical ids, order and
// absolute AND final scores. Read with ..._OldExactMatchReturnedCold: a fix
// that empties both regimes passes the equality, so non-emptiness and the
// target's presence are asserted here too.
func TestHonestClock_WarmEqualsCold(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-warm-cold"
	target, all := hcFixture(h, vault)
	queries := []string{"waterbar drainage on switchback four", "waterbar", "crosscut saw blowdown", "footbridge planks"}

	h.restart()
	cold := map[string][]hcRow{}
	for _, q := range queries {
		cold[q] = h.recall(vault, q, "")
	}
	if !hcHas(cold[queries[0]], target) {
		t.Fatalf("premise: cold recall of the exact text must return the target (else warm==cold is vacuous): %v", cold[queries[0]])
	}
	h.prime(vault, all)
	for _, q := range queries {
		warm := h.recall(vault, q, "")
		if len(warm) == 0 {
			t.Errorf("%q: empty warm result", q)
		}
		if fmt.Sprint(warm) != fmt.Sprint(cold[q]) {
			t.Errorf("%q: warm and cold differ\ncold %v\nwarm %v", q, cold[q], warm)
		}
	}
	h.restart()
	for _, q := range queries {
		if again := h.recall(vault, q, ""); fmt.Sprint(again) != fmt.Sprint(cold[q]) {
			t.Errorf("%q: differs across a second restart\nfirst %v\nnow   %v", q, cold[q], again)
		}
	}
}

// TestHonestClock_ReadOnlyCallsBetweenIdenticalRecalls — the identical
// read-only recall, issued twice with a read-only read, an explain and a
// where_left_off of a candidate in between, returns identical results — and
// so does a NON-read-only read (a stamping load), because scoring no longer
// reads the stamp at all.
func TestHonestClock_ReadOnlyCallsBetweenIdenticalRecalls(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-ro-between"
	target, _ := hcFixture(h, vault)
	// A fresh match, so the first recall is non-empty even under the old
	// clock and the comparison itself (not just the premise) is what fails.
	h.write(vault, "switchback four crew notes", "waterbar work on switchback four is scheduled", time.Hour, 0, 0)
	h.restart()
	ctx := context.Background()
	const q = "waterbar switchback"
	before := h.recall(vault, q, "")
	if len(before) == 0 {
		t.Fatalf("premise: recall returned nothing")
	}
	if _, err := h.eng.Read(ctx, &mbp.ReadRequest{Vault: vault, ID: target.String(), ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.Explain(ctx, vault, target.String(), []string{q}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.eng.WhereLeftOff(ctx, vault, 10, nil); err != nil {
		t.Fatal(err)
	}
	// A stamping load of the candidate: the shape that used to rescue it.
	if _, err := h.store.GetEngram(ctx, h.store.ResolveVaultPrefix(vault), target); err != nil {
		t.Fatal(err)
	}
	h.eng.WaitWriteTimeIdle()
	if after := h.recall(vault, q, ""); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("read-only calls (and a cache load) between two identical read-only recalls changed the result\nbefore %v\nafter  %v", before, after)
	}
}

// TestHonestClock_ExplainAgreesWithRecall — explain's reported decay factor is
// the APPLIED one (1.0 under ACT-R) and its would_return agrees with the next
// read-only recall, before and after; a cold 400-day exact match is both
// "would return" and returned.
func TestHonestClock_ExplainAgreesWithRecall(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-explain"
	target, _ := hcFixture(h, vault)
	h.restart()
	ctx := context.Background()
	const q = "waterbar drainage on switchback four"
	before := h.recall(vault, q, "")
	ex, err := h.eng.Explain(ctx, vault, target.String(), []string{q}, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := h.recall(vault, q, "")
	if !ex.Scored {
		t.Fatalf("explain did not score the target: %s", ex.Note)
	}
	if ex.Components.DecayFactor != 1 || ex.Components.Recency != 1 || ex.Components.AccessFrequency != 0 {
		t.Errorf("explain reports decay_factor=%.4f recency=%.4f access_frequency=%.4f on an ACT-R vault; applied values are 1, 1, 0",
			ex.Components.DecayFactor, ex.Components.Recency, ex.Components.AccessFrequency)
	}
	if ex.WouldReturn != hcHas(before, target) || ex.WouldReturn != hcHas(after, target) {
		t.Errorf("would_return=%v but recall before returned it=%v, after=%v", ex.WouldReturn, hcHas(before, target), hcHas(after, target))
	}
	if !ex.WouldReturn {
		t.Errorf("a cold 400-day exact match: would_return=false (abs %.4f vs threshold %.3f)", ex.Components.AbsoluteScore, ex.Threshold)
	}
	for _, r := range before {
		if r.id == target.String() && r.abs != ex.Components.AbsoluteScore {
			t.Errorf("explain abs %.6f != recall abs %.6f", ex.Components.AbsoluteScore, r.abs)
		}
	}
}

// TestHonestClock_RecallRowsDropDecayRelevanceUnderACTR — the stored
// Ebbinghaus `relevance` is not a recall input under ACT-R, so a recall row
// must not present it (mirrors `dormant`, already ACT-R-gated).
func TestHonestClock_RecallRowsDropDecayRelevanceUnderACTR(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-relevance"
	target, _ := hcFixture(h, vault)
	ws := h.store.ResolveVaultPrefix(vault)
	if err := h.store.UpdateRelevance(context.Background(), ws, target, 0.07, 30); err != nil {
		t.Fatal(err)
	}
	h.restart()
	resp, err := h.eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: vault, Context: []string{"waterbar drainage on switchback four"}, MaxResults: 10, ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range resp.Activations {
		if a.ID == target.String() {
			found = true
			if a.Relevance != 0 {
				t.Errorf("ACT-R recall row reports relevance %.3f, a decay number ACT-R does not apply", a.Relevance)
			}
			if a.ScoreComponents.DecayFactor != 1 || a.ScoreComponents.Recency != 1 {
				t.Errorf("ACT-R recall row score_components decay %.3f recency %.3f, applied values are 1", a.ScoreComponents.DecayFactor, a.ScoreComponents.Recency)
			}
		}
	}
	if !found {
		t.Fatalf("target not returned")
	}
}

// TestHonestClock_InertDecayWarnsOnce — reference/scratchpad/knowledge-graph
// carry a decay exponent recall scoring no longer reads, and an explicit
// actr_decay override is likewise inert. Explicit config is never silently
// ignored: one WARN per vault per process naming the setting; none for the
// default preset or for a preset whose decay equals the default.
func TestHonestClock_InertDecayWarnsOnce(t *testing.T) {
	e2, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()
	f64 := func(v float64) *float64 { return &v }
	str := func(s string) *string { return &s }
	cases := []struct {
		vault string
		cfg   *auth.PlasticityConfig
		warn  bool
	}{
		{"hc-default", nil, false},
		{"hc-working", &auth.PlasticityConfig{Preset: "working"}, false},
		{"hc-reference", &auth.PlasticityConfig{Preset: "reference"}, true},
		{"hc-scratchpad", &auth.PlasticityConfig{Preset: "scratchpad"}, true},
		{"hc-knowledge", &auth.PlasticityConfig{Preset: "knowledge-graph"}, true},
		{"hc-override", &auth.PlasticityConfig{ACTRDecay: f64(0.35)}, true},
		{"hc-rrf-reference", &auth.PlasticityConfig{Preset: "reference", ScoringFusion: str("rrf")}, false},
	}
	for _, c := range cases {
		if err := as.SetVaultConfig(auth.VaultConfig{Name: c.vault, Public: true, Plasticity: c.cfg}); err != nil {
			t.Fatalf("SetVaultConfig %s: %v", c.vault, err)
		}
		if err := store.WriteVaultName(store.VaultPrefix(c.vault), c.vault); err != nil {
			t.Fatal(err)
		}
	}
	buf := captureWarn(t)
	for i := 0; i < 3; i++ {
		for _, c := range cases {
			if _, err := e2.Activate(ctx, &mbp.ActivateRequest{Vault: c.vault, Context: []string{"waterbar"}, MaxResults: 5, ReadOnly: true}); err != nil {
				t.Fatalf("activate %s: %v", c.vault, err)
			}
		}
	}
	out := buf.String()
	for _, c := range cases {
		n := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "actr_decay") && strings.Contains(line, "vault="+c.vault+" ") {
				n++
			}
		}
		want := 0
		if c.warn {
			want = 1
		}
		if n != want {
			t.Errorf("%s: inert-decay WARN fired %d times over 3 recalls, want %d\n%s", c.vault, n, want, out)
		}
	}
	if !strings.Contains(out, "recall scoring") {
		t.Errorf("WARN must say recall scoring no longer uses the setting; got:\n%s", out)
	}
}

// TestHonestClock_InMemoryLogNoLongerReordersScoredRows replaces the honest
// clock's pinned KNOWN RESIDUAL (..._KnownResidual_InMemoryBoostsReorderAcrossARestart):
// after one unrelated normal recall, an associated row used to go to rank 1
// over an exact match, and a fresh process put the exact match back. Under
// content-first order (COG-35) the Hebbian and PAS boosts no longer order
// scored rows, so the scored order is the same fresh, after the unrelated
// recall, and after a restart. RED on the trunk (the associated row took
// rank 1 after the unrelated recall).
func TestHonestClock_InMemoryLogNoLongerReordersScoredRows(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-residual"
	ctx := context.Background()
	exact := h.write(vault, "waterbar drainage on switchback four", "the waterbar on switchback four drains to the left side", 90*24*time.Hour, 0, 0)
	partner := h.write(vault, "footbridge decking", "the footbridge decking was replaced with treated planks", 30*24*time.Hour, 0, 0)
	assoc := h.write(vault, "switchback four crew notes", "crew notes for switchback four", 60*24*time.Hour, 0, 0)
	if _, err := h.eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: partner.String(), TargetID: assoc.String(), Weight: 1.0}); err != nil {
		t.Fatal(err)
	}
	const q = "waterbar drainage on switchback four"
	order := func() string {
		t.Helper()
		resp, err := h.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{q}, MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		h.eng.activation.WaitLogIdle()
		if len(resp.Activations) == 0 {
			t.Fatal("no rows")
		}
		var b strings.Builder
		for _, a := range resp.Activations {
			fmt.Fprintf(&b, "%s:%.4f heb=%.3f ", a.ID, a.Score, a.ScoreComponents.HebbianBoost)
		}
		return b.String()
	}

	h.restart()
	fresh := order()
	if !strings.HasPrefix(fresh, exact.String()) {
		t.Fatalf("premise: a fresh process must rank the exact match first: %s", fresh)
	}
	h.restart()
	if _, err := h.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"footbridge decking planks"}, MaxResults: 10}); err != nil {
		t.Fatal(err)
	}
	h.eng.activation.WaitLogIdle()
	after := order()
	if !strings.Contains(after, assoc.String()) {
		t.Fatalf("premise: the associated row must be returned so its evidence is in play: %s", after)
	}
	strip := func(s string) string { // ids and scores only: the evidence fields MAY differ
		var ids []string
		for _, f := range strings.Fields(s) {
			if !strings.HasPrefix(f, "heb=") {
				ids = append(ids, f)
			}
		}
		return strings.Join(ids, " ")
	}
	if strip(after) != strip(fresh) {
		t.Errorf("the in-memory activation log reordered scored rows (COG-35 says it must not)\nfresh process: %s\nafter one unrelated recall: %s", fresh, after)
	}
	h.restart()
	if again := order(); strip(again) != strip(fresh) {
		t.Errorf("scored rows differ across a restart\nfirst: %s\nnow:   %s", fresh, again)
	}
}

// TestContentFirst_KnownResidual_TieOrderReadsTheInMemoryLog is a PINNED KNOWN
// RESIDUAL, not a guarantee: it documents current behaviour and FAILS WHEN IT
// IS FIXED, so the fix has to update COG-34/COG-35 with it.
//
// Under content-first order association evidence breaks EXACT content ties,
// and the Hebbian/PAS halves of that evidence read the in-memory activation
// log (written by every read_only=false recall, weighted by a wall clock,
// wiped by a restart). So two memories with identical content scores can swap
// places after an unrelated recall and swap back in a fresh process. Only exact
// ties are exposed; rows with different content scores cannot move (pinned by
// TestHonestClock_InMemoryLogNoLongerReordersScoredRows). Injected rows are
// ordered by entity evidence, which is persisted, so they are not exposed —
// except that on vaults over 1,000 memories a PAS transition can add a genuine
// content match the index cut missed to the candidate pool (named in COG-35).
func TestContentFirst_KnownResidual_TieOrderReadsTheInMemoryLog(t *testing.T) {
	known := map[string]string{
		"exact content ties are broken by log-dependent association evidence": "persist or window the activation log (Hebbian/PAS read side)",
	}
	h := newHCEnv(t)
	const vault = "trail-tie-residual"
	ctx := context.Background()
	older := h.write(vault, "waterbar on switchback", "waterbar on switchback", 20*time.Hour, 0, 0)
	newer := h.write(vault, "waterbar on switchback", "waterbar on switchback", 10*time.Hour, 0, 0)
	partner := h.write(vault, "footbridge decking", "the footbridge decking was replaced with treated planks", 50*time.Hour, 0, 0)
	for i := 0; i < 10; i++ {
		h.write(vault, fmt.Sprintf("brushing log %d", i), fmt.Sprintf("crew brushed the corridor on day %d", i), time.Duration(100+i)*time.Hour, 0, 0)
	}
	if _, err := h.eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: partner.String(), TargetID: older.String(), Weight: 1.0}); err != nil {
		t.Fatal(err)
	}
	first := func() string {
		t.Helper()
		resp, err := h.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"waterbar switchback"}, MaxResults: 10, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Activations) < 2 || resp.Activations[0].ScoreComponents.AbsoluteScore != resp.Activations[1].ScoreComponents.AbsoluteScore {
			t.Fatalf("premise: the twins must be the top two rows and tie exactly: %+v", resp.Activations)
		}
		return resp.Activations[0].ID
	}
	h.restart()
	if got := first(); got != newer.String() {
		t.Fatalf("premise: a fresh process breaks the tie newest-first, got %s", got)
	}
	if _, err := h.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{"footbridge decking planks"}, MaxResults: 10}); err != nil {
		t.Fatal(err)
	}
	h.eng.activation.WaitLogIdle()
	stillReorders := first() == older.String()
	for residual, followUp := range known {
		if !stillReorders {
			t.Errorf("KNOWN RESIDUAL APPEARS FIXED (%s): an exact tie no longer depends on the in-memory log — remove the allowlist entry and amend COG-34 / COG-35 [%s]", residual, followUp)
		}
	}
	if stillReorders && len(known) == 0 {
		t.Errorf("exact-tie order depends on the in-memory activation log but no residual is allowlisted")
	}
	h.restart()
	if got := first(); got != newer.String() {
		t.Errorf("a restart must restore newest-first on the tie, got %s", got)
	}
}

// TestWeakOnly_UncalibratedAndExplicitRequestsUnchanged — weak_only applies
// only where a measured noise floor makes "weak" mean something, and never
// overrides what the caller explicitly asked for.
func TestWeakOnly_UncalibratedAndExplicitRequestsUnchanged(t *testing.T) {
	weak := []string{activation.RelevanceWeak, activation.RelevanceWeak}
	cal := activation.RelevanceCalibration{FusionMode: activation.FusionACTR, SemanticBaseline: 0.52, ContentCeiling: 1}
	plain := &mbp.ActivateRequest{Context: []string{"q"}}
	if !weakOnlyAbstains(plain, nil, "", cal, false, weak) {
		t.Fatalf("control: a calibrated all-weak default recall must abstain")
	}
	for name, c := range map[string]struct {
		req        *mbp.ActivateRequest
		vaultMode  string
		cal        activation.RelevanceCalibration
		degraded   bool
		bands      []string
		structured bool
	}{
		"rrf":                {plain, "", activation.RelevanceCalibration{FusionMode: activation.FusionRRF, SemanticBaseline: 0.52}, false, weak, false},
		"weighted_sum":       {plain, "", activation.RelevanceCalibration{FusionMode: activation.FusionWeightedSum, SemanticBaseline: 0.52}, false, weak, false},
		"cgdn":               {plain, "", activation.RelevanceCalibration{FusionMode: activation.FusionCGDN, SemanticBaseline: 0.52}, false, weak, false},
		"semantic degraded":  {plain, "", cal, true, weak, false},
		"unregistered model": {plain, "", activation.RelevanceCalibration{FusionMode: activation.FusionACTR}, false, weak, false},
		"explicit threshold": {&mbp.ActivateRequest{Threshold: 0.1}, "", cal, false, weak, false},
		"include_weak":       {&mbp.ActivateRequest{IncludeWeak: true}, "", cal, false, weak, false},
		"tag filter":         {&mbp.ActivateRequest{Filters: []mbp.Filter{{Field: "tags_any", Op: "any", Value: []string{"x"}}}}, "", cal, false, weak, false},
		"structured filter":  {plain, "", cal, false, weak, true},
		"wire mode deep":     {&mbp.ActivateRequest{Mode: "deep"}, "", cal, false, weak, false},
		"vault default deep": {plain, "deep", cal, false, weak, false},
		"one moderate row":   {plain, "", cal, false, []string{activation.RelevanceModerate, activation.RelevanceWeak}, false},
		"one filter_match":   {plain, "", cal, false, []string{activation.RelevanceFilterMatch, activation.RelevanceWeak}, false},
		"one injected row":   {plain, "", cal, false, []string{activation.RelevanceUncalibrated, activation.RelevanceWeak}, false},
		"empty response":     {plain, "", cal, false, nil, false},
	} {
		var sf activation.EngramFilter
		if c.structured {
			sf = hcAnyFilter{}
		}
		if weakOnlyAbstains(c.req, sf, c.vaultMode, c.cal, c.degraded, c.bands) {
			t.Errorf("%s: weak_only must not apply", name)
		}
	}
}

type hcAnyFilter struct{}

func (hcAnyFilter) Match(*storage.Engram) bool { return true }

// TestWeakOnly_UnregisteredModelEndToEnd — the FTS-only engine has no
// measured noise floor, so its weak-looking rows are uncalibrated and still
// returned (behaviour unchanged).
func TestWeakOnly_UnregisteredModelEndToEnd(t *testing.T) {
	h := newHCEnv(t)
	const vault = "trail-uncal"
	hcFixture(h, vault)
	h.restart()
	if rows := h.recall(vault, "waterbar", ""); len(rows) == 0 {
		t.Fatalf("an uncalibrated response must not be withheld")
	}
}
