package writetwin

import (
	"math"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// rank1 builds a guard sample: n values at base plus k "templated" values.
func rank1(n int, base float64, k int, templated float64) []float64 {
	out := make([]float64, 0, n+k)
	for i := 0; i < n; i++ {
		out = append(out, base+0.0001*float64(i%50))
	}
	for i := 0; i < k; i++ {
		out = append(out, templated)
	}
	return out
}

func TestWriteTwin_HomogeneousVaultGoesShadowAndSaysSo_Rule(t *testing.T) {
	// A homogeneous corpus: ordinary writes' nearest-earlier neighbours sit at
	// ≈0.95, so a fixed 0.93 would flag nearly every write.
	st, share := GuardVerdict(rank1(600, 0.95, 0, 0), 0.93)
	if st != GuardTooHomogeneous || share < 0.9 {
		t.Fatalf("homogeneous sample: status %q share %.2f, want too_homogeneous", st, share)
	}
	// Under 500 values is cold start, never a pass.
	if st, _ := GuardVerdict(rank1(499, 0.5, 0, 0), 0.93); st != GuardColdStart {
		t.Fatalf("499 samples: status %q, want cold_start", st)
	}
	// NaN (no earlier neighbour) never counts as a would-flag.
	nan := rank1(600, 0.5, 0, 0)
	for i := 0; i < 100; i++ {
		nan[i] = math.NaN()
	}
	if st, _ := GuardVerdict(nan, 0.93); st != GuardOK {
		t.Fatalf("NaN entries counted as flags: %q", st)
	}
}

func TestWriteTwin_GuardRobustToFewTemplatedWrites(t *testing.T) {
	// 600 ordinary writes plus 20 templated near-copies at 0.99: the
	// would-flag rate is 20/620 = 3.2%, under the 5% budget — the guard must
	// still call the vault discriminating.
	sample := rank1(600, 0.6, 20, 0.99)
	st, share := GuardVerdict(sample, 0.93)
	if st != GuardOK {
		t.Fatalf("20 templated writes in 620 flipped the guard to %q (share %.3f)", st, share)
	}
	// v2's statistic — an extreme quantile of a small sample — would have
	// been moved by those same few writes: its q99 of the last 200 values is
	// one of the templated ones.
	last := append([]float64(nil), sample[len(sample)-200:]...)
	sort.Float64s(last)
	if q99 := last[int(0.99*float64(len(last)-1))]; q99 < 0.93 {
		t.Fatalf("fixture sanity: the old q99 statistic should be dominated by the templated writes, got %.3f", q99)
	}
	// And the boundary is the stated 5 per 100, not "any".
	if st, _ := GuardVerdict(rank1(570, 0.6, 30, 0.99), 0.93); st != GuardOK {
		t.Fatalf("exactly 5%% must pass (the budget is 'exceeds'), got %q", st)
	}
	if st, _ := GuardVerdict(rank1(569, 0.6, 31, 0.99), 0.93); st != GuardTooHomogeneous {
		t.Fatalf("just over 5%% must trip, got %q", st)
	}
}

func TestWriteTwin_UnknownModelHasNoHint_Rule(t *testing.T) {
	hint := func(m string) (float64, bool) {
		if m == "bge-small-en-v1.5" {
			return 0.93, true
		}
		return 0, false
	}
	if th := ResolveThreshold(nil, "some-other-model", hint); th.OK || th.Value != 0 {
		t.Fatalf("an unregistered model must get no value at all, got %+v", th)
	}
	if th := ResolveThreshold(nil, "", hint); th.OK {
		t.Fatalf("an unknown (empty) model must get no value, got %+v", th)
	}
	if th := ResolveThreshold(nil, "bge-small-en-v1.5", hint); !th.OK || th.Value != 0.93 || th.Source != "model_hint:bge-small-en-v1.5" {
		t.Fatalf("registered model: %+v", th)
	}
}

func TestWriteTwin_OverrideWinsAndIsClamped_Rule(t *testing.T) {
	hint := func(string) (float64, bool) { return 0.93, true }
	for _, c := range []struct{ in, want float64 }{
		{0.2, OverrideMin}, {1.5, OverrideMax}, {math.NaN(), OverrideMax}, {0.95, 0.95}, {0.5, 0.5}, {0.999, 0.999},
	} {
		in := c.in
		th := ResolveThreshold(&in, "bge-small-en-v1.5", hint)
		if !th.OK || th.Source != "override" || th.Value != c.want {
			t.Fatalf("override %v: got %+v, want %v from override", c.in, th, c.want)
		}
		r := auth.ResolvePlasticity(&auth.PlasticityConfig{WriteTwinThreshold: &in})
		if r.WriteTwinThresholdOverride == nil || *r.WriteTwinThresholdOverride != c.want {
			t.Fatalf("plasticity resolution of %v = %v, want %v (COG-2: clamped, never rejected)", c.in, r.WriteTwinThresholdOverride, c.want)
		}
	}
	if r := auth.ResolvePlasticity(&auth.PlasticityConfig{}); r.WriteTwinThresholdOverride != nil || r.WriteTwinInline {
		t.Fatalf("unset fields must resolve to no override and inline off: %+v", r)
	}
}

// TestWriteTwin_FieldsNotPresetVarying pins that the write-twin fields are
// identical across every preset for the same config — they are per-vault
// operator choices, not cognition presets (principle #6).
func TestWriteTwin_FieldsNotPresetVarying(t *testing.T) {
	v, on := 0.95, true
	type pin struct {
		T      *float64
		Inline bool
	}
	var want *pin
	for _, preset := range []string{"default", "reference", "scratchpad", "knowledge-graph", "working"} {
		r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: preset, WriteTwinThreshold: &v, WriteTwinInline: &on})
		got := &pin{r.WriteTwinThresholdOverride, r.WriteTwinInline}
		if want == nil {
			want = got
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("preset %q resolves write-twin fields to %+v, default gave %+v", preset, got, want)
		}
	}
	for _, preset := range []string{"default", "reference", "scratchpad", "knowledge-graph", "working"} {
		if r := auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: preset}); r.WriteTwinInline || r.WriteTwinThresholdOverride != nil {
			t.Fatalf("preset %q turned the write-twin block on by itself", preset)
		}
	}
}

