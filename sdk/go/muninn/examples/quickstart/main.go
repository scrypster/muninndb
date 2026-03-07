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

	// Health check
	ok, err := client.Health(ctx)
	if err != nil {
		log.Fatalf("health check failed: %v", err)
	}
	fmt.Printf("Server healthy: %v\n", ok)

	// Write memories
	authID, err := client.Write(ctx, "default", "auth architecture",
		"Short-lived JWTs (15min) with refresh tokens in HttpOnly cookies.", []string{"auth", "security"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}
	fmt.Printf("Stored auth memory: %s\n", authID)

	deployID, err := client.Write(ctx, "default", "deployment process",
		"Blue-green deployments with Kubernetes rolling updates.", []string{"devops"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}
	fmt.Printf("Stored deploy memory: %s\n", deployID)

	// Activate — semantic recall
	result, err := client.Activate(ctx, "default", []string{"reviewing login flow for security"}, 5)
	if err != nil {
		log.Fatalf("activate failed: %v", err)
	}
	fmt.Printf("\nRecall found %d memories:\n", result.TotalFound)
	for _, item := range result.Activations {
		fmt.Printf("  [%.3f] %s\n", item.Score, item.Concept)
	}

	// Read a specific memory
	memory, err := client.Read(ctx, authID, "default")
	if err != nil {
		log.Fatalf("read failed: %v", err)
	}
	fmt.Printf("\nRead: %s (confidence: %.2f)\n", memory.Concept, memory.Confidence)

	// Link two memories
	err = client.Link(ctx, "default", authID, deployID, 5, 0.8)
	if err != nil {
		log.Fatalf("link failed: %v", err)
	}
	fmt.Printf("Linked %s → %s\n", authID, deployID)

	// Stats
	stats, err := client.Stats(ctx)
	if err != nil {
		log.Fatalf("stats failed: %v", err)
	}
	fmt.Printf("\nTotal engrams: %d, vaults: %d\n", stats.EngramCount, stats.VaultCount)

	// List vaults
	vaults, err := client.ListVaults(ctx)
	if err != nil {
		log.Fatalf("list vaults failed: %v", err)
	}
	fmt.Printf("Vaults: %v\n", vaults)
}
