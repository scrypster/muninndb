package engine

import (
	"context"
	"encoding/json"
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

// TestContentFirst_LatencyMeasure is an opt-in measurement (CF_LATENCY=1), not
// a gate: recall p50/p90 on a synthetic trail-maintenance vault with entity
// links and a primed activation log. Run it twice on the same commit for an
// A/B — normally (content-first) and as
//
//	MUNINN_TRIAL_LEGACY_BOOST_ORDER=1 go test -tags cognitiontrial,localassets ...
//
// for the boosted order. CF_LATENCY_N sets the vault size (default 2000; at
// <=1000 the per-index candidate budget is the whole vault, the widest pool).
func TestContentFirst_LatencyMeasure(t *testing.T) {
	if os.Getenv("CF_LATENCY") != "1" {
		t.Skip("measurement; set CF_LATENCY=1")
	}
	n := 2000
	if v := os.Getenv("CF_LATENCY_N"); v != "" {
		fmt.Sscanf(v, "%d", &n)
	}
	words := strings.Fields("culvert outlet riprap drainage waterbar switchback tread gravel bridge decking " +
		"blowdown crosscut brushing corridor cairn blaze trailhead ridge saddle spur creek ford boardwalk " +
		"puncheon turnpike checkdam grade reroute sidehill backslope outslope crown rut erosion mud stone")
	places := make([]string, 50)
	for i := range places {
		places[i] = fmt.Sprintf("Place%02d Hollow", i)
	}
	r := rand.New(rand.NewSource(7))
	c := newCFEnv(t)
	const vault = "trail-latency"
	ids := make([]storage.ULID, 0, n)
	for i := 0; i < n; i++ {
		k := 6 + r.Intn(5)
		ws := make([]string, k)
		for j := range ws {
			ws[j] = words[r.Intn(len(words))]
		}
		var ents []string
		for e := r.Intn(3); e > 0; e-- {
			ents = append(ents, places[r.Intn(len(places))])
		}
		ids = append(ids, c.write(vault, strings.Join(ws[:3], " "), strings.Join(ws, " "), time.Duration(n-i)*time.Minute, ents...))
	}
	for i := 0; i < 200; i++ {
		c.link(vault, ids[r.Intn(n)], ids[r.Intn(n)], 0.5+0.5*r.Float32())
	}
	queries := make([]string, 200)
	for i := range queries {
		queries[i] = words[r.Intn(len(words))] + " " + words[r.Intn(len(words))] + " " + words[r.Intn(len(words))]
	}
	c.primeLog(vault, ids[:20]...)
	for _, q := range queries[:20] { // warm-up
		c.recall(vault, q, 10, true)
	}
	// Bytes the COG-35 associated block adds per recall (JSON), every query
	// counted, zeros included.
	var sizes []int
	withBlock := 0
	for _, q := range queries {
		resp, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: vault, Context: []string{q}, MaxResults: 10, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if resp.Associated != nil {
			b, _ := json.Marshal(map[string]any{"associated": resp.Associated})
			n = len(b) - 2 // the key and value as they sit inside the response object
			withBlock++
		}
		sizes = append(sizes, n)
	}
	sort.Ints(sizes)
	t.Logf("associated block: %d of %d limit-10 recalls carry it; bytes median=%d p99=%d max=%d",
		withBlock, len(queries), sizes[len(sizes)/2], sizes[len(sizes)*99/100], sizes[len(sizes)-1])
	if os.Getenv("CF_EXPLAIN") == "1" {
		var rec, exp []time.Duration
		for i, q := range queries[:60] {
			target := ids[(i*37)%len(ids)]
			t0 := time.Now()
			if _, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: vault, Context: []string{q}, MaxResults: 10, ReadOnly: true}); err != nil {
				t.Fatal(err)
			}
			rec = append(rec, time.Since(t0))
			t1 := time.Now()
			if _, err := c.eng.Explain(context.Background(), vault, target.String(), []string{q}, nil); err != nil {
				t.Fatal(err)
			}
			exp = append(exp, time.Since(t1))
		}
		sort.Slice(rec, func(i, j int) bool { return rec[i] < rec[j] })
		sort.Slice(exp, func(i, j int) bool { return exp[i] < exp[j] })
		t.Logf("explain: recall(limit 10) p50=%v  explain p50=%v  ratio=%.2f", rec[len(rec)/2], exp[len(exp)/2], float64(exp[len(exp)/2])/float64(rec[len(rec)/2]))
	}
	for _, limit := range []int{10, 100} {
		var lat []time.Duration
		for rep := 0; rep < 3; rep++ {
			for _, q := range queries {
				start := time.Now()
				if _, err := c.eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: vault, Context: []string{q}, MaxResults: limit, ReadOnly: true}); err != nil {
					t.Fatal(err)
				}
				lat = append(lat, time.Since(start))
			}
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.Logf("n=%d limit=%d recalls=%d p50=%v p90=%v", n, limit, len(lat), lat[len(lat)/2], lat[len(lat)*9/10])
	}
}
