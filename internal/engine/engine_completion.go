package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// CompletedEngram is a single member of a completed episode.
type CompletedEngram struct {
	ID        string
	Concept   string
	Content   string
	Summary   string
	State     string
	CreatedAt time.Time
}

// CompleteEpisode takes a seed engram ID and walks same_episode associations
// (both forward and reverse) to return all engrams in the episode, ordered by
// creation time. This implements CA3-style autoassociative pattern completion:
// a partial cue reconstructs the full episodic memory.
func (e *Engine) CompleteEpisode(ctx context.Context, vault string, seedID string) ([]CompletedEngram, error) {
	id, err := storage.ParseULID(seedID)
	if err != nil {
		return nil, fmt.Errorf("parse seed id: %w", err)
	}

	wsPrefix := e.store.ResolveVaultPrefix(vault)

	// BFS through same_episode links (both forward and reverse).
	visited := map[storage.ULID]struct{}{id: {}}
	queue := []storage.ULID{id}

	// Safety cap to prevent runaway traversal on pathological graphs.
	const maxEpisodeSize = 200

	for len(queue) > 0 && len(visited) < maxEpisodeSize {
		batch := queue
		queue = nil

		// Forward: get outgoing associations, filter for RelSameEpisode.
		assocMap, err := e.store.GetAssociations(ctx, wsPrefix, batch, 50)
		if err != nil {
			return nil, fmt.Errorf("get forward associations: %w", err)
		}
		for _, src := range batch {
			for _, assoc := range assocMap[src] {
				if assoc.RelType != storage.RelSameEpisode {
					continue
				}
				if _, seen := visited[assoc.TargetID]; seen {
					continue
				}
				visited[assoc.TargetID] = struct{}{}
				queue = append(queue, assoc.TargetID)
			}
		}

		// Reverse: find engrams that have same_episode links TO each node.
		for _, nodeID := range batch {
			sources, revErr := e.store.GetReverseAssociationsByType(ctx, wsPrefix, nodeID, storage.RelSameEpisode)
			if revErr != nil {
				continue
			}
			for _, srcID := range sources {
				if _, seen := visited[srcID]; seen {
					continue
				}
				visited[srcID] = struct{}{}
				queue = append(queue, srcID)
			}
		}
	}

	// Collect all episode member IDs.
	memberIDs := make([]storage.ULID, 0, len(visited))
	for id := range visited {
		memberIDs = append(memberIDs, id)
	}

	// Batch-read all engrams.
	engrams, err := e.store.GetEngrams(ctx, wsPrefix, memberIDs)
	if err != nil {
		return nil, fmt.Errorf("read episode members: %w", err)
	}

	var result []CompletedEngram
	for _, eng := range engrams {
		if eng == nil {
			continue
		}
		if eng.State == storage.StateSoftDeleted {
			continue
		}
		result = append(result, CompletedEngram{
			ID:        eng.ID.String(),
			Concept:   eng.Concept,
			Content:   eng.Content,
			Summary:   eng.Summary,
			State:     eng.State.String(),
			CreatedAt: eng.CreatedAt,
		})
	}

	// Sort by creation time ascending to present the episode chronologically.
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})

	return result, nil
}
