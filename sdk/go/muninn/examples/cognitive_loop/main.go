package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/scrypster/muninndb/sdk/go/muninn"
)

func main() {
	token := os.Getenv("MUNINN_TOKEN")
	client := muninn.NewClient("http://127.0.0.1:8476", token)
	ctx := context.Background()

	vault := "default"

	// --- Batch write ---
	fmt.Println("=== MuninnDB Cognitive Loop Demo ===")
	fmt.Println("Writing a batch of related memories...")

	batch, err := client.WriteBatch(ctx, vault, []muninn.WriteRequest{
		{
			Concept:    "API gateway",
			Content:    "Kong gateway handles rate-limiting, auth, and request routing for all microservices.",
			Tags:       []string{"infra", "api"},
			Confidence: 0.9,
			Stability:  0.5,
		},
		{
			Concept:    "caching strategy",
			Content:    "Redis for hot data with 5min TTL. CDN caching for static assets. Write-through for user sessions.",
			Tags:       []string{"infra", "performance"},
			Confidence: 0.9,
			Stability:  0.5,
		},
		{
			Concept:    "observability stack",
			Content:    "OpenTelemetry for traces, Prometheus for metrics, Loki for logs. Grafana dashboards.",
			Tags:       []string{"infra", "monitoring"},
			Confidence: 0.9,
			Stability:  0.5,
		},
		{
			Concept:    "database sharding",
			Content:    "Shard user data by tenant ID. Read replicas for analytics queries. PgBouncer for connection pooling.",
			Tags:       []string{"database", "scaling"},
			Confidence: 0.9,
			Stability:  0.5,
		},
	})
	if err != nil {
		log.Fatalf("batch write failed: %v", err)
	}

	var ids []string
	for _, r := range batch.Results {
		fmt.Printf("  [%s] %s\n", r.Status, r.ID)
		if r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	fmt.Printf("Batch wrote %d engrams\n\n", len(ids))

	// --- Link related memories ---
	fmt.Println("Creating associations...")
	links := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}}
	for _, pair := range links {
		if pair[0] < len(ids) && pair[1] < len(ids) {
			err := client.Link(ctx, vault, ids[pair[0]], ids[pair[1]], 5, 0.85)
			if err != nil {
				log.Fatalf("link failed: %v", err)
			}
			fmt.Printf("  %s → %s\n", ids[pair[0]][:8], ids[pair[1]][:8])
		}
	}

	// --- Activate with options ---
	fmt.Println("\nActivating with extended options...")
	result, err := client.ActivateWithOptions(ctx, muninn.ActivateRequest{
		Vault:      vault,
		Context:    []string{"how does the system handle high traffic and performance"},
		MaxResults: 10,
		Threshold:  0.05,
		MaxHops:    2,
		IncludeWhy: true,
	})
	if err != nil {
		log.Fatalf("activate failed: %v", err)
	}

	fmt.Printf("Found %d memories (latency: %.1fms):\n", result.TotalFound, result.LatencyMs)
	for _, item := range result.Activations {
		fmt.Printf("  [%.3f] %s\n", item.Score, item.Concept)
		if item.Why != nil {
			fmt.Printf("         why: %s\n", *item.Why)
		}
		if len(item.HopPath) > 0 {
			fmt.Printf("         hop path: %v\n", item.HopPath)
		}
	}

	// --- Traverse the graph ---
	if len(ids) > 0 {
		fmt.Printf("\nTraversing graph from %s...\n", ids[0][:8])
		graph, err := client.Traverse(ctx, vault, ids[0], 2, 20, nil)
		if err != nil {
			log.Fatalf("traverse failed: %v", err)
		}

		fmt.Printf("Graph: %d nodes, %d edges (total reachable: %d)\n",
			len(graph.Nodes), len(graph.Edges), graph.TotalReachable)
		for _, node := range graph.Nodes {
			fmt.Printf("  [hop %d] %s (%s)\n", node.HopDist, node.Concept, node.ID[:8])
		}
		for _, edge := range graph.Edges {
			fmt.Printf("  edge: %s → %s (type: %s, weight: %.2f)\n",
				edge.FromID[:8], edge.ToID[:8], edge.RelType, edge.Weight)
		}
	}

	// --- Explain a score ---
	if len(ids) > 0 {
		fmt.Printf("\nExplaining score for %s...\n", ids[0][:8])
		explanation, err := client.Explain(ctx, vault, ids[0],
			[]string{"API gateway", "microservices"})
		if err != nil {
			log.Fatalf("explain failed: %v", err)
		}

		fmt.Printf("  Concept: %s\n", explanation.Concept)
		fmt.Printf("  Final score: %.4f (would return: %v, threshold: %.2f)\n",
			explanation.FinalScore, explanation.WouldReturn, explanation.Threshold)
		fmt.Printf("  Components:\n")
		fmt.Printf("    Full-text relevance:  %.4f\n", explanation.Components.FullTextRelevance)
		fmt.Printf("    Semantic similarity:  %.4f\n", explanation.Components.SemanticSimilarity)
		fmt.Printf("    Decay factor:         %.4f\n", explanation.Components.DecayFactor)
		fmt.Printf("    Hebbian boost:        %.4f\n", explanation.Components.HebbianBoost)
		fmt.Printf("    Access frequency:     %.4f\n", explanation.Components.AccessFrequency)
		fmt.Printf("    Confidence:           %.4f\n", explanation.Components.Confidence)
	}

	// --- Get links ---
	if len(ids) > 0 {
		fmt.Printf("\nLinks for %s:\n", ids[0][:8])
		associations, err := client.GetLinks(ctx, ids[0], vault)
		if err != nil {
			log.Fatalf("get links failed: %v", err)
		}
		for _, a := range associations {
			fmt.Printf("  → %s (rel_type: %d, weight: %.2f)\n",
				a.TargetID[:8], a.RelType, a.Weight)
		}
	}

	fmt.Println("\n=== Done ===")
}
