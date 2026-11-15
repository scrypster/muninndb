package engine

import (
	"bytes"
	"context"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/metrics"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// Invented fixture domain throughout: model-railway layout notes.

const twinDim = 32

type countingQueryEmbedder struct {
	calls atomic.Int64
}

func (c *countingQueryEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	c.calls.Add(1)
	return make([]float32, len(texts)*twinDim), nil
}
func (c *countingQueryEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

type twinTestEnv struct {
	t     *testing.T
	eng   *Engine
	store *storage.PebbleStore
	as    *auth.Store
	reg   *hnsw.Registry
	emb   *countingQueryEmbedder
}

func newTwinEnv(t *testing.T, mode writetwin.Mode, model string) *twinTestEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	reg := hnsw.NewRegistry(db)
	emb := &countingQueryEmbedder{}
	act := activation.New(store, &ftsAdapter{ftsIdx}, nil, emb)
	trig := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, emb)
	as := auth.NewStore(db)
	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: act,
		TriggerSystem: trig, Embedder: emb, HNSWRegistry: reg, EmbedModelName: model, WriteTwinMode: mode})
	t.Cleanup(func() { eng.Stop(); store.Close() })
	return &twinTestEnv{t: t, eng: eng, store: store, as: as, reg: reg, emb: emb}
}

func (env *twinTestEnv) optIn(vault string, inline bool, override *float64) {
	env.t.Helper()
	if err := env.as.SetVaultConfig(auth.VaultConfig{Name: vault, Plasticity: &auth.PlasticityConfig{
		Version: 1, WriteTwinInline: &inline, WriteTwinThreshold: override}}); err != nil {
		env.t.Fatal(err)
	}
}

// guardOK installs a measured, discriminating guard for vault — a direct seam
// into the structure, so inline tests need not write 500 engrams.
func (env *twinTestEnv) guardOK(vault string) {
	ws := env.store.ResolveVaultPrefix(vault)
	r := make([]float64, 600)
	for i := range r {
		r[i] = 0.5
	}
	env.eng.twin.mu.Lock()
	env.eng.twin.guards[ws] = &twinGuard{rank1: r, computedAt: time.Now()}
	env.eng.twin.mu.Unlock()
}

type idleFeed struct{}

func (idleFeed) PassStartedAt() (time.Time, bool) { return time.Time{}, false }

type busyFeed struct{ since time.Time }

func (b busyFeed) PassStartedAt() (time.Time, bool) { return b.since, true }

func (env *twinTestEnv) write(vault, concept, content string) storage.ULID {
	env.t.Helper()
	r, err := env.eng.Write(context.Background(), &mbp.WriteRequest{Vault: vault, Concept: concept, Content: content})
	if err != nil {
		env.t.Fatal(err)
	}
	id, err := storage.ParseULID(r.ID)
	if err != nil {
		env.t.Fatal(err)
	}
	return id
}

// embed plays the embed processor's post-store hook for id with vec.
func (env *twinTestEnv) embed(vault string, id storage.ULID, vec []float32) {
	env.t.Helper()
	ws := env.store.ResolveVaultPrefix(vault)
	eng, err := env.store.GetEngram(storage.ContextWithNoAccessCacheStamp(context.Background()), ws, id)
	if err != nil {
		env.t.Fatal(err)
	}
	env.eng.WriteTwinOnEmbedded(ws, eng, append([]float32(nil), vec...))
}