func TestWriteTwin_ParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "0": Off, "shadow": Shadow, " SHADOW ": Shadow, "1": Inline} {
		m, ok := ParseMode(in)
		if !ok || m != want {
			t.Fatalf("ParseMode(%q) = %v,%v want %v", in, m, ok, want)
		}
	}
	if m, ok := ParseMode("2"); ok || m != Off {
		t.Fatalf("unrecognised value must be reported and run off, got %v,%v", m, ok)
	}
}

func TestWriteTwin_DecideClauses(t *testing.T) {
	cases := []struct {
		name             string
		flags, writes    int
		guard            string
		judged, positive int
		want             string
	}{
		{"rate veto", 60, 1000, GuardOK, 0, 0, DecisionVeto},
		{"homogeneous veto", 50, 2000, GuardTooHomogeneous, 0, 0, DecisionVeto},
		{"precision veto", 50, 2000, GuardOK, 50, 32, DecisionVeto},
		{"too few flags", 39, 2000, GuardOK, 39, 39, DecisionDoNotEnable},
		{"cold start", 45, 2000, GuardColdStart, 0, 0, DecisionDoNotEnable},
		{"awaiting judging", 45, 2000, GuardOK, 0, 0, DecisionPendingJudging},
		{"enable", 100, 4000, GuardOK, 100, 87, DecisionEnable},
	}
	for _, c := range cases {
		if got := Decide(c.flags, c.writes, c.guard, c.judged, c.positive); got.Decision != c.want {
			t.Fatalf("%s: got %+v, want %s", c.name, got, c.want)
		}
	}
	// Exactly 5 per 100 is within budget.
	if got := Decide(50, 1000, GuardOK, 0, 0); got.Decision == DecisionVeto {
		t.Fatalf("5.0 per 100 is the budget, not over it: %+v", got)
	}
}

