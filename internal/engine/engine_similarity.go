package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// SimilarEntityPair represents a pair of entity names that are likely the same.
type SimilarEntityPair struct {
	EntityA    string
	EntityB    string
	Similarity float64
}

// MergeEntityResult is the result of a MergeEntity operation.
type MergeEntityResult struct {
	EntityA         string
	EntityB         string
	EngramsRelinked int
	DryRun          bool
}

// FindSimilarEntities scans all entity names in a vault and returns pairs whose
// trigram similarity is >= threshold. Results are capped at topN pairs sorted
// by similarity descending.
func (e *Engine) FindSimilarEntities(ctx context.Context, vault string, threshold float64, topN int) ([]SimilarEntityPair, error) {
	if threshold < 0 || threshold > 1 {
		return nil, fmt.Errorf("find_similar_entities: threshold must be in range [0.0, 1.0]")
	}
	if topN <= 0 {
		topN = 20
	}

	ws := e.store.ResolveVaultPrefix(vault)

	var names []string
	err := e.store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		names = append(names, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find_similar_entities: scan names: %w", err)
	}

	// Phase 1: build an inverted trigram index: trigram → []int (indices into names).
	// This reduces FindSimilarEntities from O(n²) to O(n × T × C) where T is the average
	// number of trigrams per name (~10) and C is the average candidate count per name
	// (small at high thresholds). For n=1000 this is ~10K–50K comparisons vs 500K.
	invertedIdx := make(map[string][]int, len(names)*8)
	for i, name := range names {
		for t := range trigrams(name) {
			invertedIdx[t] = append(invertedIdx[t], i)
		}
	}

	// Phase 2: for each name, find candidate indices sharing ≥1 trigram, then compare.
	// Using j > i avoids duplicate pairs and mirrors the previous double-loop semantics.
	// Note: trigramSim > 0 iff the trigram sets share at least one entry, so no pair with
	// sim ≥ threshold > 0 can be missed by the candidate set. At threshold=0 pairs of
	// entirely different strings are excluded, but that edge case is not meaningful in practice.
	var pairs []SimilarEntityPair
	for i, name := range names {
		candidates := make(map[int]struct{})
		for t := range trigrams(name) {
			for _, j := range invertedIdx[t] {
				if j > i {
					candidates[j] = struct{}{}
				}
			}
		}
		for j := range candidates {
			sim := trigramSim(name, names[j])
			if sim >= threshold {
				pairs = append(pairs, SimilarEntityPair{
					EntityA:    name,
					EntityB:    names[j],
					Similarity: sim,
				})
			}
		}
	}

	// Sort by similarity descending.
	sort.Slice(pairs, func(a, b int) bool {
		return pairs[a].Similarity > pairs[b].Similarity
	})

	// Cap at topN.
	if len(pairs) > topN {
		pairs = pairs[:topN]
	}

	return pairs, nil
}

