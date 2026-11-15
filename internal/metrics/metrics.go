package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	EngineWritesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "muninndb_engine_writes_total",
		Help: "Total number of engrams written",
	})
	EngineActivationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "muninndb_engine_activations_total",
		Help: "Total number of activation calls",
	})
	FTSSearchDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "muninndb_fts_search_duration_seconds",
		Help:    "FTS search latency",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5},
	})
	NoveltyDropsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "muninndb_novelty_drops_total",
		Help: "Total novelty jobs silently dropped due to full channel",
	})
	RESTRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muninn_rest_request_duration_seconds",
		Help:    "HTTP request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "status_class"})

	RateLimitRejections = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninn_rate_limit_rejections_total",
		Help: "Total number of requests rejected by rate limiting.",
	}, []string{"limiter_type"})

	ImportJobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninn_import_jobs_total",
		Help: "Total number of vault import jobs by completion status.",
	}, []string{"status"})

	FTSIndexFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninn_fts_index_failures_total",
		Help: "Total number of FTS index write failures during reindex.",
	}, []string{"vault"})

	WriteDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muninndb_write_duration_seconds",
		Help:    "Engine write latency per vault",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"vault"})

	ActivateDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muninndb_activate_duration_seconds",
		Help:    "Engine activation (recall) latency per vault",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
	}, []string{"vault"})

	ReadDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muninndb_read_duration_seconds",
		Help:    "Engine read latency per vault",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1},
	}, []string{"vault"})

	EmbedPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "muninndb_embed_pending",
		Help: "Number of engrams pending embedding",
	})

	// RecallEmbedFallbackTotal counts activation calls that degraded to
	// BM25-only recall because the embed backend could not be trusted
	// (unreachable, timed out even after #658's reserved budget, or returned
	// a garbage/zero embedding). Paired with muninndb_activate_duration_seconds's
	// per-vault _count (already the activation total) this gives an operator
	// rate(fallback)/rate(total) embed-flakiness signal without a separate
	// duplicate total counter (#606).
	RecallEmbedFallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_recall_embed_fallback_total",
		Help: "Total activation calls that degraded to BM25-only recall because the embed backend could not be trusted, per vault.",
	}, []string{"vault"})

	// RecallEmbedTimeoutTotal counts the subset of RecallEmbedFallbackTotal
	// whose query embed did not arrive within recall's embed budget (the embed
	// lane was busy, or the embedder was slow) — the fallback an operator can
	// fix with capacity rather than by repairing a broken backend.
	RecallEmbedTimeoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_recall_embed_timeout_total",
		Help: "Total activation calls that degraded to BM25-only recall because the query embed exceeded its budget, per vault.",
	}, []string{"vault"})

	// EmbedLaneWaitsTotal counts embed calls that found their lane of the
	// bundled local embedder busy and had to queue. lane is "query" (recall's
	// query embed) or "batch" (write-path and retroactive embeds).
	EmbedLaneWaitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_embed_lane_waits_total",
		Help: "Total local embed calls that queued for a busy lane, per lane (query|batch).",
	}, []string{"lane"})

	// EmbedLaneTimeoutsTotal counts embed calls whose context ended while they
	// were still queued for a lane, so they returned an error without running.
	EmbedLaneTimeoutsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_embed_lane_timeouts_total",
		Help: "Total local embed calls abandoned because their deadline passed while queued, per lane (query|batch).",
	}, []string{"lane"})

	// EmbedLaneWaitSeconds is how long a queued embed call waited for its lane
	// (only calls that actually queued are observed).
	EmbedLaneWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "muninndb_embed_lane_wait_seconds",
		Help:    "Time a queued local embed call waited for its lane, per lane (query|batch).",
		Buckets: []float64{0.001, 0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"lane"})

	// RecallErrorsTotal counts activation calls that hard-failed (returned an
	// error, not a degraded-but-successful result), labelled by a coarse
	// reason so an operator can separate caller-side cancellation/timeout
	// from an internal activation failure (#606).
	RecallErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_recall_errors_total",
		Help: "Total activation calls that returned a hard error, per vault and reason.",
	}, []string{"vault", "reason"})

	// MCPAuthTotal counts each authenticated MCP request by the credential
	// class that authenticated it. The three credential types are already
	// structurally distinct (auth.AuthContext.IsAPIKey / IsCapability /
	// neither-with-a-Token for the static mdb_ token / neither-with-no-Token
	// for open-server mode) — this surfaces that existing distinction as a
	// scrapeable signal so an operator can confirm zero static-token traffic
	// before retiring it (#648). Never logs or labels the credential value
	// itself, only its class.
	MCPAuthTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninn_mcp_auth_total",
		Help: "Total authenticated MCP requests by credential source (api_key, capability, static_token, open).",
	}, []string{"source"})

	// MCPArgRejectionsTotal counts MCP tool calls rejected for the SHAPE of
	// their arguments (JSON-RPC -32602), labelled by tool and a coarse reason
	// from a closed set: missing_field, conflicting_alias, invalid_params (the
	// generic catch-all every handler's -32602 falls into). Both labels are
	// bounded — tool is a registered tool name (unknown names are folded to
	// "unknown") and reason is a code constant — and no vault label is carried,
	// so the series cannot reveal anything about vault contents or names.
	MCPArgRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninn_mcp_arg_rejections_total",
		Help: "Total MCP tool calls rejected for argument shape (JSON-RPC -32602), by tool and reason.",
	}, []string{"tool", "reason"})

	// Label pipeline (slice 2a). Every label value is from a closed set; no
	// vault name, query id or memory id is ever a label, so the series reveal
	// nothing about vault contents. The muninn_feedback response is uniform;
	// these counters (and muninn_status's `labels` block) are the ONLY place
	// per-outcome information appears, and only in aggregate.

	// FeedbackLabelsTotal counts accepted label posts. first=true is a new
	// (query, memory, credential) label; first=false a repeat (counted on the
	// existing row, never stored as a new one).
	FeedbackLabelsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_feedback_labels_total",
		Help: "Accepted muninn_feedback labels, by membership (verified = against the recall event | token = against a read-only id's row tags), polarity (pos|neg), channel (direct|handoff) and first (true|false).",
	}, []string{"membership", "polarity", "channel", "first"})

	// FeedbackRejectedTotal counts label posts rejected for a semantic reason
	// (the caller still received the uniform success response).
	FeedbackRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_feedback_rejected_total",
		Help: "Rejected muninn_feedback label ids, by reason (not_in_recall|unknown_event|bad_mac|no_key).",
	}, []string{"reason"})

	// FeedbackInvalidTotal counts syntactically invalid feedback calls (the
	// caller received a -32602 error).
	FeedbackInvalidTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_feedback_invalid_total",
		Help: "muninn_feedback calls rejected for shape, by field (useful|form|query_id|id|limit|source).",
	}, []string{"field"})

	// FeedbackLegacyTotal counts the legacy no-query_id form; capped=true when
	// the per-(credential, memory, UTC day) access-bump cap suppressed a bump.
	FeedbackLegacyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_feedback_legacy_total",
		Help: "Legacy muninn_feedback calls (engram_id, no query_id; record no label), by whether the daily access-bump cap applied.",
	}, []string{"capped"})

	// FeedbackStormTotal is incremented when one (credential, memory, UTC day)
	// post count first reaches a threshold — repeats made visible without
	// storing them.
	FeedbackStormTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_feedback_storm_total",
		Help: "Times a (credential, memory, UTC day) feedback post count first reached a threshold (10|100).",
	}, []string{"threshold"})

	// RecallTotal is the server-side recall denominator, including read-only
	// recalls, which persist nothing.
	RecallTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "muninndb_recall_total",
		Help: "Completed recalls (Activate), by read_only (true|false).",
	}, []string{"read_only"})
)

// VaultStore is the subset of storage.PebbleStore methods needed by VaultEngramCollector.
type VaultStore interface {
	ListVaultNames() ([]string, error)
	ResolveVaultPrefix(name string) [8]byte
	GetVaultCount(ctx context.Context, ws [8]byte) int64
}

// VaultEngramCollector collects per-vault engram counts at scrape time.
type VaultEngramCollector struct {
	store VaultStore
	desc  *prometheus.Desc
}

// NewVaultEngramCollector creates a new VaultEngramCollector backed by store.
func NewVaultEngramCollector(store VaultStore) *VaultEngramCollector {
	return &VaultEngramCollector{
		store: store,
		desc: prometheus.NewDesc(
			"muninndb_vault_engrams",
			"Current number of engrams per vault",
			[]string{"vault"},
			nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *VaultEngramCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *VaultEngramCollector) Collect(ch chan<- prometheus.Metric) {
	vaults, err := c.store.ListVaultNames()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range vaults {
		ws := c.store.ResolveVaultPrefix(name)
		count := c.store.GetVaultCount(ctx, ws)
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), name)
	}
}
