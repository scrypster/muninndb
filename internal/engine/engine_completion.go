package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

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

// NarrativeContext provides structured context around a completed episode:
// what came before, what came after, how long it lasted, and its theme.
type NarrativeContext struct {
	Members  []CompletedEngram // episode members sorted by creation time
	BeforeID string            // engram ID immediately before the episode (empty if none)
	AfterID  string            // engram ID immediately after the episode (empty if none)
	Duration time.Duration     // time span of the episode
	TopicHint string           // most common concept words across members
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

// CompleteEpisodeWithContext returns the full episode like CompleteEpisode, but
// also provides narrative context: boundary engrams (before/after), duration,
// and a topic hint derived from word frequency across member concepts.
func (e *Engine) CompleteEpisodeWithContext(ctx context.Context, vault string, seedID string) (*NarrativeContext, error) {
	members, err := e.CompleteEpisode(ctx, vault, seedID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return &NarrativeContext{}, nil
	}

	wsPrefix := e.store.ResolveVaultPrefix(vault)

	first := members[0]
	last := members[len(members)-1]

	nc := &NarrativeContext{
		Members:  members,
		Duration: last.CreatedAt.Sub(first.CreatedAt),
	}

	// Find the engram immediately before the episode.
	// Scan a small window ending just before the first member's timestamp.
	if beforeIDs, err := e.store.EngramIDsByCreatedRange(
		ctx, wsPrefix,
		first.CreatedAt.Add(-30*time.Minute), // look back up to 30 minutes
		first.CreatedAt.Add(-time.Millisecond),
		50,
	); err == nil && len(beforeIDs) > 0 {
		// Take the last (most recent) ID that is not itself an episode member.
		memberSet := make(map[string]struct{}, len(members))
		for _, m := range members {
			memberSet[m.ID] = struct{}{}
		}
		for i := len(beforeIDs) - 1; i >= 0; i-- {
			idStr := beforeIDs[i].String()
			if _, isMember := memberSet[idStr]; !isMember {
				nc.BeforeID = idStr
				break
			}
		}
	}

	// Find the engram immediately after the episode.
	if afterIDs, err := e.store.EngramIDsByCreatedRange(
		ctx, wsPrefix,
		last.CreatedAt.Add(time.Millisecond),
		last.CreatedAt.Add(30*time.Minute), // look ahead up to 30 minutes
		50,
	); err == nil && len(afterIDs) > 0 {
		memberSet := make(map[string]struct{}, len(members))
		for _, m := range members {
			memberSet[m.ID] = struct{}{}
		}
		for _, id := range afterIDs {
			idStr := id.String()
			if _, isMember := memberSet[idStr]; !isMember {
				nc.AfterID = idStr
				break
			}
		}
	}

	// Generate TopicHint from word frequency across member concepts.
	nc.TopicHint = topicHintFromConcepts(members)

	return nc, nil
}

// topicHintFromConcepts extracts the 3 most common non-trivial words from
// the Concept fields of the episode members.
func topicHintFromConcepts(members []CompletedEngram) string {
	freq := make(map[string]int)
	for _, m := range members {
		for _, word := range conceptWords(m.Concept) {
			freq[word]++
		}
	}
	if len(freq) == 0 {
		return ""
	}

	type wc struct {
		word  string
		count int
	}
	var pairs []wc
	for w, c := range freq {
		pairs = append(pairs, wc{w, c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].word < pairs[j].word // stable tie-break
	})

	n := 3
	if len(pairs) < n {
		n = len(pairs)
	}
	words := make([]string, n)
	for i := 0; i < n; i++ {
		words[i] = pairs[i].word
	}
	return strings.Join(words, ", ")
}

// stopWords is a small set of common English words filtered from topic hints.
var stopWords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {},
	"in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "of": {},
	"is": {}, "it": {}, "by": {}, "with": {}, "as": {}, "from": {},
	"that": {}, "this": {}, "was": {}, "are": {}, "be": {}, "has": {},
	"had": {}, "not": {}, "no": {}, "do": {}, "if": {}, "so": {},
}

// conceptWords splits a concept string into lowercase words, filtering out
// stop words and very short tokens.
func conceptWords(concept string) []string {
	var words []string
	for _, raw := range strings.FieldsFunc(concept, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		w := strings.ToLower(raw)
		if len(w) < 3 {
			continue
		}
		if _, stop := stopWords[w]; stop {
			continue
		}
		words = append(words, w)
	}
	return words
}