func TestWriteTwin_WilsonMatchesGate0(t *testing.T) {
	// Gate 0 reported 29/35 → [0.67, 0.92]; the design's power note says
	// 87/100 → ≈0.79.
	if lb := WilsonLower(29, 35); math.Abs(lb-0.674) > 0.002 {
		t.Fatalf("Wilson lower 29/35 = %.4f, want ≈0.674", lb)
	}
	if lb := WilsonLower(87, 100); math.Abs(lb-0.790) > 0.003 {
		t.Fatalf("Wilson lower 87/100 = %.4f, want ≈0.790", lb)
	}
}

func TestWriteTwin_ClampConstantsPinnedToAuth(t *testing.T) {
	lo, hi := 0.0, 9.0
	if r := auth.ResolvePlasticity(&auth.PlasticityConfig{WriteTwinThreshold: &lo}); *r.WriteTwinThresholdOverride != OverrideMin {
		t.Fatalf("auth clamp floor %v != writetwin.OverrideMin %v", *r.WriteTwinThresholdOverride, OverrideMin)
	}
	if r := auth.ResolvePlasticity(&auth.PlasticityConfig{WriteTwinThreshold: &hi}); *r.WriteTwinThresholdOverride != OverrideMax {
		t.Fatalf("auth clamp ceiling %v != writetwin.OverrideMax %v", *r.WriteTwinThresholdOverride, OverrideMax)
	}
}

func unitVec(d int, hot int) []float32 {
	v := make([]float32, d)
	v[hot] = 1
	return v
}

func TestRing_ScanRules(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := NewRing(func() time.Time { return now })
	ws := [8]byte{1}
	other := [8]byte{2}
	mk := func(b byte, created time.Time, vec []float32, batch uint64) Entry {
		return Entry{ID: [16]byte{b}, Vec: vec, CreatedAt: created, BatchID: batch}
	}
	probe := mk(9, now, unitVec(4, 0), 0)
	r.Insert(ws, mk(1, now.Add(-10*time.Minute), unitVec(4, 0), 0))   // twin, in window
	r.Insert(ws, mk(2, now.Add(-2*time.Hour), unitVec(4, 0), 0))      // twin, too old
	r.Insert(ws, mk(3, now.Add(-5*time.Minute), unitVec(4, 1), 0))    // orthogonal
	r.Insert(ws, mk(4, now.Add(time.Minute), unitVec(4, 0), 0))       // written after the probe
	r.Insert(ws, mk(5, now.Add(-time.Minute), []float32{1, 0, 0}, 0)) // other dimension
	r.Insert(other, mk(6, now.Add(-time.Minute), unitVec(4, 0), 0))   // other vault
	r.Insert(ws, probe)
	m, considered := r.Scan(ws, probe, 0.93, SelectRecent)
	if len(m) != 1 || m[0].ID != [16]byte{1} {
		t.Fatalf("want only entry 1, got %+v", m)
	}
	if considered != 2 { // entries 1 and 3 are earlier, in window, comparable
		t.Fatalf("considered = %d, want 2", considered)
	}
	// Batch selection.
	b1 := mk(20, now.Add(-time.Second), unitVec(4, 2), 7)
	b2 := mk(21, now, unitVec(4, 2), 7)
	r.Insert(ws, b1)
	r.Insert(ws, b2)
	if m, _ := r.Scan(ws, b2, 0.93, SelectRecent); len(m) != 0 {
		t.Fatalf("a batch-mate must not count as a recent twin: %+v", m)
	}
	if m, _ := r.Scan(ws, b2, 0.93, SelectBatch); len(m) != 1 || m[0].ID != b1.ID {
		t.Fatalf("intra-batch twin missing: %+v", m)
	}
	r.ClearVault(ws)
	if r.Len(ws) != 0 || r.Len(other) != 1 {
		t.Fatalf("clear must drop only its own vault: ws=%d other=%d", r.Len(ws), r.Len(other))
	}
}

