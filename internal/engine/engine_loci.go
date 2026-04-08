package engine

import (
	"context"

	"github.com/scrypster/muninndb/internal/cognitive"
)

// DetectLoci discovers emergent communities in the entity co-occurrence graph
// using label propagation. Returns communities sorted by size descending.
func (e *Engine) DetectLoci(ctx context.Context, vault string, minEdgeWeight int) ([]cognitive.Locus, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	// Collect co-occurrence pairs from the 0x24 index.
	var pairs []cognitive.EntityPair
	err := e.store.ScanEntityClusters(ctx, ws, minEdgeWeight, func(nameA, nameB string, count int) error {
		pairs = append(pairs, cognitive.EntityPair{
			EntityA: nameA,
			EntityB: nameB,
			Count:   count,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(pairs) == 0 {
		return nil, nil
	}

	// Loci detection is on-demand (invoked via MCP tool), so HippocampalConfig.EnableLoci
	// is not checked here — the user explicitly requests detection. The EnableLoci flag
	// is reserved for a future background worker that maintains cached communities.
	cfg := cognitive.DefaultLociConfig()
	detector := cognitive.NewLociDetector(cfg)

	return detector.DetectCommunities(pairs), nil
}