func twinUnit(seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	v := make([]float32, twinDim)
	var n float64
	for i := range v {
		x := rng.NormFloat64()
		v[i] = float32(x)
		n += x * x
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

// twinAt returns a unit vector whose cosine with unit v is c.
func twinAt(v []float32, c float64, seed int64) []float32 {
	u := twinUnit(seed)
	var d float64
	for i := range v {
		d += float64(u[i] * v[i])
	}
	var n float64
	for i := range u {
		u[i] -= float32(d) * v[i]
		n += float64(u[i] * u[i])
	}
	out := make([]float32, len(v))
	s := math.Sqrt(1 - c*c)
	for i := range v {
		out[i] = float32(c)*v[i] + float32(s)*u[i]/float32(math.Sqrt(n))
	}
	return out
}

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) lines(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range strings.Split(l.buf.String(), "\n") {
		if strings.Contains(ln, substr) {
			out = append(out, ln)
		}
	}
	return out
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lc, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lc
}

// inlineReady returns an env in mode 1 with vault opted in, a discriminating
// guard, and an idle embed feed.
func inlineReady(t *testing.T, vault string) *twinTestEnv {
	env := newTwinEnv(t, writetwin.Inline, "bge-small-en-v1.5")
	env.optIn(vault, true, nil)
	env.guardOK(vault)
	env.eng.SetWriteTwinFeed(idleFeed{})
	return env
}

// ---------------------------------------------------------------- tests ----

func TestWriteTwin_OffByDefault(t *testing.T) {
	m, ok := writetwin.ParseMode("")
	if !ok || m != writetwin.Off {
		t.Fatalf("unset MUNINN_WRITE_RELATED parses to %v (ok=%v), want off", m, ok)
	}
	if m, ok := writetwin.ParseMode("yes-please"); ok || m != writetwin.Off {
		t.Fatalf("an unrecognised value must run off and be reported: got %v ok=%v", m, ok)
	}
	env := newTwinEnv(t, EngineConfig{}.WriteTwinMode, "bge-small-en-v1.5")
	if env.eng.twin != nil {
		t.Fatal("a zero EngineConfig must leave the twin check off (no ring, no worker)")
	}
	env.optIn("layout", true, nil)
	a := env.write("layout", "turnout", "the yard turnout uses a code 83 frog")
	env.embed("layout", a, twinUnit(1)) // a no-op hook, never a panic
	b := env.write("layout", "turnout", "the yard turnout uses a code 83 frog, insulated")
	env.embed("layout", b, twinAt(twinUnit(1), 0.99, 2))
	if res := env.eng.WriteTwinInline(context.Background(), "layout", b.String(), nil); res != nil {
		t.Fatalf("off: WriteTwinInline must return nil (byte-identical responses), got %+v", res)
	}
}

func TestWriteTwin_InlineNeverDelaysWriteBeyondN(t *testing.T) {
	env := inlineReady(t, "layout")
	var asked []time.Duration
	env.eng.twin.after = func(d time.Duration) <-chan time.Time {
		asked = append(asked, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now() // the budget elapses at once: no vector ever arrives
		return ch
	}
	w := env.write("layout", "helix", "the helix climbs two percent per turn")
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Unavailable != twinEmbedTimeout {
		t.Fatalf("no vector within N must answer embed_timeout, got %+v", res)
	}
	if len(asked) != 1 || asked[0] != twinMaxInlineWait {
		t.Fatalf("with no measurement the wait must be the 100ms cap, asked %v", asked)
	}
	// N = min(100ms, 2 × measured delivery).
	env.eng.twin.mu.Lock()
	env.eng.twin.ewma = 20 * time.Millisecond
	env.eng.twin.mu.Unlock()
	if got := env.eng.twin.inlineWaitBudget(); got != 40*time.Millisecond {
		t.Fatalf("budget after a 20ms measurement = %v, want 40ms", got)
	}
	env.eng.twin.mu.Lock()
	env.eng.twin.ewma = 80 * time.Millisecond
	env.eng.twin.mu.Unlock()
	if got := env.eng.twin.inlineWaitBudget(); got != twinMaxInlineWait {
		t.Fatalf("budget after an 80ms measurement = %v, want the 100ms cap", got)
	}
	// The write itself stands.
	if _, err := env.eng.Read(context.Background(), &mbp.ReadRequest{Vault: "layout", ID: w.String()}); err != nil {
		t.Fatalf("the committed write must be readable: %v", err)
	}
}

func TestWriteTwin_ReturnsBusyInsteadOfQueueingBehindABatch(t *testing.T) {
	env := inlineReady(t, "layout")
	// The embed processor has been mid-pass since before this write: the
	// write's vector is queued behind earlier work.
	env.eng.SetWriteTwinFeed(busyFeed{since: time.Now().Add(-2 * time.Second)})
	env.eng.twin.after = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	before := testutil.ToFloat64(metrics.WriteTwinEmbedSkippedBusyTotal)
	w := env.write("layout", "helix", "the helix climbs two percent per turn")
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Unavailable != twinEmbedderBusy {
		t.Fatalf("queued behind an earlier pass must answer embedder_busy, got %+v", res)
	}
	if got := testutil.ToFloat64(metrics.WriteTwinEmbedSkippedBusyTotal) - before; got != 1 {
		t.Fatalf("embed_skipped_busy counter moved by %v, want 1", got)
	}
	if n := env.emb.calls.Load(); n != 0 {
		t.Fatalf("the inline path must never call an embedder; it made %d calls", n)
	}
}

func TestWriteTwin_PanicAfterCommitStillReturnsTheWrite(t *testing.T) {
	env := inlineReady(t, "layout")
	w := env.write("layout", "helix", "the helix climbs two percent per turn")
	env.embed("layout", w, twinUnit(3))
	env.eng.twin.testAfterVector = func() { panic("injected lookup fault") }
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Unavailable != twinInternalError {
		t.Fatalf("a panic in the lookup must surface as unavailable internal_error, got %+v", res)
	}
	if _, err := env.eng.Read(context.Background(), &mbp.ReadRequest{Vault: "layout", ID: w.String()}); err != nil {
		t.Fatalf("the committed write must be unaffected: %v", err)
	}
}

func TestWriteTwin_NamesOnlyChainHeads(t *testing.T) {
	env := inlineReady(t, "layout")
	ctx := context.Background()
	base := twinUnit(10)
	cur := env.write("layout", "branch line", "the branch line runs on DCC address 3")
	env.embed("layout", cur, twinAt(base, 0.97, 100))
	chain := []storage.ULID{cur}
	for i := 1; i < 12; i++ {
		next, err := env.eng.Evolve(ctx, "layout", cur.String(),
			"the branch line runs on DCC address "+string(rune('3'+i%7))+" after re-mapping "+string(rune('a'+i)),
			"re-mapped", nil, "branch line")
		if err != nil {
			t.Fatalf("evolve %d: %v", i, err)
		}
		env.embed("layout", next, twinAt(base, 0.97, int64(100+i)))
		chain = append(chain, next)
		cur = next
	}
	head := cur
	gone := env.write("layout", "branch line", "the branch line uses DCC address 3 on the old controller")
	env.embed("layout", gone, twinAt(base, 0.99, 200))
	if _, err := env.eng.Forget(ctx, &mbp.ForgetRequest{Vault: "layout", ID: gone.String()}); err != nil {
		t.Fatal(err)
	}
	w := env.write("layout", "branch line", "branch line: DCC address after the latest re-mapping")
	env.embed("layout", w, base)

	res := env.eng.WriteTwinInline(ctx, "layout", w.String(), nil)
	if res == nil || res.Unavailable != "" {
		t.Fatalf("inline check: %+v", res)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].ID != head.String() {
		var got []string
		for _, c := range res.Candidates {
			got = append(got, c.ID)
		}
		t.Fatalf("12 versions + a forgotten twin must name exactly the live head %s once, named %v (retired: %v, forgotten: %s)",
			head, got, chain[:11], gone)
	}
}

func TestWriteTwin_DoesNotStampOrCountAsAccess(t *testing.T) {
	env := inlineReady(t, "layout")
	ctx := context.Background()
	ws := env.store.ResolveVaultPrefix("layout")
	e1 := env.write("layout", "scenery", "the mountain backdrop is painted on hardboard")
	env.embed("layout", e1, twinUnit(20))
	w := env.write("layout", "scenery", "the mountain backdrop is painted on tempered hardboard")
	env.eng.WaitWriteTimeIdle()

	nostamp := storage.ContextWithNoAccessCacheStamp(ctx)
	before, err := env.store.GetEngram(nostamp, ws, e1)
	if err != nil {
		t.Fatal(err)
	}
	stampBefore := env.store.EngramLastAccessNs(ws, e1)
	events := func() int {
		n := 0
		for _, p := range []storage.RecallEventPurpose{storage.RecallPurposeLabel, storage.RecallPurposeCalibration, storage.RecallPurposeTrial} {
			_ = env.store.ScanRecallEvents(ctx, ws, p, func(storage.ULID, *storage.RecallEvent) error { n++; return nil })
		}
		return n
	}
	evBefore := events()

	env.embed("layout", w, twinAt(twinUnit(20), 0.98, 21)) // shadow job queued too
	res := env.eng.WriteTwinInline(ctx, "layout", w.String(), nil)
	if res == nil || len(res.Candidates) != 1 || res.Candidates[0].ID != e1.String() {
		t.Fatalf("precondition: the check must name e1, got %+v", res)
	}
	env.eng.WaitWriteTimeIdle()

	after, err := env.store.GetEngram(nostamp, ws, e1)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccessCount != before.AccessCount || !after.LastAccess.Equal(before.LastAccess) {
		t.Fatalf("naming a candidate counted as access: count %d→%d, last %v→%v",
			before.AccessCount, after.AccessCount, before.LastAccess, after.LastAccess)
	}
	if got := env.store.EngramLastAccessNs(ws, e1); got != stampBefore {
		t.Fatalf("naming a candidate stamped the L1 cache recency: %d→%d", stampBefore, got)
	}
	if got := events(); got != evBefore {
		t.Fatalf("naming a candidate wrote recall events: %d→%d", evBefore, got)
	}
	if n := env.emb.calls.Load(); n != 0 {
		t.Fatalf("the check ran the recall embedder (%d calls) — it must not touch the scoring pipeline", n)
	}
}

func TestWriteTwin_IndependentOfActivationScoring(t *testing.T) {
	env := inlineReady(t, "layout")
	ctx := context.Background()
	ws := env.store.ResolveVaultPrefix("layout")
	base := twinUnit(30)
	a := env.write("layout", "signals", "the home signal at the station is a two-aspect searchlight")
	env.embed("layout", a, twinAt(base, 0.97, 31))
	b := env.write("layout", "signals", "station home signal: two-aspect searchlight head")
	env.embed("layout", b, twinAt(base, 0.95, 32))
	// Make B everything the scoring pipeline would prefer: heavily used,
	// highly confident. The twin check must not care.
	for i := 0; i < 30; i++ {
		_ = env.store.TouchAccess(ctx, ws, b)
	}
	w := env.write("layout", "signals", "the station home signal is a searchlight with two aspects")
	env.embed("layout", w, base)
	res := env.eng.WriteTwinInline(ctx, "layout", w.String(), nil)
	if res == nil || len(res.Candidates) != 2 {
		t.Fatalf("want both candidates, got %+v", res)
	}
	if res.Candidates[0].ID != a.String() || res.Candidates[1].ID != b.String() {
		t.Fatalf("order must follow similarity alone (a .97 before b .95), got %s, %s", res.Candidates[0].ID, res.Candidates[1].ID)
	}
	if math.Abs(res.Candidates[0].Similarity-0.97) > 1e-4 || math.Abs(res.Candidates[1].Similarity-0.95) > 1e-4 {
		t.Fatalf("similarity must be the raw cosine, got %.5f / %.5f", res.Candidates[0].Similarity, res.Candidates[1].Similarity)
	}
}

func TestWriteTwin_HomogeneousVaultGoesShadowAndSaysSo(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, spread float64) string {
		env := newTwinEnv(t, writetwin.Inline, "bge-small-en-v1.5")
		env.optIn("layout", true, nil)
		env.eng.SetWriteTwinFeed(idleFeed{})
		env.eng.twin.guardMinSample = 20 // the store wiring, not the 500 constant, is under test
		base := twinUnit(40)
		for i := 0; i < 30; i++ {
			vec := twinAt(base, spread, int64(400+i))
			if spread < 0.5 {
				vec = twinUnit(int64(4000 + i))
			}
			if _, err := env.eng.Write(ctx, &mbp.WriteRequest{Vault: "layout", Concept: "wagon",
				Content: "wagon card " + string(rune('A'+i)) + ": a forty-foot boxcar for the interchange", Embedding: vec}); err != nil {
				t.Fatal(err)
			}
		}
		w := env.write("layout", "wagon", "wagon card: a new forty-foot boxcar")
		first := env.eng.WriteTwinInline(ctx, "layout", w.String(), nil)
		if first == nil || first.Unavailable != twinCalibrating {
			t.Fatalf("before any measurement the vault must say calibrating, got %+v", first)
		}
		env.eng.WaitWriteTimeIdle() // the guard computation has finished
		res := env.eng.WriteTwinInline(ctx, "layout", w.String(), nil)
		if res == nil {
			t.Fatal("nil result")
		}
		return res.Unavailable
	}
	if got := run(t, 0.97); got != writetwin.GuardTooHomogeneous {
		t.Fatalf("a vault whose ordinary writes already clear T must stay in shadow and say too_homogeneous, got %q", got)
	}
	if got := run(t, 0); got == writetwin.GuardTooHomogeneous || got == twinCalibrating {
		t.Fatalf("a diverse vault must pass the guard, got %q", got)
	}
}

func TestWriteTwin_UnknownModelHasNoHint(t *testing.T) {
	if v, ok := embedpkg.WriteTwinHint("bge-small-en-v1.5"); !ok || v != 0.93 {
		t.Fatalf("bge-small hint = %v ok=%v, want 0.93", v, ok)
	}
	env := newTwinEnv(t, writetwin.Inline, "unregistered-model-x")
	env.optIn("layout", true, nil)
	env.eng.SetWriteTwinFeed(idleFeed{})
	w := env.write("layout", "helix", "the helix climbs two percent per turn")
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Unavailable != twinNoModelHint {
		t.Fatalf("an unregistered model must get no hint (no_model_hint), got %+v", res)
	}
	lc := captureLogs(t)
	before := testutil.ToFloat64(metrics.WriteTwinUnavailableTotal.WithLabelValues(twinNoModelHint))
	a := env.write("layout", "turnout", "the yard turnout uses a code 83 frog")
	env.embed("layout", a, twinUnit(1))
	b := env.write("layout", "turnout", "the yard turnout uses a code 83 insulated frog")
	env.embed("layout", b, twinAt(twinUnit(1), 0.99, 2))
	env.eng.WaitWriteTimeIdle()
	if got := testutil.ToFloat64(metrics.WriteTwinUnavailableTotal.WithLabelValues(twinNoModelHint)) - before; got != 2 {
		t.Fatalf("the shadow worker must count no_model_hint per write (2), counted %v", got)
	}
	if l := lc.lines(writetwin.FlagLogMsg); len(l) != 0 {
		t.Fatalf("no hint, no rule: nothing may be flagged, got %v", l)
	}
}

func TestWriteTwin_OverrideWinsAndIsClamped(t *testing.T) {
	low, high := 0.2, 0.96
	env := inlineReady(t, "layout")
	env.optIn("layout", true, &low)
	w := env.write("layout", "helix", "the helix climbs two percent per turn")
	env.embed("layout", w, twinUnit(5))
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Threshold != writetwin.OverrideMin || res.ThresholdSource != "override" {
		t.Fatalf("override 0.2 must clamp to %.2f with source override, got %+v", writetwin.OverrideMin, res)
	}
	env.optIn("layout", true, &high)
	res = env.eng.WriteTwinInline(context.Background(), "layout", w.String(), nil)
	if res == nil || res.Threshold != 0.96 || res.ThresholdSource != "override" {
		t.Fatalf("an in-range override must win over the 0.93 model hint, got %+v", res)
	}
}

func TestWriteTwin_IntraBatchPairsLoggedNotShown(t *testing.T) {
	env := newTwinEnv(t, writetwin.Shadow, "bge-small-en-v1.5")
	lc := captureLogs(t)
	resps, errs := env.eng.WriteBatch(context.Background(), []*mbp.WriteRequest{
		{Vault: "layout", Concept: "lighting", Content: "the street lamps run on a 12V bus"},
		{Vault: "layout", Concept: "lighting", Content: "street lamps are wired to the 12-volt bus"},
	})
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, _ := storage.ParseULID(resps[0].ID)
	b, _ := storage.ParseULID(resps[1].ID)
	env.embed("layout", a, twinUnit(50))
	env.embed("layout", b, twinAt(twinUnit(50), 0.98, 51))
	env.eng.WaitWriteTimeIdle()
	lines := lc.lines(writetwin.FlagLogMsg)
	if len(lines) != 1 {
		t.Fatalf("want exactly one flag line for the batch pair, got %d: %v", len(lines), lines)
	}
	l := lines[0]
	for _, want := range []string{"rule=batch", "shown=false", "batch_call=true", "new_id=" + b.String(), "head_id=" + a.String()} {
		if !strings.Contains(l, want) {
			t.Fatalf("flag line missing %q: %s", want, l)
		}
	}
}

func TestWriteTwin_NoContentOrVaultNameInLabelsOrLog(t *testing.T) {
	env := newTwinEnv(t, writetwin.Shadow, "bge-small-en-v1.5")
	lc := captureLogs(t)
	const vault = "riverside-depot-layout"
	a := env.write(vault, "coaling tower", "the coaling tower kit needs weathering powder")
	env.embed(vault, a, twinUnit(60))
	b := env.write(vault, "coaling tower", "coaling tower kit: weather it with powder")
	env.embed(vault, b, twinAt(twinUnit(60), 0.97, 61))
	env.eng.WaitWriteTimeIdle()
	lines := lc.lines(writetwin.FlagLogMsg)
	if len(lines) != 1 {
		t.Fatalf("want one shadow flag line, got %v", lines)
	}
	for _, leak := range []string{vault, "coaling", "weathering", "powder", "tower"} {
		if strings.Contains(lines[0], leak) {
			t.Fatalf("flag log line leaks %q: %s", leak, lines[0])
		}
	}
	ws := env.store.ResolveVaultPrefix(vault)
	if !strings.Contains(lines[0], "vault_ws="+writetwin.HexWS(ws)) {
		t.Fatalf("flag line must identify the vault by workspace hex only: %s", lines[0])
	}
	allowed := map[string]map[string]bool{
		"path":    {"retro": true, "inline": true},
		"outcome": {"flagged": true, "none": true, "unavailable": true},
		"rule":    {"recent": true, "batch": true, "historic": true},
		"shown":   {"true": true, "false": true},
		"reason": {twinEmbedderBusy: true, twinEmbedTimeout: true, twinNoModelHint: true, twinNoEmbedder: true,
			twinCalibrating: true, twinInternalError: true, writetwin.GuardColdStart: true, writetwin.GuardTooHomogeneous: true},
	}
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "muninn_write_twin_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				seen++
				vals, ok := allowed[lp.GetName()]
				if !ok || !vals[lp.GetValue()] {
					t.Fatalf("metric %s carries label %s=%q outside its closed enum", mf.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no write-twin metric labels were gathered — the check above never looked")
	}
}

// TestWriteTwin_RingRaceConcurrentWritesAndClear is for -race: hook inserts,
// inline scans, and a vault clear interleave freely.
func TestWriteTwin_RingRaceConcurrentWritesAndClear(t *testing.T) {
	env := inlineReady(t, "layout")
	ctx := context.Background()
	var ids []storage.ULID
	for i := 0; i < 24; i++ {
		ids = append(ids, env.write("layout", "yard", "yard ladder track "+string(rune('a'+i))+" holds six wagons"))
	}
	ws := env.store.ResolveVaultPrefix("layout")
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < len(ids); i += 4 {
				env.embed("layout", ids[i], twinAt(twinUnit(70), 0.96, int64(700+i)))
				_ = env.eng.WriteTwinInline(ctx, "layout", ids[i].String(), nil)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			env.eng.resetTwinVault(ws)
		}
	}()
	wg.Wait()
	env.eng.WaitWriteTimeIdle()
	env.eng.resetTwinVault(ws)
	if n := env.eng.twin.ring.Len(ws); n != 0 {
		t.Fatalf("after a clear the vault's ring must be empty, holds %d", n)
	}
}

// TestWriteTwin_ShownLogMatchesRender: when the caller can show only the
// first candidate (the byte cap), the log says shown=true for that one and
// shown=false for the other — never a display that did not happen.
func TestWriteTwin_ShownLogMatchesRender(t *testing.T) {
	env := inlineReady(t, "layout")
	lc := captureLogs(t)
	base := twinUnit(80)
	a := env.write("layout", "ballast", "mainline ballast is fine grey granite")
	env.embed("layout", a, twinAt(base, 0.97, 81))
	b := env.write("layout", "ballast", "the mainline uses fine grey granite ballast")
	env.embed("layout", b, twinAt(base, 0.95, 82))
	w := env.write("layout", "ballast", "fine grey granite ballast on the mainline")
	env.embed("layout", w, base)
	env.eng.WaitWriteTimeIdle()
	res := env.eng.WriteTwinInline(context.Background(), "layout", w.String(), func(*WriteTwinResult) int { return 1 })
	if res == nil || len(res.Candidates) != 2 {
		t.Fatalf("want two candidates, got %+v", res)
	}
	// (The shadow worker also logged both, shown=false, from the hook above;
	// what matters is that b is never claimed shown.)
	var shownA, shownB bool
	for _, l := range lc.lines(writetwin.FlagLogMsg) {
		if !strings.Contains(l, "new_id="+w.String()) || !strings.Contains(l, "shown=true") {
			continue
		}
		shownA = shownA || strings.Contains(l, "head_id="+a.String())
		shownB = shownB || strings.Contains(l, "head_id="+b.String())
	}
	if !shownA || shownB {
		t.Fatalf("log must say a shown and never b (shownA=%v shownB=%v): %v", shownA, shownB, lc.lines(writetwin.FlagLogMsg))
	}
}
