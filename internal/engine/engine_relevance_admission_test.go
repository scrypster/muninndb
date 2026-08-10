package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// calibratedNoEmbedEnv is testEnv with a REGISTERED embed model name wired, so
// resolveSemanticBaseline finds bge-small's b=0.520 and the relevance-band
// phase is CALIBRATED (no no_model_baseline response basis). No embedder assets
// are needed: the baseline comes from the registry constant, and with hnsw=nil
// activation never embeds the query, so semanticDegraded stays false too. That
// makes this the lightest engine wiring on which filter_match is reachable.
func calibratedNoEmbedEnv(t *testing.T) (*Engine, *storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-band-admission-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem,
		Embedder: embedder, EmbedModelName: "bge-small-en-v1.5",
	})
	return eng, store, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestRecall_TagOnlyHit_BandsFilterMatch_RegardlessOfConfidenceAndRecency is
// the end-to-end pin for G6 refute findings 1 and 2: a `due:<=today` reminder
// whose content is unrelated to the query text must band `filter_match` /
// `tag_filter_bypass` on the wire — for the fresh confidence-1.0 row, the
// 0.9-confidence twin AND the stale twin alike. Same fixture shape as
// TestRawTagRange_SeedsDueItems_DefaultScoring (content-unrelated, due-tagged,
// seeded only by the S1 raw-tag-range index), driven through the full engine
// recall path so it exercises the actual production band phase.
//
// RED at dda8a3e: the fresh confidence-1.0 row bands `weak` — its floored
// absolute score lands exactly ON the COG-6 gate (tagMatchFloor == 0.1 == the
// ACT-R default threshold) and admissionOf classified on `gated < threshold`
// instead of on the REASON the row was admitted.
func TestRecall_TagOnlyHit_BandsFilterMatch_RegardlessOfConfidenceAndRecency(t *testing.T) {
	eng, store, cleanup := calibratedNoEmbedEnv(t)
	defer cleanup()

	ctx := context.Background()
	ws := store.ResolveVaultPrefix("default")
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)

	write := func(concept string, conf float32, ts time.Time) string {
		t.Helper()
		id, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    concept,
			Content:    fmt.Sprintf("zqxw glorbnak fluvventious prendicular skoombat %s", concept),
			Tags:       []string{"due:2026-01-01"},
			Confidence: conf,
			Stability:  30.0,
			CreatedAt:  ts,
			LastAccess: ts,
			Relevance:  0,
		})
		if err != nil {
			t.Fatalf("WriteEngram(%s): %v", concept, err)
		}
		return id.String()
	}

	rows := map[string]string{
		"fresh, confidence 1.0 (absolute lands ON the 0.1 gate)": write("fresh-conf-one", 1.0, now),
		"fresh, confidence 0.9 (absolute 0.09, below the gate)":  write("fresh-conf-ninety", 0.9, now),
		"stale 30d, confidence 1.0 (decayed prior)":              write("stale-conf-one", 1.0, stale),
	}

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "default",
		Context:    []string{"totally different query about nothing related"},
		MaxResults: 50,
		Filters: []mbp.Filter{
			{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-07-27"}},
		},
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	for label, id := range rows {
		item := itemByID(resp, id)
		if item == nil {
			t.Errorf("%s: not returned at all (the S1 bypass regressed)", label)
			continue
		}
		if item.RelevanceBand != activation.RelevanceFilterMatch {
			t.Errorf("%s: relevance_band = %q (basis %q, abs %.6f), want %q — the filter defined "+
				"the set; whether the row's floored arithmetic lands on, above or below the gate "+
				"must not change what recall says the filter did",
				label, item.RelevanceBand, item.RelevanceBandBasis,
				item.ScoreComponents.AbsoluteScore, activation.RelevanceFilterMatch)
			continue
		}
		if item.RelevanceBandBasis != activation.BasisTagFilterBypass {
			t.Errorf("%s: relevance_band_basis = %q, want %q",
				label, item.RelevanceBandBasis, activation.BasisTagFilterBypass)
		}
	}
}
