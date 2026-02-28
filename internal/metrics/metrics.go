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
