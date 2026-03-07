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

	fmt.Println("=== MuninnDB Lifecycle & Recovery Demo ===")

	// --- Write seed memories ---
	fmt.Println("Writing seed memories...")
	origID, err := client.Write(ctx, vault, "auth design",
		"JWT tokens with 15-minute expiry and HttpOnly refresh cookies.",
		[]string{"auth", "security"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}
	fmt.Printf("  Original: %s\n", origID)

	deployID, err := client.Write(ctx, vault, "deploy strategy",
		"Blue-green deployments via Kubernetes with canary releases.",
		[]string{"devops"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}
	fmt.Printf("  Deploy:   %s\n", deployID)

	// --- Evolve ---
	fmt.Println("\nEvolving auth design...")
	evolved, err := client.Evolve(ctx, vault, origID,
		"Migrated to OAuth2 with PKCE flow for public clients.",
		"Security audit recommended PKCE for mobile apps")
	if err != nil {
		log.Fatalf("evolve failed: %v", err)
	}
	fmt.Printf("  Evolved to: %s\n", evolved.ID)

	// --- Set state ---
	fmt.Println("\nSetting state to 'active'...")
	stateResp, err := client.SetState(ctx, vault, evolved.ID, "active", "Confirmed after review")
	if err != nil {
		log.Fatalf("set state failed: %v", err)
	}
	fmt.Printf("  State: %s (updated: %v)\n", stateResp.State, stateResp.Updated)

	// --- Decide ---
	fmt.Println("\nRecording a decision...")
	decision, err := client.Decide(ctx, vault,
		"Adopt OAuth2 with PKCE for all public clients",
		"PKCE prevents authorization code interception attacks.",
		[]string{"Keep JWT-only flow", "Use device code flow"},
		[]string{evolved.ID})
	if err != nil {
		log.Fatalf("decide failed: %v", err)
	}
	fmt.Printf("  Decision: %s\n", decision.ID)

	// --- Consolidate ---
	fmt.Println("\nConsolidating related memories...")
	pool1, err := client.Write(ctx, vault, "DB pooling",
		"PgBouncer with 100 max connections.", []string{"database"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}
	pool2, err := client.Write(ctx, vault, "DB config",
		"Pool size 100, timeout 30s.", []string{"database"})
	if err != nil {
		log.Fatalf("write failed: %v", err)
	}

	consolidated, err := client.Consolidate(ctx, vault,
		[]string{pool1, pool2},
		"PgBouncer: 100 max connections, 30s timeout.")
	if err != nil {
		log.Fatalf("consolidate failed: %v", err)
	}
	fmt.Printf("  Consolidated %d into: %s\n", len(consolidated.Archived), consolidated.ID)
	if len(consolidated.Warnings) > 0 {
		for _, w := range consolidated.Warnings {
			fmt.Printf("  Warning: %s\n", w)
		}
	}

	// --- Forget and restore ---
	fmt.Println("\nSoft-deleting consolidated memory...")
	err = client.Forget(ctx, consolidated.ID, vault)
	if err != nil {
		log.Fatalf("forget failed: %v", err)
	}
	fmt.Printf("  Deleted: %s\n", consolidated.ID)

	fmt.Println("\nListing deleted engrams...")
	deleted, err := client.ListDeleted(ctx, vault, 10)
	if err != nil {
		log.Fatalf("list deleted failed: %v", err)
	}
	fmt.Printf("  Found %d deleted engrams:\n", deleted.Count)
	for _, d := range deleted.Deleted {
		fmt.Printf("    %s — %s\n", d.ID[:8], d.Concept)
	}

	fmt.Println("\nRestoring deleted memory...")
	restored, err := client.Restore(ctx, consolidated.ID, vault)
	if err != nil {
		log.Fatalf("restore failed: %v", err)
	}
	fmt.Printf("  Restored: %s (concept: %s, state: %s)\n",
		restored.ID, restored.Concept, restored.State)

	// --- Contradictions ---
	fmt.Println("\nChecking for contradictions...")
	contradictions, err := client.Contradictions(ctx, vault)
	if err != nil {
		log.Fatalf("contradictions failed: %v", err)
	}
	fmt.Printf("  Found %d contradictions\n", len(contradictions.Contradictions))
	for _, c := range contradictions.Contradictions {
		fmt.Printf("    %s ↔ %s\n", c.ConceptA, c.ConceptB)
	}

	// --- Guide ---
	fmt.Println("\nGetting vault guide...")
	guide, err := client.Guide(ctx, vault)
	if err != nil {
		log.Fatalf("guide failed: %v", err)
	}
	if len(guide) > 120 {
		fmt.Printf("  Guide: %s...\n", guide[:120])
	} else {
		fmt.Printf("  Guide: %s\n", guide)
	}

	// --- Retry enrich ---
	fmt.Println("\nRetrying enrichment...")
	enrich, err := client.RetryEnrich(ctx, evolved.ID, vault)
	if err != nil {
		fmt.Printf("  Re-enrich (expected if no plugin): %v\n", err)
	} else {
		fmt.Printf("  Engram: %s\n", enrich.EngramID)
		fmt.Printf("  Plugins queued: %v\n", enrich.PluginsQueued)
		fmt.Printf("  Already complete: %v\n", enrich.AlreadyComplete)
		if enrich.Note != "" {
			fmt.Printf("  Note: %s\n", enrich.Note)
		}
	}

	// --- List engrams ---
	fmt.Println("\nListing engrams...")
	engrams, err := client.ListEngrams(ctx, vault, 5, 0)
	if err != nil {
		log.Fatalf("list engrams failed: %v", err)
	}
	fmt.Printf("  Total: %d, showing %d:\n", engrams.Total, len(engrams.Engrams))
	for _, e := range engrams.Engrams {
		fmt.Printf("    %s — %s (confidence: %.2f)\n", e.ID[:8], e.Concept, e.Confidence)
	}

	// --- Session ---
	fmt.Println("\nSession activity...")
	session, err := client.Session(ctx, vault, "", 10, 0)
	if err != nil {
		log.Fatalf("session failed: %v", err)
	}
	fmt.Printf("  Total entries: %d, showing %d:\n", session.Total, len(session.Entries))
	for _, e := range session.Entries {
		fmt.Printf("    %s — %s\n", e.ID[:8], e.Concept)
	}

	fmt.Println("\n=== Done ===")
}
