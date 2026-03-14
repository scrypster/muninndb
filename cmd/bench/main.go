package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"math/rand"
	"os"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/bench"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

func main() {
	// Parse command-line flags
	mode := flag.String("mode", "all", "Benchmark mode: retrieval, throughput, write-only, activate-only, latency, memory, all")
	concurrency := flag.Int("concurrency", 12, "Number of concurrent workers for throughput test")
	duration := flag.Duration("duration", 30*time.Second, "Duration for throughput test")
	soakDuration := flag.Duration("soak-duration", 60*time.Second, "Duration for memory soak test")
	cpuProfile := flag.String("cpuprofile", "", "Write CPU profile to file")
	memProfile := flag.String("memprofile", "", "Write memory profile to file")
	flag.Parse()

	// Reduce GC frequency for throughput benchmarks. The activation pipeline
	// allocates heavily (maps, slices per call) and default GOGC=100 causes
	// ~17% CPU in gcDrain. GOGC=400 trades memory for throughput.
	debug.SetGCPercent(400)

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatalf("create cpu profile: %v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatalf("start cpu profile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	// Create temporary directory for Pebble storage
	tmpDir, err := os.MkdirTemp("", "muninndb-bench-*")
	if err != nil {
		log.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initialize storage and engine
	db, err := storage.OpenPebble(tmpDir, storage.DefaultOptions())
	if err != nil {
		log.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100_000})
	ftsIdx := fts.New(db)

	// hashEmbedder: deterministic word-hash vectors — distinct per text, no external calls.
	// Enables accurate retrieval quality metrics while keeping throughput/latency unaffected.
	embedder := &hashEmbedder{}

	// Initialize activation engine (HNSW omitted — separate plugin pipeline in production).
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)

	// Initialize trigger system.
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	// Wire all cognitive workers so the benchmark reflects real activation side effects:
	// Hebbian weight updates, confidence accumulation, contradiction detection.
	hebbianWorker := cognitive.NewHebbianWorker(&benchHebbianAdapter{store})
	contradictWorker := cognitive.NewContradictWorker(&benchContradictAdapter{store})
	confidenceWorker := cognitive.NewConfidenceWorker(&benchConfidenceAdapter{store})

	// Start contradict, confidence workers (Hebbian starts its own goroutine internally).
	benchCtx, benchCancel := context.WithCancel(context.Background())
	go contradictWorker.Worker.Run(benchCtx)
	go confidenceWorker.Worker.Run(benchCtx)

	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		FTSIndex:         ftsIdx,
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		HebbianWorker:    hebbianWorker,
		ContradictWorker: contradictWorker.Worker,
		ConfidenceWorker: confidenceWorker.Worker,
		Embedder:         embedder,
	})
	defer func() {
		benchCancel()
		hebbianWorker.Stop()
		eng.Stop()
		store.Close()
	}()

	ctx := context.Background()
	vaultName := "benchmark"

	// Run requested benchmarks
	fmt.Println("MuninnDB Benchmark Report")
	fmt.Println("══════════════════════════════════════════════════")

	switch *mode {
	case "retrieval", "all":
		fmt.Println("\nRunning Retrieval Quality Benchmark...")
		result, err := benchmarkRetrieval(ctx, eng, vaultName)
		if err != nil {
			log.Printf("Retrieval benchmark failed: %v", err)
		} else {
			printRetrievalResult(result)
		}
		if *mode == "retrieval" {
			break
		}
		fallthrough

	case "throughput":
		if *mode == "throughput" {
			fmt.Println("\nRunning Throughput Benchmark...")
		} else {
			fmt.Println("\nRunning Throughput Benchmark...")
		}
		result, err := benchmarkThroughput(ctx, eng, vaultName, *concurrency, *duration)
		if err != nil {
			log.Printf("Throughput benchmark failed: %v", err)
		} else {
			printThroughputResult(result)
		}
		if *mode == "throughput" {
			break
		}
		fallthrough

	case "latency":
		if *mode == "latency" {
			fmt.Println("\nRunning Latency Benchmark...")
		} else {
			fmt.Println("\nRunning Latency Benchmark...")
		}
		result, err := benchmarkLatency(ctx, eng, vaultName)
		if err != nil {
			log.Printf("Latency benchmark failed: %v", err)
		} else {
			printLatencyResult(result)
		}
		if *mode == "latency" {
			break
		}
		fallthrough

	case "memory":
		if *mode == "memory" {
			fmt.Println("\nRunning Memory Soak Benchmark...")
		} else {
			fmt.Println("\nRunning Memory Soak Benchmark...")
		}
		result, err := benchmarkMemory(ctx, eng, vaultName, *soakDuration)
		if err != nil {
			log.Printf("Memory benchmark failed: %v", err)
		} else {
			printMemoryResult(result)
		}
		if *mode == "memory" {
			break
		}

	case "write-only":
		// Pure write benchmark: all goroutines are writers, no activators.
		// Shows peak single-path write throughput without read-write contention.
		fmt.Println("\nRunning Write-Only Throughput Benchmark...")
		result, err := benchmarkWriteOnly(ctx, eng, vaultName, *concurrency, *duration)
		if err != nil {
			log.Printf("Write-only benchmark failed: %v", err)
		} else {
			fmt.Printf("Write Throughput (write-only)  %8.0f ops/sec  (concurrency=%d)\n", result.WritesPerSec, result.Concurrency)
		}

	case "activate-only":
		// Pure activation benchmark: pre-seed corpus, then all goroutines activate.
		// Shows peak activation throughput without write-read contention.
		fmt.Println("\nRunning Activate-Only Throughput Benchmark...")
		result, err := benchmarkActivateOnly(ctx, eng, vaultName, *concurrency, *duration)
		if err != nil {
			log.Printf("Activate-only benchmark failed: %v", err)
		} else {
			fmt.Printf("Activate Throughput (activate-only) %8.0f ops/sec  (concurrency=%d)\n", result.ActivationsPerSec, result.Concurrency)
		}

	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s\n", *mode)
		fmt.Fprintf(os.Stderr, "Valid modes: retrieval, throughput, write-only, activate-only, latency, memory, all\n")
		os.Exit(1)
	}

	fmt.Println("══════════════════════════════════════════════════")

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			log.Fatalf("create mem profile: %v", err)
		}
		defer f.Close()
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Fatalf("write mem profile: %v", err)
		}
	}
}

