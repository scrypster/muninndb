package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
)

// muninn_status used to return the literal health "good" no matter what. This
// engine has NO active embedder (the noop embedder: recall is keyword-only), so
// "good" is false here, and the response must say why.
func TestStatus_HealthIsDerivedNotConstant(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()

	out := callTool(t, srv, "muninn_status", `{"vault":"railway-status"}`)
	if got := out["health"]; got != "degraded" {
		t.Fatalf("health = %v, want \"degraded\" for an engine with no active embedder. full: %v", got, out)
	}
	reasons, _ := out["health_reasons"].([]any)
	found := false
	for _, r := range reasons {
		if s, _ := r.(string); strings.Contains(s, "embedder") {
			found = true
		}
	}
	if !found {
		t.Fatalf("health_reasons = %v, want a reason naming the embedder", out["health_reasons"])
	}
	emb, _ := out["embedder"].(map[string]any)
	if emb["state"] != "disabled" {
		t.Errorf("embedder.state = %v, want disabled", emb["state"])
	}
	workers, _ := out["workers"].(map[string]any)
	for _, name := range []string{"hebbian", "contradiction", "confidence"} {
		w, _ := workers[name].(map[string]any)
		if w == nil {
			t.Errorf("workers.%s missing: %v", name, out["workers"])
			continue
		}
		// No cognitive workers are wired on this engine: reported, not hidden,
		// and not by itself a degradation.
		if w["state"] != "disabled" {
			t.Errorf("workers.%s.state = %v, want disabled", name, w["state"])
		}
	}
	// Backward compatibility: the pre-existing fields are still present.
	for _, k := range []string{"vault", "total_memories", "enrichment_mode"} {
		if _, ok := out[k]; !ok {
			t.Errorf("legacy field %q missing from status: %v", k, out)
		}
	}
}

// TestDeriveHealth pins the verdict table: what degrades health and what is
// merely reported.
func TestDeriveHealth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	on := func(status string) cognitive.WorkerStats {
		return cognitive.WorkerStats{Enabled: true, Status: status, LastRun: now.Add(-90 * time.Second).UnixNano()}
	}
	healthy := cognitive.EngineWorkerStats{Hebbian: on("active"), Contradict: on("idle"), Confidence: on("dormant")}
	failing := healthy
	failing.Contradict.LastBatchFailed = true
	failing.Contradict.Errors = 3
	stopped := healthy
	stopped.Hebbian.Status = "stopped"
	off := cognitive.EngineWorkerStats{Hebbian: cognitive.DisabledWorkerStats(), Contradict: cognitive.DisabledWorkerStats(), Confidence: cognitive.DisabledWorkerStats()}

	for _, tc := range []struct {
		name        string
		embed       string
		workers     cognitive.EngineWorkerStats
		wantHealth  string
		wantReasons []string // substrings, one per expected reason
	}{
		{"all up, dormant worker is not degraded", "up", healthy, "good", nil},
		{"embedder not yet exercised", "unknown", healthy, "good", nil},
		{"workers not configured on this node", "up", off, "good", nil},
		{"embedder fell back to keyword", "degraded_to_keyword", healthy, "degraded", []string{"embedder degraded"}},
		{"no embedder", "disabled", healthy, "degraded", []string{"embedder disabled"}},
		{"worker erroring", "up", failing, "degraded", []string{"contradiction worker erroring"}},
		{"worker stopped", "up", stopped, "degraded", []string{"hebbian worker stopped"}},
		{"two causes, two reasons", "disabled", failing, "degraded", []string{"embedder disabled", "contradiction worker erroring"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, reasons, _, workers := deriveHealth(healthSnapshot{EmbedState: tc.embed, Workers: tc.workers, Now: now})
			if h != tc.wantHealth {
				t.Fatalf("health = %q, want %q (reasons %v)", h, tc.wantHealth, reasons)
			}
			if len(reasons) != len(tc.wantReasons) {
				t.Fatalf("reasons = %v, want %d matching %v", reasons, len(tc.wantReasons), tc.wantReasons)
			}
			for i, want := range tc.wantReasons {
				if !strings.Contains(reasons[i], want) {
					t.Errorf("reason[%d] = %q, want it to contain %q", i, reasons[i], want)
				}
			}
			if tc.workers.Confidence.Enabled {
				c := workers["confidence"]
				if c.State != "dormant" || c.LastRunAgeSeconds == nil || *c.LastRunAgeSeconds != 90 {
					t.Errorf("confidence = %+v, want dormant with last_run_age_seconds 90", c)
				}
			}
			if tc.workers.Contradict.LastBatchFailed && workers["contradiction"].State != "erroring" {
				t.Errorf("contradiction state = %q, want erroring", workers["contradiction"].State)
			}
		})
	}
}

// TestStatus_UnwritableStorageIsDegraded: health used to ignore storage, so a
// read-only or full disk still reported its verdict from the embedder and
// workers alone. The probe is the same one REST /api/health reports as
// db_writable.
func TestStatus_UnwritableStorageIsDegraded(t *testing.T) {
	_, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	srv.SetStorageWritableProbe(func() bool { return false })
	out := callTool(t, srv, "muninn_status", `{"vault":"railway-disk"}`)
	if out["storage"] != "not_writable" {
		t.Errorf("storage = %v, want not_writable", out["storage"])
	}
	found := false
	for _, r := range out["health_reasons"].([]any) {
		if strings.Contains(r.(string), "storage not writable") {
			found = true
		}
	}
	if out["health"] != "degraded" || !found {
		t.Fatalf("health = %v reasons = %v; want degraded with a storage reason", out["health"], out["health_reasons"])
	}
	srv.SetStorageWritableProbe(func() bool { return true })
	if out := callTool(t, srv, "muninn_status", `{"vault":"railway-disk"}`); out["storage"] != "writable" {
		t.Errorf("storage = %v, want writable", out["storage"])
	}
}
