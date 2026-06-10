package engine

import (
	"context"
	"runtime"
	"strings"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/metrics/latency"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/llmstats"
)

// LLMStats holds aggregate LLM call metrics for enrich and embed subsystems.
type LLMStats struct {
	EnrichCalls  int64   `json:"enrich_calls"`
	EnrichErrors int64   `json:"enrich_errors"`
	EnrichAvgMs  float64 `json:"enrich_avg_latency_ms"`
	EmbedCalls   int64   `json:"embed_calls"`
	EmbedErrors  int64   `json:"embed_errors"`
	EmbedAvgMs   float64 `json:"embed_avg_latency_ms"`
}

// ObservabilitySnapshot is a full system snapshot for the observability endpoint.
type ObservabilitySnapshot struct {
	System     SystemStats                   `json:"system"`
	Storage    StorageStats                  `json:"storage"`
	Processors []ProcessorStats              `json:"processors"`
	Workers    WorkerStatsSnapshot           `json:"cognitive_workers"`
	Vaults     map[string]VaultObservability `json:"vaults"`
	LLM        *LLMStats                     `json:"llm,omitempty"`
}

// SystemStats holds Go runtime and process-level metrics.
type SystemStats struct {
	UptimeSeconds   int64  `json:"uptime_seconds"`
	Version         string `json:"version"`
	GoRoutines      int    `json:"go_routines"`
	GoMemAllocBytes uint64 `json:"go_mem_alloc_bytes"`
	GoMemSysBytes   uint64 `json:"go_mem_sys_bytes"`
	GoGCPauseNs     uint64 `json:"go_gc_pause_ns"`
}

// PebbleStats holds selected Pebble storage engine metrics.
type PebbleStats struct {
	CompactionCount     int64   `json:"compaction_count"`
	CompactionDebtBytes uint64  `json:"compaction_debt_bytes"`
	CacheHits           int64   `json:"cache_hits"`
	CacheMisses         int64   `json:"cache_misses"`
	CacheHitRate        float64 `json:"cache_hit_rate"`
	ReadAmp             int     `json:"read_amp"`
	NumSSTables         int64   `json:"num_sstables"`
	MemTableBytes       uint64  `json:"mem_table_bytes"`
	WALBytes            uint64  `json:"wal_bytes"`
}

// StorageStats holds disk and Pebble engine metrics.
type StorageStats struct {
	DiskBytes int64       `json:"disk_bytes"`
	Pebble    PebbleStats `json:"pebble"`
}

// ProcessorStats holds the status of a retroactive background processor.
type ProcessorStats struct {
	Name       string  `json:"name"`
	PluginName string  `json:"plugin_name"`
	Status     string  `json:"status"`
	Processed  int64   `json:"processed"`
	Total      int64   `json:"total"`
	Pending    int64   `json:"pending"`
	RatePerSec float64 `json:"rate_per_sec"`
	ETASeconds int64   `json:"eta_seconds"`
	Errors     int64   `json:"errors"`
}

// WorkerStatsSnapshot holds cognitive worker statistics.
type WorkerStatsSnapshot struct {
	Hebbian       cognitive.WorkerStats `json:"hebbian"`
	Contradiction cognitive.WorkerStats `json:"contradiction"`
	Confidence    cognitive.WorkerStats `json:"confidence"`
}

// CoherenceStats holds vault-level coherence metrics.
type CoherenceStats struct {
	Score                float64 `json:"score"`
	OrphanRatio          float64 `json:"orphan_ratio"`
	ContradictionDensity float64 `json:"contradiction_density"`
	DuplicationPressure  float64 `json:"duplication_pressure"`
	TemporalVariance     float64 `json:"temporal_variance"`
}

// VaultObservability holds per-vault observability data.
type VaultObservability struct {
	EngramCount int64                    `json:"engram_count"`
	HNSWVectors int                      `json:"hnsw_vectors"`
	HNSWBytes   int64                    `json:"hnsw_bytes"`
	Coherence   *CoherenceStats          `json:"coherence,omitempty"`
	Latency     map[string]latency.Stats `json:"latency"`
}