// hashEmbedder produces deterministic, semantically-meaningful 384-dim unit vectors
// by summing per-word random vectors seeded from each word's FNV hash.
// Texts sharing words get similar vectors — no ML model required, no network calls,
// and retrieval quality metrics reflect real signal rather than zero-vector noise.
type hashEmbedder struct{}

func (e *hashEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	const dims = 384
	vec := make([]float64, dims)

	for _, text := range texts {
		for _, word := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New64a()
			h.Write([]byte(word))
			rng := rand.New(rand.NewSource(int64(h.Sum64()))) //nolint:gosec
			for i := range vec {
				vec[i] += rng.NormFloat64()
			}
		}
	}

	// L2-normalize to unit vector.
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)

	out := make([]float32, dims)
	if norm > 0 {
		for i, v := range vec {
			out[i] = float32(v / norm)
		}
	}
	return out, nil
}

func (e *hashEmbedder) Tokenize(text string) []string {
	return strings.Fields(strings.ToLower(text))
}

// ftsAdapter converts fts.ScoredID to activation.ScoredID.
type ftsAdapter struct{ idx *fts.Index }

func (a *ftsAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]activation.ScoredID, error) {
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

// ftsTrigAdapter converts fts.ScoredID to trigger.ScoredID.
type ftsTrigAdapter struct{ idx *fts.Index }

func (a *ftsTrigAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]trigger.ScoredID, error) {
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

// Print functions
func printRetrievalResult(r *bench.RetrievalResult) {
	fmt.Println("Retrieval Quality")
	fmt.Printf("  P@1=%.2f  P@5=%.2f  P@10=%.2f\n", r.PrecisionAt1, r.PrecisionAt5, r.PrecisionAt10)
	fmt.Printf("  NDCG@10=%.2f  MRR=%.2f  Recall@10=%.2f\n", r.NDCGAt10, r.MRR, r.RecallAt10)
	fmt.Printf("  Queries: %d (%v)\n", r.QueryCount, r.Duration)
}

func printThroughputResult(r *bench.ThroughputResult) {
	fmt.Println("Throughput")
	fmt.Printf("  Write Throughput    %.0f ops/sec  (concurrency=%d)\n", r.WritesPerSec, r.Concurrency)
	fmt.Printf("  Activate Throughput %.0f ops/sec\n", r.ActivationsPerSec)
}

func printLatencyResult(r *bench.LatencyResult) {
	fmt.Println("Latency (1000 activations)")
	fmt.Printf("  P50=%dms  P95=%dms  P99=%dms  P999=%dms  Max=%dms\n",
		r.P50.Milliseconds(),
		r.P95.Milliseconds(),
		r.P99.Milliseconds(),
		r.P999.Milliseconds(),
		r.Max.Milliseconds())
}

func printMemoryResult(r *bench.MemoryResult) {
	fmt.Println("Memory (soak test)")
	fmt.Printf("  Baseline=%.1f MB  Peak=%.1f MB  Growth=+%.1f MB  GC=%d\n",
		r.BaselineHeapMB,
		r.PeakHeapMB,
		r.GrowthMB,
		r.GCCount)
}
