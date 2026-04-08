package cognitive

import (
	"math/rand"
	"sort"
)

// EntityPair represents a co-occurrence edge between two entities.
type EntityPair struct {
	EntityA string
	EntityB string
	Count   int
}

// Locus represents a detected community of co-occurring entities.
type Locus struct {
	ID       int      `json:"id"`
	Label    string   `json:"label"`
	Members  []string `json:"members"`
	Size     int      `json:"size"`
	Cohesion float64  `json:"cohesion"`
}

// LociDetector runs label propagation on entity co-occurrence graphs.
type LociDetector struct {
	Config LociConfig
}

// coEdge is an adjacency list entry: neighbor entity and edge weight.
type coEdge struct {
	neighbor string
	weight   int
}

// NewLociDetector creates a LociDetector with the given config.
func NewLociDetector(cfg LociConfig) *LociDetector {
	return &LociDetector{Config: cfg}
}

// DetectCommunities runs label propagation on entity co-occurrence pairs.
// Returns a list of communities (loci), each containing >=2 members, sorted
// by size descending.
func (d *LociDetector) DetectCommunities(pairs []EntityPair) []Locus {
	if len(pairs) == 0 {
		return nil
	}

	// Build adjacency list: entity -> [(neighbor, weight)]
	adj := make(map[string][]coEdge)
	entities := make(map[string]struct{})

	for _, p := range pairs {
		adj[p.EntityA] = append(adj[p.EntityA], coEdge{neighbor: p.EntityB, weight: p.Count})
		adj[p.EntityB] = append(adj[p.EntityB], coEdge{neighbor: p.EntityA, weight: p.Count})
		entities[p.EntityA] = struct{}{}
		entities[p.EntityB] = struct{}{}
	}

	// Step 1: Initialize — each entity gets its own label.
	label := make(map[string]string, len(entities))
	for e := range entities {
		label[e] = e
	}

	// Build sorted entity list for iteration.
	entityList := make([]string, 0, len(entities))
	for e := range entities {
		entityList = append(entityList, e)
	}
	sort.Strings(entityList) // deterministic base order

	// Step 2: Iterate label propagation.
	maxIter := d.Config.MaxIterations
	if maxIter <= 0 {
		maxIter = 100
	}

	for iter := 0; iter < maxIter; iter++ {
		// Shuffle for random order each iteration.
		rand.Shuffle(len(entityList), func(i, j int) {
			entityList[i], entityList[j] = entityList[j], entityList[i]
		})

		changed := false
		for _, entity := range entityList {
			neighbors := adj[entity]
			if len(neighbors) == 0 {
				continue
			}

			// Count weighted votes for each neighbor label.
			votes := make(map[string]int)
			for _, e := range neighbors {
				votes[label[e.neighbor]] += e.weight
			}

			// Find the most frequent label (weighted).
			bestLabel := label[entity]
			bestCount := 0
			// Sort candidate labels for deterministic tie-breaking.
			candidates := make([]string, 0, len(votes))
			for l := range votes {
				candidates = append(candidates, l)
			}
			sort.Strings(candidates)

			for _, l := range candidates {
				c := votes[l]
				if c > bestCount {
					bestCount = c
					bestLabel = l
				}
			}

			if bestLabel != label[entity] {
				label[entity] = bestLabel
				changed = true
			}
		}

		if !changed {
			break // converged
		}
	}

	// Step 3: Group entities by final label.
	groups := make(map[string][]string)
	for entity, l := range label {
		groups[l] = append(groups[l], entity)
	}

	// Step 4: Build loci, filtering out singletons (<2 members).
	var loci []Locus
	id := 0
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)

		// Generate label from top-3 entity names by mention count in edges.
		mentionCount := make(map[string]int)
		for _, m := range members {
			for _, e := range adj[m] {
				if label[e.neighbor] == label[m] {
					mentionCount[m] += e.weight
				}
			}
		}
		topLabel := generateLocusLabel(members, mentionCount)

		// Compute cohesion: sum(internal edge weights) / (size*(size-1)/2)
		cohesion := computeCohesion(members, adj)

		loci = append(loci, Locus{
			ID:       id,
			Label:    topLabel,
			Members:  members,
			Size:     len(members),
			Cohesion: cohesion,
		})
		id++
	}

	// Sort by size descending, then by label for stability.
	sort.Slice(loci, func(i, j int) bool {
		if loci[i].Size != loci[j].Size {
			return loci[i].Size > loci[j].Size
		}
		return loci[i].Label < loci[j].Label
	})

	// Re-assign sequential IDs after sorting.
	for i := range loci {
		loci[i].ID = i
	}

	return loci
}

// generateLocusLabel builds a label from the top-3 entities by mention count.
func generateLocusLabel(members []string, mentionCount map[string]int) string {
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(members))
	for _, m := range members {
		entries = append(entries, entry{name: m, count: mentionCount[m]})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name
	})

	n := 3
	if len(entries) < n {
		n = len(entries)
	}
	label := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			label += ", "
		}
		label += entries[i].name
	}
	return label
}

// computeCohesion calculates the ratio of actual internal edge weight to possible edges.
// cohesion = sum(internal_edge_weights) / (size * (size - 1) / 2)
// Returns 0.0 for communities with fewer than 2 members.
func computeCohesion(members []string, adj map[string][]coEdge) float64 {
	if len(members) < 2 {
		return 0.0
	}

	memberSet := make(map[string]struct{}, len(members))
	for _, m := range members {
		memberSet[m] = struct{}{}
	}

	totalWeight := 0
	// Count each edge once (undirected graph — each pair appears in both adjacency lists).
	seen := make(map[[2]string]struct{})
	for _, m := range members {
		for _, e := range adj[m] {
			if _, ok := memberSet[e.neighbor]; !ok {
				continue
			}
			pair := [2]string{m, e.neighbor}
			if m > e.neighbor {
				pair = [2]string{e.neighbor, m}
			}
			if _, ok := seen[pair]; ok {
				continue
			}
			seen[pair] = struct{}{}
			totalWeight += e.weight
		}
	}

	possibleEdges := len(members) * (len(members) - 1) / 2
	if possibleEdges == 0 {
		return 0.0
	}
	return float64(totalWeight) / float64(possibleEdges)
}
