package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	enrichpkg "github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/storage"
)

// stageRecordingOllama stands in for Ollama's /api/chat. It names each request
// after the pipeline stage whose system prompt it carries ("probe" for any
// other prompt, which is the connectivity check in Init), records the name, and
// answers with a well-formed reply for that stage.
type stageRecordingOllama struct {
	mu     sync.Mutex
	stages []string
}

func (o *stageRecordingOllama) calls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.stages...)
}

// newStageRecordingOllama starts the stub and returns it with an ollama://
// provider URL that points at it.
func newStageRecordingOllama(t *testing.T) (*stageRecordingOllama, string) {
	t.Helper()
	prompts := enrichpkg.DefaultPrompts()
	stageOf := map[string]string{
		prompts.EntitiesSystem:      "entities",
		prompts.RelationshipsSystem: "relationships",
		prompts.ClassifySystem:      "classification",
		prompts.SummarizeSystem:     "summary",
	}
	replies := map[string]string{
		"probe":          "OK",
		"entities":       `{"entities":[{"name":"Ferngrove","type":"project","confidence":0.9},{"name":"PostgreSQL","type":"database","confidence":0.9}]}`,
		"relationships":  `{"relationships":[{"from":"Ferngrove","to":"PostgreSQL","type":"uses","weight":0.8}]}`,
		"classification": `{"memory_type":"decision","type_label":"storage_decision","category":"infrastructure","subcategory":"databases"}`,
		"summary":        `{"summary":"Ferngrove moves its event store to PostgreSQL.","key_points":["event store on PostgreSQL"]}`,
	}
	o := &stageRecordingOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Messages) == 0 {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		stage, ok := stageOf[req.Messages[0].Content]
		if !ok {
			stage = "probe"
		}
		o.mu.Lock()
		o.stages = append(o.stages, stage)
		o.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": replies[stage]},
		})
	}))
	t.Cleanup(srv.Close)
	return o, "ollama://" + srv.Listener.Addr().String() + "/test-model"
}

