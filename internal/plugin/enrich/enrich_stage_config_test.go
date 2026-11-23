package enrich

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine/circuit"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestEnrichService_EnabledStages pins the stage set the startup log reports
// (#883). It must match what Run does: light mode is summary only whatever the
// per-stage flags say, and relationships need the entities stage in the same run.
func TestEnrichService_EnabledStages(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		cfg  *config.PluginConfig
		want []string
	}{
		{"no config", nil, []string{"entities", "relationships", "classification", "summary"}},
		{"empty config", &config.PluginConfig{}, []string{"entities", "relationships", "classification", "summary"}},
		{"light", &config.PluginConfig{EnrichMode: "light"}, []string{"summary"}},
		{"light ignores enrich_summary false", &config.PluginConfig{EnrichMode: "light", EnrichSummary: &off}, []string{"summary"}},
		{"light ignores enrich_entities true", &config.PluginConfig{EnrichMode: "light", EnrichEntities: &on}, []string{"summary"}},
		{"entities off takes relationships with it", &config.PluginConfig{EnrichEntities: &off}, []string{"classification", "summary"}},
		{"classification off", &config.PluginConfig{EnrichClassification: &off}, []string{"entities", "relationships", "summary"}},
		{"every stage off", &config.PluginConfig{EnrichEntities: &off, EnrichRelationships: &off, EnrichClassification: &off, EnrichSummary: &off}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &EnrichService{}
			s.SetEnrichConfig(tc.cfg)
			if got := s.EnabledStages(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EnabledStages() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnrichService_NothingToEnrichDoesNotOpenCircuit: ErrNothingToEnrich
// means the pipeline had nothing to do, not that the provider failed, so it
// must not count against the circuit breaker (#883). Light mode makes it
// common: every memory that already has a summary returns it. Before the fix
// one such memory (maxFails 1 here, 5 in production) opened the circuit and
// the next memory, which did need a summary, got circuit.ErrOpen.
func TestEnrichService_NothingToEnrichDoesNotOpenCircuit(t *testing.T) {
	mock := NewMockLLMProvider()
	pipeline := NewPipeline(mock, NewTokenBucketLimiter(100, 100))
	pipeline.SetConfig(&config.PluginConfig{EnrichMode: "light"})
	es := &EnrichService{provider: mock, pipeline: pipeline, breaker: circuit.New(1, time.Hour)}

	summarized := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x", Summary: "caller summary"}
	if _, err := es.Enrich(context.Background(), summarized); !errors.Is(err, ErrNothingToEnrich) {
		t.Fatalf("Enrich(summarized) error = %v, want ErrNothingToEnrich", err)
	}
	if mock.callCount != 0 {
		t.Fatalf("provider calls for a summarized memory in light mode = %d, want 0", mock.callCount)
	}

	needsSummary := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	res, err := es.Enrich(context.Background(), needsSummary)
	if err != nil {
		t.Fatalf("Enrich(needs summary) error = %v, want success: nothing-to-enrich must not open the circuit", err)
	}
	if res.Summary == "" || mock.callCount != 1 {
		t.Fatalf("summary = %q after %d provider calls, want a summary from exactly 1 call", res.Summary, mock.callCount)
	}
	if got := es.breaker.StateString(); got != "closed" {
		t.Fatalf("breaker state = %s, want closed", got)
	}
}

// The engine's replay narrows its stages through this interface (#883).
var _ plugin.StageReporter = (*EnrichService)(nil)