func TestRing_CapAndEviction(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	r := NewRing(func() time.Time { return clock })
	ws := [8]byte{3}
	for i := 0; i < RingCap+40; i++ {
		var id [16]byte
		id[0], id[1] = byte(i>>8), byte(i)
		r.Insert(ws, Entry{ID: id, Vec: unitVec(4, 0), CreatedAt: now})
	}
	if r.Len(ws) != RingCap {
		t.Fatalf("ring holds %d, cap %d", r.Len(ws), RingCap)
	}
	r.Insert(ws, Entry{ID: [16]byte{0xEE}, Vec: unitVec(4, 0), CreatedAt: now}) // re-insert dedupes by id
	r.Insert(ws, Entry{ID: [16]byte{0xEE}, Vec: unitVec(4, 1), CreatedAt: now})
	if e, _ := r.Get(ws, [16]byte{0xEE}); e.Vec[1] != 1 {
		t.Fatal("re-insert must replace the entry's vector")
	}
	clock = now.Add(Window + time.Second)
	r.Insert(ws, Entry{ID: [16]byte{0xEF}, Vec: unitVec(4, 0), CreatedAt: clock})
	if r.Len(ws) != 1 {
		t.Fatalf("entries seen over an hour ago must be evicted, %d remain", r.Len(ws))
	}
}

func TestRing_IdleVaultEmptiesAfterTheWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	r := NewRing(func() time.Time { return clock })
	idle, busy := [8]byte{7}, [8]byte{8}
	for i := 0; i < 50; i++ {
		r.Insert(idle, Entry{ID: [16]byte{byte(i), 1}, Vec: unitVec(384, 0), CreatedAt: now})
	}
	// 48 hours later another vault writes once: the idle vault's vectors are
	// past the window and must be gone — the vault slot too.
	clock = now.Add(48 * time.Hour)
	r.Insert(busy, Entry{ID: [16]byte{1, 2}, Vec: unitVec(384, 0), CreatedAt: clock})
	if r.Len(idle) != 0 || r.Vaults() != 1 {
		t.Fatalf("idle vault still holds %d entries (%d vaults) after 48h", r.Len(idle), r.Vaults())
	}
	// And with no write anywhere, the periodic Sweep empties everything.
	clock = clock.Add(2 * Window)
	r.Sweep()
	if r.Vaults() != 0 || r.Bytes() != 0 {
		t.Fatalf("after a sweep past the window: %d vaults, %d bytes", r.Vaults(), r.Bytes())
	}
}

func TestRing_TotalCapAcrossVaults(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	r := NewRing(func() time.Time { return clock })
	evicted := 0
	r.OnEvict = func(reason string, n int) {
		if reason == "total_cap" {
			evicted += n
		}
	}
	vec := unitVec(384, 0)
	for v := 0; v < 300; v++ {
		for i := 0; i < RingCap; i++ {
			clock = clock.Add(time.Microsecond)
			var id [16]byte
			id[0], id[1], id[2] = byte(v>>8), byte(v), byte(i>>8)
			id[3] = byte(i)
			r.Insert([8]byte{byte(v >> 8), byte(v), 1}, Entry{ID: id, Vec: vec, CreatedAt: clock})
		}
	}
	if b := r.Bytes(); b > RingTotalBytes {
		t.Fatalf("300 vaults x 512 entries hold %d bytes, over the %d cap", b, RingTotalBytes)
	}
	if evicted == 0 {
		t.Fatal("no total_cap evictions were reported")
	}
	// The newest writes survive; the globally oldest went first.
	v299 := 299
	last := [8]byte{byte(v299 >> 8), byte(v299), 1}
	if r.Len(last) != RingCap {
		t.Fatalf("the most recent vault lost entries to the global cap: %d", r.Len(last))
	}
}