// TestBuildEnricher_StageConfigReachesPipeline pins #883: enrich_mode and the
// enrich_* flags from plugin_config.json reach the pipeline buildEnricher
// builds, whichever source the provider URL comes from. Before the fix nothing
// handed the config to the service, the pipeline saw nil, and every stage ran.
// Each case enriches one memory that has no summary, key points or type, so
// no stage is skipped for data the memory already has.
func TestBuildEnricher_StageConfigReachesPipeline(t *testing.T) {
	off := false
	cases := []struct {
		name     string
		urlInEnv bool // provider URL from MUNINN_ENRICH_URL instead of plugin_config.json
		cfg      plugincfg.PluginConfig
		want     []string // stages that call the provider, in order
		wantLog  string
	}{
		{
			name:    "light mode",
			cfg:     plugincfg.PluginConfig{EnrichMode: "light"},
			want:    []string{"summary"},
			wantLog: "mode=light stages=summary",
		},
		{
			name:     "light mode, URL from MUNINN_ENRICH_URL",
			urlInEnv: true,
			cfg:      plugincfg.PluginConfig{EnrichMode: "light"},
			want:     []string{"summary"},
			wantLog:  "mode=light stages=summary",
		},
		{
			// Relationships need the entities stage's output, so they do not run either.
			name:    "full mode, enrich_entities false",
			cfg:     plugincfg.PluginConfig{EnrichEntities: &off},
			want:    []string{"classification", "summary"},
			wantLog: "mode=full stages=classification,summary",
		},
		{
			// Control: passes with and without the fix, and shows the stub
			// answers every stage well enough for the pipeline to continue.
			name:    "no stage config",
			cfg:     plugincfg.PluginConfig{},
			want:    []string{"entities", "relationships", "classification", "summary"},
			wantLog: "mode=full stages=entities,relationships,classification,summary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ollama, url := newStageRecordingOllama(t)
			t.Setenv("MUNINN_ENRICH_URL", "")
			t.Setenv("MUNINN_ENRICH_API_KEY", "")
			t.Setenv("MUNINN_ANTHROPIC_KEY", "")
			t.Setenv("MUNINN_GOOGLE_KEY", "")
			cfg := tc.cfg
			if tc.urlInEnv {
				t.Setenv("MUNINN_ENRICH_URL", url)
			} else {
				cfg.EnrichURL = url
			}

			var p plugin.EnrichPlugin
			var failure *enrichInitFailure
			logged := captureLog(t, func() { p, failure = buildEnricher(context.Background(), cfg) })
			if failure != nil {
				t.Fatalf("buildEnricher: %v", failure.err)
			}
			t.Cleanup(func() { _ = p.Close() })
			if got := ollama.calls(); !reflect.DeepEqual(got, []string{"probe"}) {
				t.Fatalf("provider calls during init = %v, want only the probe", got)
			}
			if !strings.Contains(logged, `level=INFO msg="enrich stages" `+tc.wantLog) {
				t.Errorf("startup log has no INFO line %q:\n%s", tc.wantLog, logged)
			}

			eng := &storage.Engram{
				ID:      storage.NewULID(),
				Concept: "event store choice",
				Content: "Ferngrove moves its event store to PostgreSQL.",
			}
			if _, err := p.Enrich(context.Background(), eng); err != nil {
				t.Fatalf("Enrich: %v", err)
			}
			if got := ollama.calls()[1:]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stages that called the provider after the init probe = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildEnricher_VerboseLogsReachPipeline: llm_verbose_logs from
// plugin_config.json reaches the enrich pipeline through the same config as
// the stage flags (#883), so enrich calls are logged without the env var.
func TestBuildEnricher_VerboseLogsReachPipeline(t *testing.T) {
	_, url := newStageRecordingOllama(t)
	t.Setenv("MUNINN_ENRICH_URL", "")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_LLM_VERBOSE_LOGS", "")
	on := true
	p, failure := buildEnricher(context.Background(), plugincfg.PluginConfig{
		EnrichURL: url, EnrichMode: "light", LLMVerboseLogs: &on,
	})
	if failure != nil {
		t.Fatalf("buildEnricher: %v", failure.err)
	}
	t.Cleanup(func() { _ = p.Close() })

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "event store choice", Content: "Ferngrove moves its event store to PostgreSQL."}
	out := captureLog(t, func() {
		if _, err := p.Enrich(context.Background(), eng); err != nil {
			t.Errorf("Enrich: %v", err)
		}
	})
	if !strings.Contains(out, "msg=llm.complete") || !strings.Contains(out, "call_type=summary") {
		t.Fatalf("llm_verbose_logs is set but the enrich call was not logged:\n%s", out)
	}
}

// TestLogEnrichStages_UnknownModeWarns: an enrich_mode the pipeline does not
// know runs full mode. That is stated, not left silent.
func TestLogEnrichStages_UnknownModeWarns(t *testing.T) {
	stages := []string{"entities", "relationships", "classification", "summary"}
	out := captureLog(t, func() {
		logEnrichStages(plugincfg.PluginConfig{EnrichMode: "Light"}, stages)
	})
	got := lines(out)
	if len(got) != 2 {
		t.Fatalf("want a WARN and the stage line, got %d lines:\n%s", len(got), out)
	}
	if !strings.Contains(got[0], "level=WARN") || !strings.Contains(got[0], "enrich_mode=Light") {
		t.Errorf("first line should WARN about the unknown enrich_mode: %q", got[0])
	}
	if !strings.Contains(got[1], "mode=full stages=entities,relationships,classification,summary") {
		t.Errorf("second line should report full mode with every stage: %q", got[1])
	}

	for _, mode := range []string{"", "full", "light"} {
		out := captureLog(t, func() { logEnrichStages(plugincfg.PluginConfig{EnrichMode: mode}, stages) })
		if strings.Contains(out, "level=WARN") {
			t.Errorf("enrich_mode %q is valid and must not WARN:\n%s", mode, out)
		}
	}
}

// TestBuildEnricher_NoStageWarns: with stage flags under which no stage can
// run, the enricher still starts and the background pass marks every memory
// done without enriching it. The startup line says so at WARN, not INFO.
// enrich_relationships is left on: relationships need the entities stage, so
// they cannot run either, and the stage set comes from the real EnabledStages.
func TestBuildEnricher_NoStageWarns(t *testing.T) {
	_, url := newStageRecordingOllama(t)
	t.Setenv("MUNINN_ENRICH_URL", "")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	off := false
	cfg := plugincfg.PluginConfig{
		EnrichURL:            url,
		EnrichEntities:       &off,
		EnrichClassification: &off,
		EnrichSummary:        &off,
	}

	var p plugin.EnrichPlugin
	var failure *enrichInitFailure
	out := captureLog(t, func() { p, failure = buildEnricher(context.Background(), cfg) })
	if failure != nil {
		t.Fatalf("buildEnricher: %v", failure.err)
	}
	t.Cleanup(func() { _ = p.Close() })

	var stageLines []string
	for _, l := range lines(out) {
		if strings.Contains(l, `msg="enrich stages`) {
			stageLines = append(stageLines, l)
		}
	}
	if len(stageLines) != 1 {
		t.Fatalf("want one enrich stages line, got %d:\n%s", len(stageLines), out)
	}
	if !strings.Contains(stageLines[0], "level=WARN") || !strings.Contains(stageLines[0], "mode=full stages=none") {
		t.Fatalf("with no stage able to run, the startup line must be a WARN with mode=full stages=none: %q", stageLines[0])
	}
}
