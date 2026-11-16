//go:build localassets

package mcp

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/writetwin"
)

// TestWriteTwin_WritePathLatency is a MEASUREMENT, not a gate: it reports the
// muninn_remember handler latency (p50/p99) in each mode on a 2,000-memory
// vault, with the real bundled embedder behind a real retroactive processor.
// Skipped unless MUNINN_TWIN_LATENCY=1 (it embeds a few hundred texts).
//
//	MUNINN_TWIN_LATENCY=1 go test -tags localassets ./internal/mcp/ -run TestWriteTwin_WritePathLatency -v
//
// Invented fixture domain: trail maintenance.
func TestWriteTwin_WritePathLatency(t *testing.T) {
	if os.Getenv("MUNINN_TWIN_LATENCY") != "1" {
		t.Skip("measurement harness; set MUNINN_TWIN_LATENCY=1")
	}
	modes := []writetwin.Mode{writetwin.Off, writetwin.Shadow, writetwin.Inline}
	if only := os.Getenv("MUNINN_TWIN_LATENCY_MODE"); only != "" {
		m, ok := writetwin.ParseMode(only)
		require.True(t, ok)
		modes = []writetwin.Mode{m}
	}
	for _, mode := range modes {
		p50, p99, related, reasons := measureRememberLatency(t, mode, 2000, 120)
		t.Logf("mode=%-6s remember handler p50=%-8v p99=%-8v blocks with candidates=%d unavailable=%v", mode, p50, p99, related, reasons)
	}
}

func measureRememberLatency(t *testing.T, mode writetwin.Mode, fixture, samples int) (time.Duration, time.Duration, int, map[string]int) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	require.NoError(t, err)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 4096})
	ftsIdx := fts.New(db)
	reg := hnsw.NewRegistry(db)
	noop := activation.NewNoopEmbedder()
	act := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, noop)
	trig := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, noop)
	as := auth.NewStore(db)
	on := true
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: twinVault, Plasticity: &auth.PlasticityConfig{Version: 1, WriteTwinInline: &on}}))
	eng := engine.NewEngine(engine.EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: act,
		TriggerSystem: trig, Embedder: noop, HNSWRegistry: reg, EmbedModelName: "bge-small-en-v1.5", WriteTwinMode: mode})
	defer func() { eng.Stop(); store.Close() }()

	svc, err := embedpkg.NewEmbedService("local://bge-small-en-v1.5")
	require.NoError(t, err)
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: t.TempDir()}); err != nil {
		t.Skipf("local embedder unavailable: %v", err)
	}
	defer svc.Close()

	// The 2,000-memory fixture: random 384-d client vectors (stored + indexed
	// like any vector; the guard and HNSW see a realistic population size).
	rng := rand.New(rand.NewSource(int64(mode) + 1))
	reqs := make([]*mbp.WriteRequest, 0, 50)
	flush := func() {
		if len(reqs) == 0 {
			return
		}
		_, errs := eng.WriteBatch(ctx, reqs)
		for _, e := range errs {
			require.NoError(t, e)
		}
		reqs = reqs[:0]
	}
	for i := 0; i < fixture; i++ {
		v := make([]float32, 384)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		reqs = append(reqs, &mbp.WriteRequest{Vault: twinVault, Concept: "trail log",
			Content: fmt.Sprintf("Trail log %d: crew cleared blowdowns past switchback %d and checked drain %d.", i, i%40, i%17), Embedding: v})
		if len(reqs) == 50 {
			flush()
		}
	}
	flush()

	rp := plugin.NewRetroactiveProcessor(plugin.NewStoreAdapter(store, reg), svc, plugin.DigestEmbed)
	rp.SetOnEmbedded(eng.WriteTwinOnEmbedded)
	eng.SetWriteTwinFeed(rp)
	eng.SetRetroactiveProcessors(rp)
	eng.SetOnWrite(rp.Notify)
	pctx, cancel := context.WithCancel(ctx)
	rp.Start(pctx)
	defer func() { cancel(); rp.Stop() }()

	srv := New(":0", NewEngineAdapter(eng, nil, nil), "", nil, nil, nil)
	// Prime the guard (mode 1 reports calibrating until it is measured).
	if mode == writetwin.Inline {
		_ = eng.WriteTwinInline(ctx, twinVault, storage.NewULID().String(), nil)
		eng.WaitWriteTimeIdle()
	}

	var lat []time.Duration
	shown := 0
	reasons := map[string]int{}
	for i := 0; i < samples; i++ {
		content := fmt.Sprintf("Water bar %d on the ridge spur is set at thirty degrees and needs rock armouring.", i/2)
		if i%2 == 1 {
			content = fmt.Sprintf("The ridge spur water bar %d sits at thirty degrees; rock armouring is still needed.", i/2)
		}
		start := time.Now()
		w := postRPC(t, srv, string(mkToolCallBody("muninn_remember", map[string]any{"vault": twinVault, "content": content, "concept": "water bars"})))
		lat = append(lat, time.Since(start))
		out := extractInnerJSON(t, decodeResp(t, w.Body.String()))
		if rel, ok := out["related"].(map[string]any); ok {
			if _, cands := rel["candidates"]; cands {
				shown++
			} else if r, ok := rel["reason"].(string); ok {
				reasons[r]++
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return lat[len(lat)/2], lat[len(lat)*99/100], shown, reasons
}
