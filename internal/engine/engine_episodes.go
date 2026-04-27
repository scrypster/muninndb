package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// Episode represents a group of engrams connected by same_episode associations.
type Episode struct {
	ID        string    // ID of the first engram in the episode
	StartTime time.Time
	EndTime   time.Time
	Size      int      // number of engrams
	Members   []string // engram IDs (ULIDs as strings)
}

// ListEpisodes returns groups of engrams connected by same_episode associations.
// It scans all forward associations in the vault, filters for RelSameEpisode,
// builds connected components via union-find, and returns episodes sorted by
// StartTime descending, limited to `limit`.
//
// Note: singleton episodes (single engrams with no same_episode associations)
// are not included because they have no edges to discover. This is by design —
// the EpisodeWorker only creates same_episode links between consecutive engrams
// within an episode. A singleton means a boundary was detected immediately.
func (e *Engine) ListEpisodes(ctx context.Context, vault string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	ws := e.store.ResolveVaultPrefix(vault)

	// Phase 1: Scan all forward associations and collect same_episode edges.
	type edge struct{ src, dst storage.ULID }
	var edges []edge
	allIDs := map[storage.ULID]struct{}{}

	err := e.store.ScanAssociationsByType(ctx, ws, storage.RelSameEpisode, func(src, dst storage.ULID) error {
		edges = append(edges, edge{src, dst})
		allIDs[src] = struct{}{}
		allIDs[dst] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list episodes: scan associations: %w", err)
	}
	if len(edges) == 0 {
		return []Episode{}, nil
	}

	// Phase 2: Union-find to build connected components.
	parent := make(map[storage.ULID]storage.ULID, len(allIDs))
	for id := range allIDs {
		parent[id] = id
	}
	var find func(storage.ULID) storage.ULID
	find = func(x storage.ULID) storage.ULID {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b storage.ULID) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}
	for _, e := range edges {
		union(e.src, e.dst)
	}

	// Phase 3: Group by connected component root.
	groups := map[storage.ULID][]storage.ULID{}
	for id := range allIDs {
		root := find(id)
		groups[root] = append(groups[root], id)
	}

	// Phase 4: For each group, read engrams to get timestamps.
	var episodes []Episode
	for _, members := range groups {
		// Fetch engrams in bulk.
		engrams, err := e.store.GetEngrams(ctx, ws, members)
		if err != nil {
			continue // skip groups with read errors
		}

		var earliest, latest time.Time
		var memberIDs []string
		for _, eng := range engrams {
			if eng == nil || eng.State == storage.StateSoftDeleted {
				continue
			}
			memberIDs = append(memberIDs, eng.ID.String())
			if earliest.IsZero() || eng.CreatedAt.Before(earliest) {
				earliest = eng.CreatedAt
			}
			if latest.IsZero() || eng.CreatedAt.After(latest) {
				latest = eng.CreatedAt
			}
		}
		if len(memberIDs) == 0 {
			continue
		}

		// Sort member IDs by ULID (lexicographic = chronological).
		sort.Strings(memberIDs)

		episodes = append(episodes, Episode{
			ID:        memberIDs[0], // first engram chronologically
			StartTime: earliest,
			EndTime:   latest,
			Size:      len(memberIDs),
			Members:   memberIDs,
		})
	}

	// Sort episodes by StartTime descending (most recent first).
	sort.Slice(episodes, func(i, j int) bool {
		return episodes[i].StartTime.After(episodes[j].StartTime)
	})

	if len(episodes) > limit {
		episodes = episodes[:limit]
	}
	return episodes, nil
}

// GetEpisodeByMember walks same_episode associations from the given engram via BFS
// and returns the connected episode. This avoids the limit imposed by ListEpisodes,
// making it suitable for direct lookups by episode ID (the first engram's ULID).
func (e *Engine) GetEpisodeByMember(ctx context.Context, vault string, engramID storage.ULID) (*Episode, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	// BFS: collect all engram IDs connected via same_episode edges.
	visited := map[storage.ULID]struct{}{engramID: {}}
	queue := []storage.ULID{engramID}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		assocs, err := e.store.GetAssociations(ctx, ws, []storage.ULID{cur}, 0)
		if err != nil {
			return nil, fmt.Errorf("get episode by member: associations for %s: %w", cur, err)
		}
		for _, a := range assocs[cur] {
			if a.RelType != storage.RelSameEpisode {
				continue
			}
			if _, seen := visited[a.TargetID]; !seen {
				visited[a.TargetID] = struct{}{}
				queue = append(queue, a.TargetID)
			}
		}
	}

	if len(visited) == 0 {
		return nil, fmt.Errorf("get episode by member: no episode found for %s", engramID)
	}

	// Collect IDs and fetch engrams.
	ids := make([]storage.ULID, 0, len(visited))
	for id := range visited {
		ids = append(ids, id)
	}
	engrams, err := e.store.GetEngrams(ctx, ws, ids)
	if err != nil {
		return nil, fmt.Errorf("get episode by member: fetch engrams: %w", err)
	}

	var earliest, latest time.Time
	var memberIDs []string
	for _, eng := range engrams {
		if eng == nil || eng.State == storage.StateSoftDeleted {
			continue
		}
		memberIDs = append(memberIDs, eng.ID.String())
		if earliest.IsZero() || eng.CreatedAt.Before(earliest) {
			earliest = eng.CreatedAt
		}
		if latest.IsZero() || eng.CreatedAt.After(latest) {
			latest = eng.CreatedAt
		}
	}
	if len(memberIDs) == 0 {
		return nil, fmt.Errorf("get episode by member: all members deleted for %s", engramID)
	}

	sort.Strings(memberIDs)

	return &Episode{
		ID:        memberIDs[0],
		StartTime: earliest,
		EndTime:   latest,
		Size:      len(memberIDs),
		Members:   memberIDs,
	}, nil
}
