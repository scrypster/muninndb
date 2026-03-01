package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TreeNode is a single node in a recalled memory tree.
type TreeNode struct {
	ID           string
	Concept      string
	State        string
	Ordinal      int32
	LastAccessed string
	Children     []TreeNode
}

// TreeNodeInput is the input for a single node when building a tree.
type TreeNodeInput struct {
	Concept  string
	Content  string
	Type     string
	Tags     []string
	Children []TreeNodeInput
}

// RememberTreeRequest is the input for RememberTree.
type RememberTreeRequest struct {
	Vault string
	Root  TreeNodeInput
}

// RememberTreeResult is the output from RememberTree.
type RememberTreeResult struct {
	RootID  string
	NodeMap map[string]string // concept → ULID string
}

// RememberTree writes all nodes, associations, and ordinal keys depth-first;
// on failure, already-written nodes are left in storage.
// Returns the root ID and a map of concept → ULID.
func (e *Engine) RememberTree(ctx context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	ws := e.store.ResolveVaultPrefix(req.Vault)
	nodeMap := make(map[string]string)

	rootID, err := e.writeTreeNode(ctx, ws, req.Vault, req.Root, nil, 0, nodeMap)
	if err != nil {
		return nil, fmt.Errorf("RememberTree: %w", err)
	}

	return &RememberTreeResult{
		RootID:  rootID.String(),
		NodeMap: nodeMap,
	}, nil
}

// writeTreeNode writes a single node and all its children recursively (depth-first).
// Returns the ULID of the written engram.
func (e *Engine) writeTreeNode(
	ctx context.Context,
	ws [8]byte,
	vault string,
	input TreeNodeInput,
	parentID *storage.ULID,
	ordinal int32,
	nodeMap map[string]string,
) (storage.ULID, error) {
	// Build the WriteRequest for this node.
	wr := &mbp.WriteRequest{
		Vault:   vault,
		Concept: input.Concept,
		Content: input.Content,
		Tags:    input.Tags,
	}

	// Map the type string to a MemoryType value.
	if input.Type != "" {
		mt, ok := storage.ParseMemoryType(input.Type)
		if ok {
			wr.MemoryType = uint8(mt)
		} else {
			// Store as TypeLabel when not a recognized MemoryType.
			wr.TypeLabel = input.Type
		}
	}

	resp, err := e.Write(ctx, wr)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("write node %q: %w", input.Concept, err)
	}

	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("parse written ULID %q: %w", resp.ID, err)
	}

	nodeMap[input.Concept] = resp.ID

	// Wire parent relationship for non-root nodes.
	if parentID != nil {
		assoc := &storage.Association{
			TargetID:   *parentID,
			RelType:    storage.RelIsPartOf,
			Weight:     1.0,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}
		if err := e.store.WriteAssociation(ctx, ws, id, *parentID, assoc); err != nil {
			return storage.ULID{}, fmt.Errorf("write association for %q: %w", input.Concept, err)
		}
		if err := e.store.WriteOrdinal(ctx, ws, *parentID, id, ordinal); err != nil {
			return storage.ULID{}, fmt.Errorf("write ordinal for %q: %w", input.Concept, err)
		}
	}

	// Recurse for children (ordinals are 1-based to match insertion order).
	for i, child := range input.Children {
		if _, err := e.writeTreeNode(ctx, ws, vault, child, &id, int32(i+1), nodeMap); err != nil {
			return storage.ULID{}, err
		}
	}

	return id, nil
}

// lifecycleStateString converts a LifecycleState to a human-readable string.
func lifecycleStateString(s storage.LifecycleState) string {
	switch s {
	case storage.StateActive:
		return "active"
	case storage.StateCompleted:
		return "completed"
	case storage.StateSoftDeleted:
		return "deleted"
	case storage.StateArchived:
		return "archived"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// RecallTree reads the root engram then recursively reads children using
// ListChildOrdinals (already sorted ascending by ordinal). Returns the full tree.
// limit caps the number of children fetched per node at each level — it is not a
// global cap on total output nodes. maxDepth=0 means unlimited depth.
func (e *Engine) RecallTree(ctx context.Context, vault, rootID string, maxDepth, limit int, includeCompleted bool) (*TreeNode, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	id, err := storage.ParseULID(rootID)
	if err != nil {
		return nil, fmt.Errorf("parse root id: %w", err)
	}

	return e.recallTreeNode(ctx, ws, id, maxDepth, 0, limit, includeCompleted)
}

// recallTreeNode recursively reads a node and its children up to maxDepth.
// maxDepth=0 means unlimited depth. limit caps children per node (0 = no limit).
func (e *Engine) recallTreeNode(
	ctx context.Context,
	ws [8]byte,
	id storage.ULID,
	maxDepth, depth int,
	limit int,
	includeCompleted bool,
) (*TreeNode, error) {
	eng, err := e.store.GetEngram(ctx, ws, id)
	if err != nil {
		return nil, fmt.Errorf("get engram %s: %w", id.String(), err)
	}
	if eng == nil {
		return nil, fmt.Errorf("engram %s not found", id.String())
	}

	var lastAccessed string
	if !eng.LastAccess.IsZero() {
		lastAccessed = eng.LastAccess.Format(time.RFC3339)
	}

	node := &TreeNode{
		ID:           eng.ID.String(),
		Concept:      eng.Concept,
		State:        lifecycleStateString(eng.State),
		LastAccessed: lastAccessed,
	}

	// maxDepth <= 0 means unlimited depth.
	if maxDepth > 0 && depth >= maxDepth {
		node.Children = []TreeNode{}
		return node, nil
	}

	ordinals, err := e.store.ListChildOrdinals(ctx, ws, id)
	if err != nil {
		return nil, fmt.Errorf("list child ordinals for %s: %w", id.String(), err)
	}

	if limit > 0 && len(ordinals) > limit {
		ordinals = ordinals[:limit]
	}

	node.Children = []TreeNode{}

	for _, entry := range ordinals {
		if !includeCompleted {
			metas, err := e.store.GetMetadata(ctx, ws, []storage.ULID{entry.ChildID})
			if err != nil || len(metas) == 0 || metas[0] == nil {
				continue // skip unreadable children
			}
			if metas[0].State == storage.StateCompleted {
				continue
			}
		}
		child, err := e.recallTreeNode(ctx, ws, entry.ChildID, maxDepth, depth+1, limit, includeCompleted)
		if err != nil {
			return nil, err
		}
		child.Ordinal = entry.Ordinal
		node.Children = append(node.Children, *child)
	}

	return node, nil
}
