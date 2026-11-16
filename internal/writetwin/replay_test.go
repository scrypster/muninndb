package writetwin

import (
	"context"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// Invented fixture domain: beekeeping hive notes.

const rDim = 24

func rUnit(seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	v := make([]float32, rDim)
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

func rAt(v []float32, c float64, seed int64) []float32 {
	u := rUnit(seed)
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

type replayFixture struct {
	t     *testing.T
	store *storage.PebbleStore
	ws    [8]byte
	now   time.Time
}

func newReplayFixture(t *testing.T) *replayFixture {
	t.Helper()
	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() { store.Close() })
	ws := store.ResolveVaultPrefix("apiary")
	if err := store.WriteVaultName(ws, "apiary"); err != nil {
		t.Fatal(err)
	}
	return &replayFixture{t: t, store: store, ws: ws, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
}

func (f *replayFixture) put(concept, content string, at time.Time, vec []float32, assocs ...storage.Association) storage.ULID {
	f.t.Helper()
	id, err := f.store.WriteEngram(context.Background(), f.ws, &storage.Engram{
		Concept: concept, Content: content, CreatedAt: at, UpdatedAt: at, LastAccess: at,
		Confidence: 1, Stability: 30, State: storage.StateActive, Embedding: vec, Associations: assocs,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestTwinReplay_ExactRuleAndAsOfHeads(t *testing.T) {
	f := newReplayFixture(t)
	day := func(d int) time.Time { return f.now.Add(-time.Duration(d) * 24 * time.Hour) }

	// Background: 30 unrelated hive notes spread over the window.
	for i := 0; i < 30; i++ {
		f.put("hive", "hive inspection note "+string(rune('a'+i%26)), day(50-i).Add(time.Duration(i)*time.Minute), rUnit(int64(1000+i)))
	}
	// (1) A recent twin: 20 minutes apart, cos 0.96 → flagged, rule=recent.
	b1 := rUnit(1)
	r1a := f.put("queen", "hive 4 queen was marked blue in spring", day(40), b1)
	r1b := f.put("queen", "hive 4 queen is marked blue", day(40).Add(20*time.Minute), rAt(b1, 0.96, 11))
	// (2) Too far apart (2h) → not flagged.
	b2 := rUnit(2)
	f.put("varroa", "mite count in hive 2 was nine per wash", day(30), b2)
	f.put("varroa", "hive 2 mite wash: nine mites", day(30).Add(2*time.Hour), rAt(b2, 0.97, 12))
	// (3) Declared at write time (evolve): successor carries the supersedes
	// edge made with it → skipped, and not counted as an ordinary write.
	b3 := rUnit(3)
	e3 := f.put("feeding", "hive 7 gets one-to-one syrup", day(20), b3)
	at3 := day(20).Add(10 * time.Minute)
	f.put("feeding", "hive 7 now gets two-to-one syrup", at3, rAt(b3, 0.95, 13),
		storage.Association{TargetID: e3, RelType: storage.RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: at3})
	// (4) Resolved LATER by a link a day after the write: as of the write it
	// was an undeclared twin → still flagged.
	b4 := rUnit(4)
	e4 := f.put("supers", "hive 1 has two honey supers", day(10), b4)
	w4 := f.put("supers", "hive 1 has two supers on", day(10).Add(30*time.Minute), rAt(b4, 0.97, 14))
	if err := f.store.WriteAssociation(context.Background(), f.ws, w4, e4, &storage.Association{TargetID: e4,
		RelType: storage.RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: day(9)}); err != nil {
		t.Fatal(err)
	}
	// (5) Batch-approximated: one second apart → flagged, rule=batch.
	b5 := rUnit(5)
	f.put("smoker", "smoker fuel is pine needles", day(5), b5)
	f.put("smoker", "use pine needles as smoker fuel", day(5).Add(time.Second), rAt(b5, 0.98, 15))
	// (6) Below threshold → not flagged.
	b6 := rUnit(6)
	f.put("swarm", "hive 3 swarmed in May", day(3), b6)
	f.put("swarm", "a swarm left hive 3", day(3).Add(5*time.Minute), rAt(b6, 0.90, 16))
	// (7) Outside the 60-day window → not replayed.
	b7 := rUnit(7)
	f.put("wax", "wax moth found in stored frames", day(70), b7)
	f.put("wax", "stored frames have wax moth", day(70).Add(10*time.Minute), rAt(b7, 0.99, 17))

	rep, pairs, corpus, err := Replay(context.Background(), f.store, f.ws, ReplayOptions{
		Now: f.now, Threshold: Threshold{Value: 0.93, Source: "model_hint:bge-small-en-v1.5", OK: true}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.FlaggedWrites != 3 || rep.FlaggedRecent != 2 || rep.FlaggedBatch != 1 {
		t.Fatalf("want 3 flagged writes (recent 2: twin + later-resolved; batch 1), got %+v\npairs %+v", rep, pairs)
	}
	if rep.Successors != 1 {
		t.Fatalf("the evolve successor must be excluded from ordinary writes: successors=%d", rep.Successors)
	}
	foundR1, foundW4 := false, false
	for _, p := range pairs {
		if p.NewID == r1b && p.HeadID == r1a && p.Rule == "recent" {
			foundR1 = true
		}
		if p.NewID == w4 && p.HeadID == e4 {
			foundW4 = true
		}
		if p.HeadID == e3 {
			t.Fatalf("a pair declared by evolve at write time must be skipped: %+v", p)
		}
	}
	if !foundR1 || !foundW4 {
		t.Fatalf("missing expected pairs (recent twin %v, later-resolved %v): %+v", foundR1, foundW4, pairs)
	}
	wantWrites := rep.WritesTotal - rep.Unvectored - rep.Successors
	if rep.Writes != wantWrites || math.Abs(rep.FlagsPer100-100*3/float64(rep.Writes)) > 1e-9 {
		t.Fatalf("rate arithmetic: %+v", rep)
	}
	// 3 flags in ~48 writes is over the nag budget (VETO), and every weaker
	// clause that fires is reported too.
	if rep.Guard != GuardColdStart || rep.Verdict.Decision != DecisionVeto || len(rep.Verdict.Reasons) != 3 {
		t.Fatalf("want VETO (rate) plus too-few-flags and cold-start reasons: guard %q verdict %+v", rep.Guard, rep.Verdict)
	}

	judge, keys := JudgingSet(corpus, pairs, ReplayOptions{Seed: 7})
	if len(judge) != len(keys) || len(judge) < 3 {
		t.Fatalf("judging set: %d rows / %d keys", len(judge), len(keys))
	}
	flags := 0
	for i, k := range keys {
		if k.Item != judge[i].Item {
			t.Fatalf("key/judge rows misaligned at %d", i)
		}
		if k.Kind == "flag" {
			flags++
		}
		if judge[i].Earlier.Content == "" || judge[i].Later.Content == "" || judge[i].Ask == "" {
			t.Fatalf("judging rows carry full text and the question: %+v", judge[i])
		}
	}
	if flags != 3 {
		t.Fatalf("every flagged write is sampled when under JudgeMax: %d", flags)
	}
	// Properly blind: no later text appears twice, and no control is drawn
	// from a flagged write (that pairing gave the arm away).
	later := map[string]int{}
	flagged := map[string]bool{}
	for _, k := range keys {
		if k.Kind == "flag" {
			flagged[k.NewID] = true
		}
	}
	controls := 0
	for i, k := range keys {
		later[judge[i].Later.Concept+"|"+judge[i].Later.Content]++
		if k.Kind == "control" {
			controls++
			if flagged[k.NewID] {
				t.Fatalf("control row %d reuses flagged write %s", k.Item, k.NewID)
			}
		}
		if k.Seed != 7 {
			t.Fatalf("key row %d does not record the seed: %+v", k.Item, k)
		}
	}
	for text, n := range later {
		if n > 1 {
			t.Fatalf("later text appears %d times (the arm is identifiable): %q", n, text)
		}
	}
	if controls != flags {
		t.Fatalf("want as many controls as flags here (%d), got %d", flags, controls)
	}
}

func TestTwinAudit_UnresolvedPerHundredWrites(t *testing.T) {
	f := newReplayFixture(t)
	ctx := context.Background()
	at := f.now.Add(-10 * 24 * time.Hour)
	b := rUnit(9)
	a1 := f.put("queen", "hive 5 queen is unmarked", at, b)
	n1 := f.put("queen", "hive 5 has an unmarked queen", at.Add(10*time.Minute), rAt(b, 0.96, 1))
	a2 := f.put("frames", "hive 6 has eight frames of brood", at.Add(time.Hour), rUnit(10))
	n2 := f.put("frames", "hive 6: eight brood frames", at.Add(time.Hour+5*time.Minute), rUnit(11))
	a3 := f.put("stand", "hive 8 sits on a cinder block stand", at.Add(2*time.Hour), rUnit(12))
	n3 := f.put("stand", "hive 8 stand is cinder blocks", at.Add(2*time.Hour+5*time.Minute), rUnit(13))
	for i := 0; i < 14; i++ {
		f.put("note", "unrelated apiary note "+string(rune('a'+i)), at.Add(time.Duration(10+i)*time.Minute), rUnit(int64(100+i)))
	}
	// Pair 2 resolved by a supersedes link; pair 3 resolved by a forget.
	if err := f.store.WriteAssociation(ctx, f.ws, n2, a2, &storage.Association{TargetID: a2, RelType: storage.RelSupersedes, Weight: 1, Confidence: 1, CreatedAt: at.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SoftDelete(ctx, f.ws, n3); err != nil {
		t.Fatal(err)
	}
	wsHex := HexWS(f.ws)
	line := func(n, h storage.ULID, rule string) string {
		return `time=` + at.Add(11*time.Minute).Format(time.RFC3339Nano) + ` level=INFO msg="write twin flagged" vault_ws=` + wsHex +
			` new_id=` + n.String() + ` head_id=` + h.String() + ` similarity=0.96 age_s=600 rule=` + rule + ` shown=false`
	}
	log := strings.Join([]string{
		line(n1, a1, "recent"), line(n1, a1, "recent"), // duplicate line counts once
		line(n2, a2, "recent"), line(n3, a3, "recent"),
		line(n1, a2, "historic"), // only the shown rule counts
		`{"time":"` + at.Add(12*time.Minute).Format(time.RFC3339Nano) + `","level":"INFO","msg":"unrelated"}`,
	}, "\n")
	pairs, span, err := ParseFlagLog(strings.NewReader(log))
	if err != nil || len(pairs) != 5 {
		t.Fatalf("parse: %d pairs, err %v", len(pairs), err)
	}
	if !span.First.Equal(at.Add(11*time.Minute)) || !span.Last.Equal(at.Add(12*time.Minute)) {
		t.Fatalf("log span = %+v", span)
	}
	// The shadow period: the whole day of writes around the pairs.
	period := LogSpan{First: at.Add(-time.Minute), Last: at.Add(3 * time.Hour)}
	rep, err := Audit(ctx, f.store, f.ws, pairs, period, f.now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rep.PairsMature != 3 || rep.Unresolved != 1 {
		t.Fatalf("want 3 mature pairs, 1 unresolved (the link and the forget resolve theirs), got %+v", rep)
	}
	// 6 paired memories + 14 unrelated notes, all inside the period.
	if rep.Writes != 20 || math.Abs(rep.UnresolvedPer100-5) > 1e-9 {
		t.Fatalf("rate: want 1 unresolved per 20 writes = 5 per 100, got %+v", rep)
	}
	// A process start 5 minutes before the pairs makes the next hour blind:
	// those pairs and that hour's writes are reported apart, not in the rate.
	startLine := `time=` + at.Add(6*time.Minute).Format(time.RFC3339Nano) + ` level=INFO msg="write-time twin check enabled" mode=shadow`
	pairs2, span2, err := ParseFlagLog(strings.NewReader(startLine + "\n" + log))
	if err != nil || len(span2.Starts) != 1 {
		t.Fatalf("start line not read: %+v %v", span2, err)
	}
	span2.First, span2.Last = period.First, period.Last
	warm, err := Audit(ctx, f.store, f.ws, pairs2, span2, f.now, 7*24*time.Hour)
	if err != nil && err != ErrNoPairs {
		t.Fatal(err)
	}
	if warm.WarmupPairsExcluded != 3 || warm.PairsMature != 0 || warm.WarmupWritesExclude == 0 || warm.Writes+warm.WarmupWritesExclude != 20 {
		t.Fatalf("blind stretch not excluded and reported: %+v", warm)
	}
	// Too young pairs are not counted yet.
	young, _ := Audit(ctx, f.store, f.ws, pairs, period, at.Add(24*time.Hour), 7*24*time.Hour)
	if young.PairsMature != 0 {
		t.Fatalf("pairs flagged a day ago are not mature: %+v", young)
	}
}

func TestTwinAudit_ParsesJSONLogLines(t *testing.T) {
	l := `{"time":"2026-09-01T10:00:00Z","level":"INFO","msg":"write twin flagged","vault_ws":"00112233aabbccdd",` +
		`"new_id":"01JABCDEFGHJKMNPQRSTVWXYZ0","head_id":"01JABCDEFGHJKMNPQRSTVWXYZ1","rule":"recent","shown":true}`
	pairs, _, err := ParseFlagLog(strings.NewReader(l))
	if err != nil || len(pairs) != 1 || !pairs[0].Shown || pairs[0].VaultWS != "00112233aabbccdd" {
		t.Fatalf("json line: %+v err %v", pairs, err)
	}
}
