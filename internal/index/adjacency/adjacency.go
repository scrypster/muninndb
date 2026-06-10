package adjacency

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const (
	hopPenalty      = 0.7
	minHopScore     = 0.05
	maxBFSNodes     = 500
	maxEdgesPerNode = 20
)

// AssocEntry is a parsed association from the graph.
type AssocEntry struct {
	TargetID   [16]byte
	Weight     float32
	Confidence float32
	RelType    uint16
}

// TraversalResult is a node found via BFS traversal.
type TraversalResult struct {
	ID       [16]byte
	Score    float64
	HopPath  [][16]byte
	RelType  uint16
	HopDepth int
}

// Graph provides BFS traversal over the association adjacency store.
type Graph struct {
	db *pebble.DB
}

func New(db *pebble.DB) *Graph {
	return &Graph{db: db}
}

// GetAssociations returns forward associations for a given engram, sorted by weight descending.
// Reads from the 0x03 prefix key space which is already weight-complement sorted.
func (g *Graph) GetAssociations(ctx context.Context, ws [8]byte, id [16]byte, maxPerNode int) ([]AssocEntry, error) {
	// Build prefix: 0x03 | ws(8) | id(16)
	prefix := make([]byte, 1+8+16)
	prefix[0] = 0x03
	copy(prefix[1:9], ws[:])
	copy(prefix[9:25], id[:])

	// prefixSuccessor handles 0xFF overflow correctly; nil means no upper bound.
	iter, err := g.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixSuccessor(prefix),
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var results []AssocEntry
	for iter.First(); iter.Valid() && len(results) < maxPerNode; iter.Next() {
		key := iter.Key()
		// Key: 0x03 | ws(8) | src(16) | weight_complement(4) | dst(16) = 45 bytes
		if len(key) < 45 {
			continue
		}

		weightComp := binary.BigEndian.Uint32(key[25:29])
		weight := float32(math.MaxUint32-weightComp) / float32(math.MaxUint32)

		var dstID [16]byte
		copy(dstID[:], key[29:45])

		val := iter.Value()
		var relType uint16
		var confidence float32
		if len(val) >= 6 {
			relType = binary.BigEndian.Uint16(val[0:2])
			confidence = math.Float32frombits(binary.BigEndian.Uint32(val[2:6]))
		}

		// Early termination: since keys are weight-DESC, once weight drops to near-zero stop
		if weight < 0.001 {
			break
		}

		results = append(results, AssocEntry{
			TargetID:   dstID,
			Weight:     weight,
			Confidence: confidence,
			RelType:    relType,
		})
	}
	return results, nil
}

// WriteAssociation writes forward and reverse association keys.
func (g *Graph) WriteAssociation(ws [8]byte, src [16]byte, assoc *storage.Association) error {
	dst := [16]byte(assoc.TargetID)

	// Prevent self-edges
	if src == dst {
		return fmt.Errorf("adjacency: self-edges not allowed")
	}

	batch := g.db.NewBatch()

	// Forward key: 0x03 | ws | src | weight_complement | dst
	fwdKey := keys.AssocFwdKey(ws, src, assoc.Weight, dst)
	// Reverse key: 0x04 | ws | dst | weight_complement | src
	revKey := keys.AssocRevKey(ws, dst, assoc.Weight, src)

	// Value: rel_type(2) + confidence(4) + created_at(8) + last_activated(4) = 18 bytes
	val := make([]byte, 18)
	binary.BigEndian.PutUint16(val[0:2], uint16(assoc.RelType))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(assoc.Confidence))
	binary.BigEndian.PutUint64(val[6:14], uint64(assoc.CreatedAt.UnixNano()))
	binary.BigEndian.PutUint32(val[14:18], uint32(assoc.LastActivated))

	batch.Set(fwdKey, val, nil)
	batch.Set(revKey, val, nil)

	return batch.Commit(pebble.NoSync)
}

// Traverse performs BFS from seed IDs, returning discovered engrams.
func (g *Graph) Traverse(
	ctx context.Context,
	ws [8]byte,
	seeds [][16]byte,
	threshold float64,
	maxDepth int,
) ([]TraversalResult, error) {

	type queueItem struct {
		id        [16]byte
		baseScore float64
		depth     int
		hopPath   [][16]byte
	}

	seen := make(map[[16]byte]bool, len(seeds)+maxBFSNodes)
	for _, s := range seeds {
		seen[s] = true
	}

	queue := make([]queueItem, 0, len(seeds)*4)
	for _, s := range seeds {
		queue = append(queue, queueItem{
			id:        s,
			baseScore: 1.0,
			depth:     0,
			hopPath:   [][16]byte{s},
		})
	}

	var results []TraversalResult
	expanded := 0

	for len(queue) > 0 && expanded < maxBFSNodes {
		curr := queue[0]
		queue = queue[1:]

		if curr.depth >= maxDepth {
			continue
		}

		assocs, err := g.GetAssociations(ctx, ws, curr.id, maxEdgesPerNode)
		if err != nil {
			slog.Warn("adjacency traverse: failed to read edges", "node", curr.id, "err", err)
			continue
		}

		for _, assoc := range assocs {
			if seen[assoc.TargetID] {
				continue
			}

			propagated := curr.baseScore * float64(assoc.Weight) * math.Pow(hopPenalty, float64(curr.depth+1))
			if propagated < minHopScore {
				break // weight-sorted, can stop early
			}

			seen[assoc.TargetID] = true
			expanded++

			newPath := make([][16]byte, len(curr.hopPath)+1)
			copy(newPath, curr.hopPath)
			newPath[len(curr.hopPath)] = assoc.TargetID

			results = append(results, TraversalResult{
				ID:       assoc.TargetID,
				Score:    propagated,
				HopPath:  newPath,
				RelType:  assoc.RelType,
				HopDepth: curr.depth + 1,
			})

			if curr.depth+1 < maxDepth {
				queue = append(queue, queueItem{
					id:        assoc.TargetID,
					baseScore: propagated,
					depth:     curr.depth + 1,
					hopPath:   newPath,
				})
			}
		}
	}

	return results, nil
}

// prefixSuccessor returns the smallest byte slice that is strictly greater than
// any key sharing the given prefix. It finds the rightmost byte that is not 0xFF,
// increments it, and truncates — correctly handling carry without overflow.
// Returns nil if every byte is 0xFF (no upper bound needed).
func prefixSuccessor(prefix []byte) []byte {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] < 0xFF {
			succ := make([]byte, i+1)
			copy(succ, prefix)
			succ[i]++
			return succ
		}
	}
	return nil // all 0xFF — no upper bound
}
