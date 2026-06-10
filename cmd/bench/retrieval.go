package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/scrypster/muninndb/internal/bench"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// conceptDefinitions provides seed data for retrieval benchmarks.
// These are diverse neuroscience/cognitive psychology concepts.
var conceptDefinitions = []string{
	"memory consolidation",
	"neural plasticity",
	"sleep learning",
	"attention mechanism",
	"episodic recall",
	"semantic network",
	"working memory",
	"long-term potentiation",
	"synaptic pruning",
	"cognitive load",
	"retrieval practice",
	"spacing effect",
	"primacy effect",
	"recency effect",
	"interference theory",
	"context-dependent memory",
	"state-dependent memory",
	"encoding specificity",
	"depth of processing",
	"elaborative encoding",
}

// conceptContent maps concepts to descriptive content for embedding.
var conceptContent = map[string]string{
	"memory consolidation":     "The process of transferring information from short-term to long-term memory through biochemical changes in the brain.",
	"neural plasticity":        "The ability of the nervous system to change its activity in response to intrinsic or extrinsic stimuli.",
	"sleep learning":           "The phenomenon where memories formed during the day are consolidated and strengthened during sleep.",
	"attention mechanism":      "The selective focusing of conscious awareness on particular aspects of the environment.",
	"episodic recall":          "The retrieval of specific autobiographical events or experiences with temporal and spatial context.",
	"semantic network":         "An organized representation of concepts and their relationships in memory.",
	"working memory":           "The temporary storage and manipulation of information required for ongoing cognitive tasks.",
	"long-term potentiation":   "A persistent strengthening of synapses based on recent patterns of activity.",
	"synaptic pruning":         "The selective elimination of unused synaptic connections during development.",
	"cognitive load":           "The amount of working memory resources required to perform a task.",
	"retrieval practice":       "The act of retrieving information from memory, which strengthens the memory trace.",
	"spacing effect":           "Improved retention through spaced repetition compared to massed practice.",
	"primacy effect":           "The tendency to recall items early in a sequence better than later items.",
	"recency effect":           "The tendency to recall items late in a sequence better than earlier items.",
	"interference theory":      "The theory that forgetting occurs due to interference from similar memories.",
	"context-dependent memory": "Improved recall when environmental context matches encoding context.",
	"state-dependent memory":   "Improved recall when internal physiological state matches encoding state.",
	"encoding specificity":     "Memory performance is improved when information available at retrieval matches encoding context.",
	"depth of processing":      "Memory retention improves with deeper, more meaningful processing of information.",
	"elaborative encoding":     "Memory strategy involving connecting new information to existing knowledge structures.",
}

// QueryGroundTruth defines a query and its known top-5 most relevant concepts.
type QueryGroundTruth struct {
	Query   string
	TopFive []int // indices into conceptDefinitions
}

// queryGroundTruths maps natural language queries to their ground-truth relevant concepts.
var queryGroundTruths = []QueryGroundTruth{
	{
		Query:   "How do we retain information after learning?",
		TopFive: []int{0, 10, 11, 7, 18}, // consolidation, retrieval practice, spacing, LTP, depth
	},
	{
		Query:   "What happens to memories during sleep?",
		TopFive: []int{0, 2, 7, 11, 10}, // consolidation, sleep, LTP, spacing, retrieval
	},
	{
		Query:   "Why is it hard to focus on multiple things?",
		TopFive: []int{3, 9, 1, 6, 18}, // attention, cognitive load, plasticity, working mem, depth
	},
	{
		Query:   "How are experiences stored in the brain?",
		TopFive: []int{4, 5, 0, 1, 7}, // episodic, semantic, consolidation, plasticity, LTP
	},
	{
		Query:   "What causes forgetting over time?",
		TopFive: []int{14, 15, 16, 8, 9}, // interference, context-dep, state-dep, pruning, load
	},
	{
		Query:   "Why do we remember the first and last items better?",
		TopFive: []int{12, 13, 11, 10, 18}, // primacy, recency, spacing, retrieval, depth
	},
	{
		Query:   "How does the brain form new connections?",
		TopFive: []int{1, 7, 8, 0, 19}, // plasticity, LTP, pruning, consolidation, elaborative
	},
	{
		Query:   "What makes memories stronger?",
		TopFive: []int{7, 10, 11, 0, 19}, // LTP, retrieval, spacing, consolidation, elaborative
	},
	{
		Query:   "How does environment affect what we remember?",
		TopFive: []int{15, 16, 17, 4, 14}, // context-dep, state-dep, encoding spec, episodic, interference
	},
	{
		Query:   "Why do we focus attention on important things?",
		TopFive: []int{3, 9, 6, 18, 17}, // attention, cognitive load, working mem, depth, encoding
	},
	{
		Query:   "What is the link between memory concepts?",
		TopFive: []int{5, 0, 7, 19, 18}, // semantic network, consolidation, LTP, elaborative, depth
	},
	{
		Query:   "How do brains change and adapt over time?",
		TopFive: []int{1, 7, 8, 0, 2}, // plasticity, LTP, pruning, consolidation, sleep
	},
	{
		Query:   "What processes underlie learning?",
		TopFive: []int{0, 19, 18, 10, 7}, // consolidation, elaborative, depth, retrieval, LTP
	},
	{
		Query:   "How is temporary information held in mind?",
		TopFive: []int{6, 9, 3, 18, 17}, // working mem, cognitive load, attention, depth, encoding
	},
	{
		Query:   "Why do repeated reviews help memory?",
		TopFive: []int{10, 11, 0, 7, 19}, // retrieval, spacing, consolidation, LTP, elaborative
	},
	{
		Query:   "How do experiences become knowledge?",
		TopFive: []int{4, 5, 0, 19, 7}, // episodic, semantic, consolidation, elaborative, LTP
	},
	{
		Query:   "What happens to unused neural pathways?",
		TopFive: []int{8, 1, 7, 0, 2}, // pruning, plasticity, LTP, consolidation, sleep
	},
	{
		Query:   "How does the order of items affect recall?",
		TopFive: []int{12, 13, 11, 14, 15}, // primacy, recency, spacing, interference, context-dep
	},
	{
		Query:   "How can we improve memory encoding?",
		TopFive: []int{19, 18, 0, 10, 11}, // elaborative, depth, consolidation, retrieval, spacing
	},
	{
		Query:   "What prevents us from remembering everything?",
		TopFive: []int{14, 9, 8, 16, 15}, // interference, cognitive load, pruning, state-dep, context-dep
	},
}