// Observability assembles a full system snapshot for the observability endpoint.
func (e *Engine) Observability(ctx context.Context, version string, uptimeSeconds int64) (*ObservabilitySnapshot, error) {
	// 1. System stats
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	sys := SystemStats{
		UptimeSeconds:   uptimeSeconds,
		Version:         version,
		GoRoutines:      runtime.NumGoroutine(),
		GoMemAllocBytes: mem.Alloc,
		GoMemSysBytes:   mem.Sys,
		GoGCPauseNs:     mem.PauseTotalNs,
	}

	// 2. Storage stats
	diskBytes := e.store.DiskSize()
	pm := e.store.PebbleMetrics()

	var numSSTables int64
	for i := range pm.Levels {
		numSSTables += pm.Levels[i].NumFiles
	}

	var cacheHitRate float64
	totalCacheOps := pm.BlockCache.Hits + pm.BlockCache.Misses
	if totalCacheOps > 0 {
		cacheHitRate = float64(pm.BlockCache.Hits) / float64(totalCacheOps)
	}

	stor := StorageStats{
		DiskBytes: diskBytes,
		Pebble: PebbleStats{
			CompactionCount:     pm.Compact.Count,
			CompactionDebtBytes: pm.Compact.EstimatedDebt,
			CacheHits:           pm.BlockCache.Hits,
			CacheMisses:         pm.BlockCache.Misses,
			CacheHitRate:        cacheHitRate,
			ReadAmp:             pm.ReadAmp(),
			NumSSTables:         numSSTables,
			MemTableBytes:       pm.MemTable.Size,
			WALBytes:            pm.WAL.Size,
		},
	}

	// 3. Processor stats
	rawStats := e.GetProcessorStats()
	processors := make([]ProcessorStats, 0, len(rawStats))
	for i, rs := range rawStats {
		name := "enrich"
		if i == 0 || strings.Contains(strings.ToLower(rs.PluginName), "embed") {
			name = "embed"
		}
		processors = append(processors, ProcessorStats{
			Name:       name,
			PluginName: rs.PluginName,
			Status:     rs.Status,
			Processed:  rs.Processed,
			Total:      rs.Total,
			Pending:    rs.Total - rs.Processed,
			RatePerSec: rs.RatePerSec,
			ETASeconds: rs.ETASeconds,
			Errors:     rs.Errors,
		})
	}

	// 4. Cognitive worker stats
	ws := e.WorkerStats()
	workers := WorkerStatsSnapshot{
		Hebbian:       ws.Hebbian,
		Contradiction: ws.Contradict,
		Confidence:    ws.Confidence,
	}

	// 5. Per-vault stats
	vaultNames, err := e.store.ListVaultNames()
	if err != nil {
		return nil, err
	}

	// Build latency lookup: vault workspace -> op -> Stats
	var latencyByVault map[[8]byte]map[string]latency.Stats
	if e.latencyTracker != nil {
		latencyByVault = e.latencyTracker.Snapshot()
	}

	// Build coherence lookup: vault name -> Result
	coherenceByName := make(map[string]*CoherenceStats)
	if e.coherence != nil {
		for _, snap := range e.coherence.Snapshots() {
			coherenceByName[snap.VaultName] = &CoherenceStats{
				Score:                snap.Score,
				OrphanRatio:          snap.OrphanRatio,
				ContradictionDensity: snap.ContradictionDensity,
				DuplicationPressure:  snap.DuplicationPressure,
				TemporalVariance:     snap.TemporalVariance,
			}
		}
	}

	vaults := make(map[string]VaultObservability, len(vaultNames))
	for _, name := range vaultNames {
		wsPrefix := e.store.ResolveVaultPrefix(name)
		count := e.store.GetVaultCount(ctx, wsPrefix)

		var vectors int
		var vectorBytes int64
		if e.hnswRegistry != nil {
			vectors = e.hnswRegistry.VaultVectors(wsPrefix)
			vectorBytes = e.hnswRegistry.VaultVectorBytes(wsPrefix)
		}

		var vaultLatency map[string]latency.Stats
		if latencyByVault != nil {
			vaultLatency = latencyByVault[wsPrefix]
		}

		vaults[name] = VaultObservability{
			EngramCount: count,
			HNSWVectors: vectors,
			HNSWBytes:   vectorBytes,
			Coherence:   coherenceByName[name],
			Latency:     vaultLatency,
		}
	}

	// 6. LLM stats — only populated when at least one LLM plugin is active.
	var llmStats *LLMStats
	if e.enrichPlugin != nil {
		if p, ok := e.enrichPlugin.(llmstats.Provider); ok {
			snap := p.LLMStats()
			if llmStats == nil {
				llmStats = &LLMStats{}
			}
			llmStats.EnrichCalls = snap.Calls
			llmStats.EnrichErrors = snap.Errors
			llmStats.EnrichAvgMs = snap.AvgLatMs
		}
	}
	for _, rp := range e.retroProcessors {
		if rp == nil {
			continue
		}
		plug := rp.Plugin()
		if plug == nil || plug.Tier() != plugin.TierEmbed {
			continue
		}
		if p, ok := plug.(llmstats.Provider); ok {
			snap := p.LLMStats()
			if llmStats == nil {
				llmStats = &LLMStats{}
			}
			llmStats.EmbedCalls = snap.Calls
			llmStats.EmbedErrors = snap.Errors
			llmStats.EmbedAvgMs = snap.AvgLatMs
			break
		}
	}

	return &ObservabilitySnapshot{
		System:     sys,
		Storage:    stor,
		Processors: processors,
		Workers:    workers,
		Vaults:     vaults,
		LLM:        llmStats,
	}, nil
}
