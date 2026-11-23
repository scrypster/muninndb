package engine

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// stageReportingEnrichPlugin is mockEnrichPlugin plus plugin.StageReporter,
// which enrich.EnrichService implements in production.
type stageReportingEnrichPlugin struct {
	mockEnrichPlugin
	stages []string
}

func (m *stageReportingEnrichPlugin) EnabledStages() []string { return m.stages }

// TestReplayEnrichment_SkipsStagesSwitchedOffByConfig pins #883 for replay. In
// light mode the enrich plugin runs the summary stage only, so it never sets
// the entities, relationships or classification flag. Replay runs that plugin,
// so those stages must not count as pending. Before the fix a default replay
// selected every memory on every call and called Enrich for each, and a dry
// run reported memories the plugin had finished as work to do.
func TestReplayEnrichment_SkipsStagesSwitchedOffByConfig(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "default"

	// Three memories. The light-mode plugin already summarized the first two.
	ids := make([]storage.ULID, 3)
	for i := range ids {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("concept %d", i),
			Content: fmt.Sprintf("content %d", i),
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		if ids[i], err = storage.ParseULID(resp.ID); err != nil {
			t.Fatalf("ParseULID[%d]: %v", i, err)
		}
	}
	for _, id := range ids[:2] {
		for _, flag := range []uint16{plugin.DigestSummarized, plugin.DigestEnrich} {
			if err := eng.store.SetDigestFlag(ctx, id, flag); err != nil {
				t.Fatalf("SetDigestFlag(%#x): %v", flag, err)
			}
		}
	}

	mock := &stageReportingEnrichPlugin{stages: []string{"summary"}}
	eng.SetEnrichPlugin(mock)

	dry, err := eng.ReplayEnrichment(ctx, vault, nil, 50, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Processed != 1 || dry.Skipped != 2 || !reflect.DeepEqual(dry.StagesRun, []string{"summary"}) {
		t.Fatalf("dry run: processed=%d skipped=%d stages_run=%v, want processed=1 skipped=2 stages_run=[summary]",
			dry.Processed, dry.Skipped, dry.StagesRun)
	}

	res, err := eng.ReplayEnrichment(ctx, vault, nil, 50, false)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if mock.calls != 1 || res.Processed != 1 || res.Skipped != 2 || res.Failed != 0 {
		t.Fatalf("replay: enrich calls=%d processed=%d skipped=%d failed=%d, want 1/1/2/0",
			mock.calls, res.Processed, res.Skipped, res.Failed)
	}
	assertResultInvariant(t, res, 3)

	// The replay summarized the third memory; nothing is pending any more.
	again, err := eng.ReplayEnrichment(ctx, vault, nil, 50, false)
	if err != nil {
		t.Fatalf("second replay: %v", err)
	}
	if mock.calls != 1 || again.Processed != 0 || again.Skipped != 3 {
		t.Fatalf("second replay: enrich calls=%d processed=%d skipped=%d, want 1/0/3",
			mock.calls, again.Processed, again.Skipped)
	}
}

// TestReplayEnrichment_RefusesStageSwitchedOffByConfig: a stage the caller
// names but the plugin does not run is refused, not dropped, so a replay is
// never reported for a stage that did not run. With every stage off the
// default request is refused too. The refusal gives the reason the engine can
// derive from the reported stages: relationships that cannot run because
// entities are off are not reported as switched off by their own flag.
func TestReplayEnrichment_RefusesStageSwitchedOffByConfig(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const byConfig = "switched off by enrich_mode or an enrich_* flag in plugin_config.json"

	cases := []struct {
		name    string
		enabled []string // the stages the plugin reports
		request []string
		want    string
	}{
		{
			name:    "light mode, stages named",
			enabled: []string{"summary"},
			request: []string{"summary", "entities", "classification"},
			want:    "does not run the entities and classification stages (" + byConfig + "); replay runs that plugin and cannot fill them",
		},
		{
			name:    "entities off, relationships named",
			enabled: []string{"classification", "summary"},
			request: []string{"relationships"},
			want:    "does not run the relationships stage (relationships are extracted from the entities stage's output, and the entities stage is " + byConfig + "); replay runs that plugin and cannot fill it",
		},
		{
			name:    "relationships off, entities on",
			enabled: []string{"entities", "classification", "summary"},
			request: []string{"relationships"},
			want:    "does not run the relationships stage (" + byConfig + "); replay runs that plugin and cannot fill it",
		},
		{
			name:    "every stage off, default request",
			enabled: []string{},
			request: nil,
			want:    "does not run the entities, classification and summary stages (" + byConfig + "), nor the relationships stage (relationships are extracted from the entities stage's output); replay runs that plugin and cannot fill them",
		},
	}
	for _, tc := range cases {
		eng.SetEnrichPlugin(&stageReportingEnrichPlugin{stages: tc.enabled})
		for _, dryRun := range []bool{true, false} {
			_, err := eng.ReplayEnrichment(ctx, "default", tc.request, 50, dryRun)
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
				t.Errorf("%s, dry_run=%v: error = %v, want it to end with %q", tc.name, dryRun, err, tc.want)
			}
		}
	}
}