// benchmarkRetrieval runs a retrieval quality benchmark.
func benchmarkRetrieval(ctx context.Context, eng *engine.Engine, vaultName string) (*bench.RetrievalResult, error) {
	startTime := time.Now()

	// Seed the vault with all concepts
	conceptIDMap := make(map[int]storage.ULID)
	for i, concept := range conceptDefinitions {
		req := &mbp.WriteRequest{
			Concept:    concept,
			Content:    conceptContent[concept],
			Tags:       []string{"neuroscience", "cognitive"},
			Confidence: 0.95,
			Stability:  1.0,
			Vault:      vaultName,
		}

		resp, err := eng.Write(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("write concept %d: %w", i, err)
		}

		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			return nil, err
		}
		conceptIDMap[i] = id
	}

	// Run queries and compute metrics
	var (
		sumP1      float64
		sumP5      float64
		sumP10     float64
		sumNDCG    float64
		sumMRR     float64
		sumRecall  float64
		queryCount = 0
	)

	for _, gt := range queryGroundTruths {
		// Activate with query context
		req := &mbp.ActivateRequest{
			Context:    []string{gt.Query},
			MaxResults: 20,
			Vault:      vaultName,
		}

		resp, err := eng.Activate(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("activate: %w", err)
		}

		queryCount++

		// Build set of ground truth IDs
		gtSet := make(map[string]bool)
		for _, idx := range gt.TopFive {
			gtSet[conceptIDMap[idx].String()] = true
		}

		// Compute metrics for top-20 results
		var (
			hitsAt1  = 0
			hitsAt5  = 0
			hitsAt10 = 0
			hitsAt20 = 0
			firstHit = 0
			dcg      = 0.0
			idcg     = 0.0
		)

		for i := 0; i < 5; i++ {
			idcg += 1.0 / math.Log2(float64(i+2))
		}

		for i, item := range resp.Activations {
			if i >= 20 {
				break
			}

			isHit := gtSet[item.ID]
			if isHit {
				hitsAt20++
				if i < 10 {
					hitsAt10++
					if i < 5 {
						hitsAt5++
					}
				}
				if i < 1 {
					hitsAt1++
				}
				if firstHit == 0 {
					firstHit = i + 1
				}
				dcg += 1.0 / math.Log2(float64(i+2))
			}
		}

		// Compute P@k
		p1 := float64(hitsAt1)
		p5 := float64(hitsAt5) / 5.0
		p10 := float64(hitsAt10) / 10.0

		// Compute NDCG@10
		ndcg := 0.0
		if idcg > 0 {
			ndcg = dcg / idcg
		}

		// Compute MRR
		mrr := 0.0
		if firstHit > 0 {
			mrr = 1.0 / float64(firstHit)
		}

		// Compute Recall@10
		recall := float64(hitsAt10) / float64(len(gt.TopFive))

		sumP1 += p1
		sumP5 += p5
		sumP10 += p10
		sumNDCG += ndcg
		sumMRR += mrr
		sumRecall += recall
	}

	duration := time.Since(startTime)

	return &bench.RetrievalResult{
		PrecisionAt1:  sumP1 / float64(queryCount),
		PrecisionAt5:  sumP5 / float64(queryCount),
		PrecisionAt10: sumP10 / float64(queryCount),
		NDCGAt10:      sumNDCG / float64(queryCount),
		MRR:           sumMRR / float64(queryCount),
		RecallAt10:    sumRecall / float64(queryCount),
		QueryCount:    queryCount,
		Duration:      duration,
	}, nil
}

// computePercentile finds the k-th percentile of a sorted slice.
func computePercentile(sorted []time.Duration, percentile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * percentile / 100.0)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
