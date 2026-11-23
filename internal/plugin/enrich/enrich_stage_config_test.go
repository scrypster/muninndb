package enrich

import (
	"reflect"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
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
