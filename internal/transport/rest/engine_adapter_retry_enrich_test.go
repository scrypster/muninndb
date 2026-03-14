package rest

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

type restTestEnrichPlugin struct {
	result *plugin.EnrichmentResult
}

func (p *restTestEnrichPlugin) Name() string            { return "rest-test-enrich" }
func (p *restTestEnrichPlugin) Tier() plugin.PluginTier { return plugin.TierEnrich }
func (p *restTestEnrichPlugin) Init(_ context.Context, _ plugin.PluginConfig) error {
	return nil
}
func (p *restTestEnrichPlugin) Close() error { return nil }
func (p *restTestEnrichPlugin) Enrich(_ context.Context, _ *plugin.Engram) (*plugin.EnrichmentResult, error) {
	return p.result, nil
}

type restNoopEmbedder struct{}

func (e *restNoopEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	return make([]float32, 384), nil
}

func (e *restNoopEmbedder) Tokenize(text string) []string {
	var tokens []string
	current := ""
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			if current != "" {
				tokens = append(tokens, current)
				current = ""
			}
			continue
		}
		current += string(r)
	}
	if current != "" {
		tokens = append(tokens, current)
	}
	return tokens
}

type restFTSAdapter struct{ idx *fts.Index }

func (a *restFTSAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]activation.ScoredID, error) {
	results, err := a.idx.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]activation.ScoredID, len(results))
	for i, r := range results {
		out[i] = activation.ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

type restFTSTriggerAdapter struct{ idx *fts.Index }

func (a *restFTSTriggerAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]trigger.ScoredID, error) {
	results, err := a.idx.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]trigger.ScoredID, len(results))
	for i, r := range results {
		out[i] = trigger.ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

func newRESTRetryEnrichEnv(t *testing.T) (*engine.Engine, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "muninndb-rest-retry-enrich-*")
	if err != nil {
		t.Fatal(err)
	}

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := &restNoopEmbedder{}
	actEngine := activation.New(store, &restFTSAdapter{idx: ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &restFTSTriggerAdapter{idx: ftsIdx}, nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestRESTEngineWrapperRetryEnrich_PersistsDigestAndGraphData(t *testing.T) {
	eng, cleanup := newRESTRetryEnrichEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "rest-retry-enrich"
	writeResp, err := eng.Write(ctx, &WriteRequest{
		Vault:   vault,
		Concept: "retry enrich regression",
		Content: "service relies on postgres and redis",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	wrapper := &RESTEngineWrapper{engine: eng, enricher: &restTestEnrichPlugin{
		result: &plugin.EnrichmentResult{
			Summary:    "service depends on postgres and redis",
			KeyPoints:  []string{"postgres stores state", "redis handles caching"},
			MemoryType: "task",
			TypeLabel:  "dependency_note",
			Entities: []plugin.ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
				{Name: "redis", Type: "database", Confidence: 0.8},
			},
			Relationships: []plugin.ExtractedRelation{
				{FromEntity: "postgres", ToEntity: "redis", RelType: "cooperates_with", Weight: 0.7},
			},
		},
	}}

	ulid, err := storage.ParseULID(writeResp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}

	if _, err := wrapper.RetryEnrich(ctx, vault, writeResp.ID); err != nil {
		t.Fatalf("RetryEnrich: %v", err)
	}

	ws := eng.Store().ResolveVaultPrefix(vault)
	got, err := eng.Store().GetEngram(ctx, ws, ulid)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Summary != "service depends on postgres and redis" {
		t.Fatalf("Summary = %q", got.Summary)
	}
	if got.MemoryType != storage.TypeTask {
		t.Fatalf("MemoryType = %v, want %v", got.MemoryType, storage.TypeTask)
	}
	if got.TypeLabel != "dependency_note" {
		t.Fatalf("TypeLabel = %q", got.TypeLabel)
	}

	flags, err := eng.Store().GetDigestFlags(ctx, ulid)
	if err != nil {
		t.Fatalf("GetDigestFlags: %v", err)
	}
	if flags&plugin.DigestSummarized == 0 {
		t.Fatalf("expected DigestSummarized flag, flags=%08b", flags)
	}
	if flags&plugin.DigestClassified == 0 {
		t.Fatalf("expected DigestClassified flag, flags=%08b", flags)
	}
	if flags&plugin.DigestEntities == 0 {
		t.Fatalf("expected DigestEntities flag, flags=%08b", flags)
	}
	if flags&plugin.DigestRelationships == 0 {
		t.Fatalf("expected DigestRelationships flag, flags=%08b", flags)
	}

	postgres, err := eng.Store().GetEntityRecord(ctx, "postgres")
	if err != nil {
		t.Fatalf("GetEntityRecord(postgres): %v", err)
	}
	if postgres == nil {
		t.Fatal("expected postgres entity record")
	}

	var linkedEngramIDs []storage.ULID
	if err := eng.Store().ScanEntityEngrams(ctx, "postgres", func(gotWS [8]byte, id storage.ULID) error {
		if gotWS == ws {
			linkedEngramIDs = append(linkedEngramIDs, id)
		}
		return nil
	}); err != nil {
		t.Fatalf("ScanEntityEngrams: %v", err)
	}
	if len(linkedEngramIDs) == 0 || linkedEngramIDs[0] != ulid {
		t.Fatalf("expected postgres entity link for engram %s, got %v", writeResp.ID, linkedEngramIDs)
	}

	var relationships []storage.RelationshipRecord
	if err := eng.Store().ScanRelationships(ctx, ws, func(record storage.RelationshipRecord) error {
		relationships = append(relationships, record)
		return nil
	}); err != nil {
		t.Fatalf("ScanRelationships: %v", err)
	}
	if len(relationships) == 0 {
		t.Fatal("expected retry-enrich to persist relationship records")
	}
	coOccurrenceSeen := false
	for _, rel := range relationships {
		if rel.FromEntity == "postgres" && rel.ToEntity == "redis" && rel.RelType == "cooperates_with" {
			coOccurrenceSeen = true
			break
		}
	}
	if !coOccurrenceSeen {
		t.Fatalf("expected persisted relationship for postgres->redis, got %+v", relationships)
	}

	clusterCount := 0
	if err := eng.Store().ScanEntityClusters(ctx, ws, 1, func(nameA, nameB string, count int) error {
		if (nameA == "postgres" && nameB == "redis") || (nameA == "redis" && nameB == "postgres") {
			clusterCount = count
		}
		return nil
	}); err != nil {
		t.Fatalf("ScanEntityClusters: %v", err)
	}
	if clusterCount == 0 {
		t.Fatal("expected retry-enrich to persist co-occurrence data")
	}

	links, err := wrapper.GetEngramLinks(ctx, &GetEngramLinksRequest{ID: writeResp.ID, Vault: vault})
	if err != nil {
		t.Fatalf("GetEngramLinks: %v", err)
	}
	if len(links.Links) != 0 {
		t.Fatalf("expected entity/relationship persistence without engram-association side effects, got %+v", links.Links)
	}
}

func TestRESTEngineWrapperRetryEnrich_NilResultReturnsError(t *testing.T) {
	eng, cleanup := newRESTRetryEnrichEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "rest-retry-enrich-nil"
	writeResp, err := eng.Write(ctx, &WriteRequest{
		Vault:   vault,
		Concept: "retry enrich nil result",
		Content: "plugin returned nil",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	wrapper := &RESTEngineWrapper{engine: eng, enricher: &restTestEnrichPlugin{}}
	if _, err := wrapper.RetryEnrich(ctx, vault, writeResp.ID); err == nil {
		t.Fatal("expected RetryEnrich to reject nil enrichment result")
	}
}
