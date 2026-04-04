package consolidation

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// runPhase2Dedup scans all engrams with embeddings and merges semantically similar ones
// (cosine similarity >= 0.95). The higher-confidence engram is kept as the representative,
// and the other is archived. In DryRun mode, no mutations occur.
func (w *Worker) runPhase2Dedup(ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte, report *ConsolidationReport, vault string) error {
	similarityThreshold := float32(0.95)
	if w.DedupThreshold > 0 {
		similarityThreshold = w.DedupThreshold
	}

	// Scan all engrams in the vault
	allIDs, err := scanAllEngramIDs(ctx, store, wsPrefix)
	if err != nil {
		return err
	}

	if len(allIDs) == 0 {
		return nil
	}

	// Fetch full engrams (expensive but necessary to access embeddings)
	allEngrams, err := store.GetEngrams(ctx, wsPrefix, allIDs)
	if err != nil {
		return err
	}

	// Build a list of engrams with embeddings only.
	// ERF v2 stores embeddings in a separate 0x18 key, so GetEngrams()
	// returns nil embeddings. Fall back to GetEmbedding() in that case.
	type engramWithEmbed struct {
		engram    *storage.Engram
		embedding []float32
	}
	withEmbed := make([]engramWithEmbed, 0)
	for _, eng := range allEngrams {
		if eng == nil {
			continue
		}
		embed := eng.Embedding
		if len(embed) == 0 {
			if loaded, err := store.GetEmbedding(ctx, wsPrefix, eng.ID); err == nil && len(loaded) > 0 {
				embed = loaded
			}
		}
		if len(embed) > 0 {
			withEmbed = append(withEmbed, engramWithEmbed{engram: eng, embedding: embed})
		}
	}

	if len(withEmbed) < 2 {
		slog.Debug("consolidation phase 2 (dedup): not enough engrams with embeddings", "count", len(withEmbed))
		return nil
	}

	// Find clusters of similar engrams (naive O(n²) pairwise comparison)
	type cluster struct {
		members []*storage.Engram // all members of the cluster
		minSim  float32           // lowest pairwise similarity to seed in cluster
	}
	visited := make(map[storage.ULID]bool)
	var clusters []cluster

	for i := 0; i < len(withEmbed); i++ {
		if visited[withEmbed[i].engram.ID] {
			continue
		}

		clust := cluster{members: []*storage.Engram{withEmbed[i].engram}, minSim: 1.0}
		visited[withEmbed[i].engram.ID] = true

		// Find all engrams similar to this one
		for j := i + 1; j < len(withEmbed); j++ {
			if visited[withEmbed[j].engram.ID] {
				continue
			}

			sim := cosineSimilarity(withEmbed[i].embedding, withEmbed[j].embedding)
			if sim >= similarityThreshold {
				clust.members = append(clust.members, withEmbed[j].engram)
				visited[withEmbed[j].engram.ID] = true
				if sim < clust.minSim {
					clust.minSim = sim
				}
			}
		}

		// Only record clusters with more than 1 member
		if len(clust.members) > 1 {
			clusters = append(clusters, clust)
		}
	}

	report.DedupClusters = len(clusters)

	// Split clusters: auto-merge only if ALL members have >= 0.95 similarity to seed.
	var autoMergeClusters []cluster
	for _, clust := range clusters {
		if clust.minSim >= 0.95 {
			autoMergeClusters = append(autoMergeClusters, clust)
		} else {
			// Collect for LLM review (Phase 2b)
			dc := DedupCluster{Members: make([]ClusterMember, 0, len(clust.members))}
			for _, m := range clust.members {
				dc.Members = append(dc.Members, ClusterMember{
					ID:      m.ID.String(),
					Concept: m.Concept,
					Content: m.Content,
				})
			}
			report.DedupClustersForLLM = append(report.DedupClustersForLLM, dc)
		}
	}

	// Process each auto-merge cluster: keep representative, archive others
	for _, clust := range autoMergeClusters {
		if len(clust.members) < 2 {
			continue
		}

		// Cap at MaxDedup pairs per run
		if report.MergedEngrams >= w.MaxDedup {
			slog.Debug("consolidation phase 2: reached max dedup limit", "limit", w.MaxDedup)
			break
		}

		// Elect representative: highest relevance * confidence
		var representative *storage.Engram
		maxScore := float64(-1)
		for _, member := range clust.members {
			score := float64(member.Relevance) * float64(member.Confidence)
			if score > maxScore {
				maxScore = score
				representative = member
			}
		}

		if representative == nil {
			continue
		}

		// Merge tags from all members into representative
		tagSet := make(map[string]bool)
		for _, tag := range representative.Tags {
			tagSet[tag] = true
		}
		for _, member := range clust.members {
			if member.ID == representative.ID {
				continue
			}
			for _, tag := range member.Tags {
				tagSet[tag] = true
			}
		}
		mergedTags := make([]string, 0, len(tagSet))
		for tag := range tagSet {
			mergedTags = append(mergedTags, tag)
		}

		// Archive non-representative members
		for _, member := range clust.members {
			if member.ID == representative.ID {
				continue
			}

			if !w.DryRun {
				// Archive the member (soft delete)
				if err := w.Engine.UpdateLifecycleState(ctx, vault, member.ID.String(), "archived"); err != nil {
					slog.Warn("consolidation phase 2: failed to archive engram", "id", member.ID, "error", err)
					continue
				}
			}

			report.MergedEngrams++
		}

		// Update representative with merged tags (best-effort)
		if !w.DryRun && len(mergedTags) != len(representative.Tags) {
			_ = store.UpdateMetadata(ctx, wsPrefix, representative.ID, &storage.EngramMeta{
				State:       representative.State,
				Confidence:  representative.Confidence,
				Relevance:   representative.Relevance,
				Stability:   representative.Stability,
				AccessCount: representative.AccessCount,
				UpdatedAt:   representative.UpdatedAt,
				LastAccess:  representative.LastAccess,
			})
			// Persist the merged tags — UpdateMetadata patches only fixed-size metadata
			// fields and does not touch the variable-length tag list.
			_ = store.UpdateTags(ctx, wsPrefix, representative.ID, mergedTags)
		}
	}

	slog.Debug("consolidation phase 2 (dedup) completed", "clusters", report.DedupClusters, "merged", report.MergedEngrams)
	return nil
}

// cosineSimilarity computes the cosine similarity between two float32 vectors
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dotProduct, magA, magB float64
	for i := range a {
		fa := float64(a[i])
		fb := float64(b[i])
		dotProduct += fa * fb
		magA += fa * fa
		magB += fb * fb
	}

	magA = math.Sqrt(magA)
	magB = math.Sqrt(magB)

	if magA == 0 || magB == 0 {
		return 0
	}

	return float32(dotProduct / (magA * magB))
}

// scanAllEngramIDs scans all engram IDs in a vault using pagination
func scanAllEngramIDs(ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte) ([]storage.ULID, error) {
	var allIDs []storage.ULID
	var offset int
	const pageSize = 500

	// Use EngramsByCreatedSince with epoch start time to get all engrams
	for {
		engrams, err := store.EngramsByCreatedSince(ctx, wsPrefix, time.Unix(0, 0), offset, pageSize)
		if err != nil {
			return nil, err
		}

		if len(engrams) == 0 {
			break // no more engrams
		}

		for _, eng := range engrams {
			if eng != nil {
				allIDs = append(allIDs, eng.ID)
			}
		}

		if len(engrams) < pageSize {
			break // last page
		}

		offset += pageSize
	}

	return allIDs, nil
}
