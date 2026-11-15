package engine

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestExplain_DiagnosticActivationHydratesOnlyTheTarget — explain scores every
// candidate (threshold -1, 1,000 rows) to find ONE memory's card. Everything
// downstream that scales with that list and cannot change the card — COG-25's
// pairwise pass, items/provenance/bands/brief for every other row, the
// associated block — must not run for it. RED before the diagnostic path: the
// activation returned every scored row (and a brief over all of them), and on
// a 2,000-memory vault explain cost ~800 ms against a ~3 ms recall.
func TestExplain_DiagnosticActivationHydratesOnlyTheTarget(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-explain-cost"
	f := cfBuild(c, vault)
	target := f.middle[3]
	resp, err := c.eng.Activate(withExplainTarget(context.Background(), target), &mbp.ActivateRequest{
		Vault: vault, Context: []string{cfQuery}, MaxResults: explainMaxCandidates, Threshold: -1, ExcludeAssociated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Activations) != 1 || resp.Activations[0].ID != target.String() {
		t.Errorf("explain's diagnostic activation hydrated %d rows, want only the target", len(resp.Activations))
	}
	if len(resp.Brief) != 0 || resp.Associated != nil {
		t.Errorf("explain's diagnostic activation built a brief (%d) or an associated block (%d)", len(resp.Brief), len(resp.Associated))
	}
	// The card itself is unchanged: it equals the recall row.
	ex, err := c.eng.Explain(context.Background(), vault, target.String(), []string{cfQuery}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := c.recall(vault, cfQuery, 40, true)
	i := cfIndex(rows, target)
	if i < 0 || float32(ex.FinalScore) != rows[i].score || ex.Components.AbsoluteScore != rows[i].abs {
		t.Errorf("explain card disagrees with the recall row: explain %.4f/%.4f, recall %v", ex.FinalScore, ex.Components.AbsoluteScore, rows)
	}
}

// TestExplain_CostRatioMeasure — opt-in (CF_EXPLAIN_RATIO=1; sizes from
// CF_EXPLAIN_SIZES, default "2000,12000"): explain p50 must stay within 1.5x
// of a limit-10 recall p50 of the same queries on the same vault. A relative
// bound against the measured recall, never a wall-clock constant.
func TestExplain_CostRatioMeasure(t *testing.T) {
	if os.Getenv("CF_EXPLAIN_RATIO") != "1" {
		t.Skip("measurement; set CF_EXPLAIN_RATIO=1")
	}
	sizes := []int{2000, 12000}
	if v := os.Getenv("CF_EXPLAIN_SIZES"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			fmt.Sscanf(f, "%d", &n)
			sizes = append(sizes, n)
		}
	}
	for _, n := range sizes {
		recall, explain := explainRatioAt(t, n)
		ratio := float64(explain) / float64(recall)
		t.Logf("n=%d recall(limit 10) p50=%v explain p50=%v ratio=%.2f", n, recall, explain, ratio)
		if ratio > 1.5 {
			t.Errorf("n=%d: explain p50 is %.2fx a limit-10 recall (bound 1.5x)", n, ratio)
		}
	}
}

func explainRatioAt(t *testing.T, n int) (recallP50, explainP50 time.Duration) {
	t.Helper()
	words := strings.Fields("culvert outlet riprap drainage waterbar switchback tread gravel bridge decking " +
		"blowdown crosscut brushing corridor cairn blaze trailhead ridge saddle spur creek ford boardwalk " +
		"puncheon turnpike checkdam grade reroute sidehill backslope outslope crown rut erosion mud stone")
	places := make([]string, 50)
	for i := range places {
		places[i] = fmt.Sprintf("Place%02d Hollow", i)
	}
	r := rand.New(rand.NewSource(int64(n)))
	c := newCFEnv(t)
	vault := fmt.Sprintf("trail-explain-%d", n)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	_ = c.store.GetVaultCount(ctx, ws)
	items := make([]storage.EngramBatchItem, n)
	for i := range items {
		k := 6 + r.Intn(5)
		w := make([]string, k)
		for j := range w {
			w[j] = words[r.Intn(len(words))]
		}
		created := time.Now().Add(-time.Duration(n-i) * time.Minute)
		items[i] = storage.EngramBatchItem{WSPrefix: ws, Engram: &storage.Engram{Concept: strings.Join(w[:3], " "),
			Content: strings.Join(w, " "), Confidence: 1, CreatedAt: created, UpdatedAt: created, LastAccess: created}}
	}
	ids, errs := c.store.WriteEngramBatch(ctx, items)
	for i := range items {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		e := items[i].Engram
		if err := c.fts.IndexEngram(ws, [16]byte(ids[i]), e.Concept, "", e.Content, nil); err != nil {
			t.Fatal(err)
		}
		for e := r.Intn(3); e > 0; e-- {
			name := places[r.Intn(len(places))]
			_ = c.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: name, Type: "place", Source: "inline"}, "inline")
			_ = c.store.WriteEntityEngramLink(ctx, ws, ids[i], name)
		}
	}
	_ = c.store.WriteVaultName(ws, vault)
	var rec, exp []time.Duration
	for i := 0; i < 60; i++ {
		q := words[r.Intn(len(words))] + " " + words[r.Intn(len(words))] + " " + words[r.Intn(len(words))]
		target := ids[r.Intn(len(ids))]
		t0 := time.Now()
		if _, err := c.eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{q}, MaxResults: 10, ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
		rec = append(rec, time.Since(t0))
		t1 := time.Now()
		if _, err := c.eng.Explain(ctx, vault, target.String(), []string{q}, nil); err != nil {
			t.Fatal(err)
		}
		exp = append(exp, time.Since(t1))
	}
	sort.Slice(rec, func(i, j int) bool { return rec[i] < rec[j] })
	sort.Slice(exp, func(i, j int) bool { return exp[i] < exp[j] })
	return rec[len(rec)/2], exp[len(exp)/2]
}
