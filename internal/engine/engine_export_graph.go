package engine

import (
	"context"
	"sort"

	"github.com/scrypster/muninndb/internal/storage"
)

// GraphNode represents a named entity node in the exported graph.
type GraphNode struct {
	ID   string
	Type string
}

// GraphEdge represents a typed entity-to-entity relationship in the exported graph.
type GraphEdge struct {
	From    string
	To      string
	RelType string
	Weight  float32
}

// ExportGraph holds the full graph for a vault: nodes (entities) and edges (relationships).
type ExportGraph struct {
	Nodes []GraphNode
	Edges []GraphEdge
}

// ExportGraph builds the entity→relationship graph for vault.
// Nodes are derived from unique entity names found in relationship records.
// If includeEngrams is true the entity type is enriched from the entity record table.
// Edges are deduplicated by (From, To, RelType): only the highest-weight record per triple is kept.
func (e *Engine) ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*ExportGraph, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	// Deduplicate edges by (From, To, RelType): keep highest weight per triple.
	type edgeKey struct{ From, To, RelType string }
	edgeBest := make(map[edgeKey]GraphEdge)
	nodeSet := make(map[string]struct{})

	err := e.store.ScanRelationships(ctx, ws, func(rec storage.RelationshipRecord) error {
		k := edgeKey{From: rec.FromEntity, To: rec.ToEntity, RelType: rec.RelType}
		existing, seen := edgeBest[k]
		if !seen || rec.Weight > existing.Weight {
			edgeBest[k] = GraphEdge{
				From:    rec.FromEntity,
				To:      rec.ToEntity,
				RelType: rec.RelType,
				Weight:  rec.Weight,
			}
		}
		nodeSet[rec.FromEntity] = struct{}{}
		nodeSet[rec.ToEntity] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}

	edges := make([]GraphEdge, 0, len(edgeBest))
	for _, edge := range edgeBest {
		edges = append(edges, edge)
	}

	nodes := make([]GraphNode, 0, len(nodeSet))
	for name := range nodeSet {
		node := GraphNode{ID: name}
		if includeEngrams {
			if rec, recErr := e.store.GetEntityRecord(ctx, name); recErr == nil && rec != nil {
				node.Type = rec.Type
			}
		}
		nodes = append(nodes, node)
	}

	// Sort edges deterministically: by From, then To, then RelType
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].RelType < edges[j].RelType
	})

	// Sort nodes deterministically: by ID
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})

	return &ExportGraph{
		Nodes: nodes,
		Edges: edges,
	}, nil
}