// MergeEntity merges entity A into entity B (canonical).
//
// Steps:
//  1. Reads both entity records.
//  2. Sets A's state to "merged", A's MergedInto = B's name.
//  3. Scans all engrams in vault that link to A; writes a new entity link to B.
//  4. Updates B's confidence to the higher of the two; MentionCount is incremented
//     once (one merge event) via the natural UpsertEntityRecord path.
//
// When dryRun=true the function reports what would happen without writing anything.
func (e *Engine) MergeEntity(ctx context.Context, vault, entityA, entityB string, dryRun bool) (*MergeEntityResult, error) {
	if entityA == "" || entityB == "" {
		return nil, fmt.Errorf("merge_entity: entity_a and entity_b are required")
	}
	if entityA == entityB {
		return nil, fmt.Errorf("merge_entity: entity_a and entity_b must be different")
	}
	// Entity storage keys are hashed case-insensitively (lowercase + NFKC), so
	// names that differ only by case/whitespace normalize to the SAME entity.
	// Merging them would relink an engram from a key to itself, and the
	// underlying Set+Delete batch would destroy the link (#503). Reject up front.
	if keys.EntityNameHash(entityA) == keys.EntityNameHash(entityB) {
		return nil, fmt.Errorf("merge_entity: %q and %q normalize to the same entity (entity names are case-insensitive); nothing to merge", entityA, entityB)
	}

	// Serialise concurrent merges that touch either entity.
	// Acquired before any reads so the read→check→write sequence is atomic
	// with respect to other MergeEntity calls on the same entities.
	e.mergeMu.Lock(entityA, entityB)
	defer e.mergeMu.Unlock(entityA, entityB)

	ws := e.store.ResolveVaultPrefix(vault)

	recA, err := e.store.GetEntityRecord(ctx, entityA)
	if err != nil {
		return nil, fmt.Errorf("merge_entity: read entity_a: %w", err)
	}
	if recA == nil {
		return nil, fmt.Errorf("merge_entity: entity_a %q not found", entityA)
	}
	if recA.State == "merged" {
		return nil, fmt.Errorf("merge_entity: entity_a %q is already merged into %q", entityA, recA.MergedInto)
	}

	recB, err := e.store.GetEntityRecord(ctx, entityB)
	if err != nil {
		return nil, fmt.Errorf("merge_entity: read entity_b: %w", err)
	}
	if recB == nil {
		return nil, fmt.Errorf("merge_entity: entity_b %q not found", entityB)
	}

	// Collect all engrams in this vault that link to A.
	var engramIDs []storage.ULID
	scanErr := e.store.ScanEntityEngrams(ctx, entityA, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		engramIDs = append(engramIDs, id)
		return nil
	})
	if scanErr != nil {
		return nil, fmt.Errorf("merge_entity: scan engrams for entity_a: %w", scanErr)
	}

	result := &MergeEntityResult{
		EntityA:         entityA,
		EntityB:         entityB,
		EngramsRelinked: len(engramIDs),
		DryRun:          dryRun,
	}

	if dryRun {
		return result, nil
	}

	// Step 1: atomically relink each engram from A to B.
	// RelinkEntityEngramLink writes the new 0x20/0x23 links for B and deletes the
	// stale 0x20/0x23 links for A in a single Pebble batch, eliminating any crash
	// window where the engram would appear linked to both or neither entity.
	for _, id := range engramIDs {
		if err := e.store.RelinkEntityEngramLink(ctx, ws, id, entityA, entityB); err != nil {
			return nil, fmt.Errorf("merge_entity: relink engram %s from entity_a to entity_b: %w", id.String(), err)
		}
	}

	// Step 1b: update any 0x21 relationship records that reference entity A by name,
	// replacing A with B in both the record value and the 0x21 key (which encodes the
	// entity hash). The 0x26 index is updated in the same batches. This keeps
	// ScanEntityRelationships("B") returning the complete set after a merge.
	if err := e.store.RelinkRelationshipEntity(ctx, ws, entityA, entityB); err != nil {
		return nil, fmt.Errorf("merge_entity: relink relationship records from entity_a to entity_b: %w", err)
	}

	// Step 2: mark A as merged.
	if err := e.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:       recA.Name,
		Type:       recA.Type,
		Confidence: recA.Confidence,
		State:      "merged",
		MergedInto: entityB,
	}, "mcp:merge_entity"); err != nil {
		return nil, fmt.Errorf("merge_entity: update entity_a state: %w", err)
	}

	// Step 3: update B preserving the higher confidence.
	// UpsertEntityRecord naturally increments MentionCount by 1 (one merge event).
	newConf := recB.Confidence
	if recA.Confidence > newConf {
		newConf = recA.Confidence
	}
	if err := e.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:       recB.Name,
		Type:       recB.Type,
		Confidence: newConf,
		State:      recB.State,
		MergedInto: recB.MergedInto,
	}, "mcp:merge_entity"); err != nil {
		return nil, fmt.Errorf("merge_entity: update entity_b record: %w", err)
	}

	return result, nil
}

// trigrams returns the set of 3-character trigrams in a string (lowercased, trimmed).
// For strings shorter than 3 runes, the whole string is returned as a single entry.
func trigrams(s string) map[string]bool {
	s = strings.ToLower(strings.TrimSpace(s))
	runes := []rune(s)
	if len(runes) < 3 {
		return map[string]bool{s: true}
	}
	result := make(map[string]bool)
	for i := 0; i <= len(runes)-3; i++ {
		result[string(runes[i:i+3])] = true
	}
	return result
}

// trigramSim computes the Dice coefficient on character trigrams between two strings.
// Returns a value in [0.0, 1.0]; 1.0 means identical trigram sets.
func trigramSim(a, b string) float64 {
	trigramsA := trigrams(a)
	trigramsB := trigrams(b)
	if len(trigramsA) == 0 && len(trigramsB) == 0 {
		return 1.0
	}
	intersection := 0
	for t := range trigramsA {
		if trigramsB[t] {
			intersection++
		}
	}
	return float64(2*intersection) / float64(len(trigramsA)+len(trigramsB))
}
