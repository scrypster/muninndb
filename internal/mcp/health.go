package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
)

// healthSnapshot is everything muninn_status derives `health` from. Every
// input is already held in memory (atomics and in-process counters) — status is
// called often by agents, so nothing here may scan the store or call the
// embedder.
type healthSnapshot struct {
	EmbedState        string // engine.EmbedHealth.State
	EmbedLastOKAt     time.Time
	EmbedLastDegraded time.Time
	// EmbedDegradedReason is engine.EmbedHealth.LastDegradedReason.
	EmbedDegradedReason string
	Workers             cognitive.EngineWorkerStats
	// EmbeddingsPending is the retroactive embed backlog when a backfill is
	// running; nil when no backfill is running (unknown is not zero).
	EmbeddingsPending *int64
	// StorageWritable is the data-directory writability probe; nil = unknown.
	StorageWritable *bool
	Now             time.Time
}

// healthReporter is probed (not added to EngineInterface) so test engines and
// any embedder of this package that cannot observe health report "unknown"
// rather than a fabricated "good".
type healthReporter interface {
	HealthSnapshot(ctx context.Context) healthSnapshot
}

// EmbedderStatus is the embedder block of muninn_status.
type EmbedderStatus struct {
	// State: up | degraded_to_keyword | disabled | unknown (see engine.EmbedHealth).
	State          string `json:"state"`
	LastOKAt       string `json:"last_ok_at,omitempty"`
	LastDegradedAt string `json:"last_degraded_at,omitempty"`
	// LastDegradedReason is why the most recent degraded recall fell back:
	// embed_timeout | embed_unavailable | embed_zero_vector |
	// stored_embedding_read (see semantic_degraded_reason on muninn_recall).
	LastDegradedReason string `json:"last_degraded_reason,omitempty"`
}

// WorkerHealth is one cognitive worker in muninn_status.
type WorkerHealth struct {
	// State: active | idle | dormant | stopped | disabled | erroring.
	// "erroring" means the most recent batch failed; "dormant" means nothing
	// has been submitted recently — reported, but not by itself degraded.
	State             string `json:"state"`
	LastRunAgeSeconds *int64 `json:"last_run_age_seconds,omitempty"`
	Errors            uint64 `json:"errors"`
}

// deriveHealth turns a snapshot into the status verdict. "good" only when the
// embedder is up (or not yet exercised — see below) and no worker is erroring
// or stopped; otherwise "degraded" with one reason per cause.
//
// An embedder that no recall has exercised since startup ("unknown") does not
// degrade health: there is no evidence either way, and a probe would cost an
// embedding call on every status. It is reported in the embedder block so the
// caller can see the verdict rests on no observation.
func deriveHealth(s healthSnapshot) (health string, reasons []string, emb EmbedderStatus, workers map[string]WorkerHealth) {
	reasons = []string{}
	if s.StorageWritable != nil && !*s.StorageWritable {
		reasons = append(reasons, "storage not writable: the data directory refused a probe write (read-only or full disk) — new memories cannot be stored")
	}
	emb = EmbedderStatus{State: s.EmbedState}
	if !s.EmbedLastOKAt.IsZero() {
		emb.LastOKAt = s.EmbedLastOKAt.UTC().Format(time.RFC3339)
	}
	if !s.EmbedLastDegraded.IsZero() {
		emb.LastDegradedAt = s.EmbedLastDegraded.UTC().Format(time.RFC3339)
		emb.LastDegradedReason = s.EmbedDegradedReason
	}
	switch s.EmbedState {
	case "up", "unknown":
	case "disabled":
		reasons = append(reasons, "embedder disabled: no embedding provider is active (not configured, or unreachable at startup) — recall is keyword-only (BM25)")
	case "degraded_to_keyword":
		reasons = append(reasons, embedDegradedReason(s.EmbedDegradedReason))
	default:
		reasons = append(reasons, fmt.Sprintf("embedder state %q is not recognised", s.EmbedState))
	}

	workers = map[string]WorkerHealth{}
	for _, w := range []struct {
		name string
		st   cognitive.WorkerStats
	}{
		{"hebbian", s.Workers.Hebbian},
		{"contradiction", s.Workers.Contradict},
		{"confidence", s.Workers.Confidence},
	} {
		wh := WorkerHealth{State: w.st.Status, Errors: w.st.Errors}
		if wh.State == "" {
			wh.State = "unknown"
		}
		if w.st.LastRun > 0 {
			age := int64(s.Now.Sub(time.Unix(0, w.st.LastRun)).Seconds())
			if age < 0 {
				age = 0
			}
			wh.LastRunAgeSeconds = &age
		}
		switch {
		case w.st.Enabled && w.st.LastBatchFailed:
			wh.State = "erroring"
			reasons = append(reasons, fmt.Sprintf("%s worker erroring: its most recent batch failed (%d failed batches since start)", w.name, w.st.Errors))
		case w.st.Enabled && wh.State == "stopped":
			reasons = append(reasons, fmt.Sprintf("%s worker stopped: it is configured but no longer processing", w.name))
		}
		workers[w.name] = wh
	}

	if len(reasons) == 0 {
		return "good", reasons, emb, workers
	}
	return "degraded", reasons, emb, workers
}

// embedDegradedReason is the health_reasons entry for degraded_to_keyword,
// naming the cause when the engine recorded one: a busy embedder (the query
// embed missed its budget) is a capacity condition an operator handles very
// differently from an unreachable backend.
func embedDegradedReason(reason string) string {
	const base = "embedder degraded: the most recent recall could not embed its query and fell back to keyword-only (BM25) ranking"
	switch reason {
	case "embed_timeout":
		return base + " — the query embed missed its time budget (the embedder was busy, e.g. embedding a backfill, or slow); the backend is up (reason: embed_timeout)"
	case "embed_unavailable":
		return base + " — the embed call failed (reason: embed_unavailable)"
	case "":
		return base
	default:
		return base + " (reason: " + reason + ")"
	}
}
