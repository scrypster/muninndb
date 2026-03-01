package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
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

// AddChildInput is the input for adding a single child engram to a parent.
type AddChildInput struct {
	Concept string
	Content string
	Type    string
	Tags    []string
	Ordinal *int32 // nil = append at end (max ordinal + 1)
}

// AddChildResult is returned by AddChild.
type AddChildResult struct {
	ChildID string
	Ordinal int32
}

// maxTreeDepth is the maximum allowed nesting depth for a RememberTree input.
// Trees deeper than this are rejected before any writes occur.
const maxTreeDepth = 20

// validateTreeNode checks that a node and all its descendants have non-empty
// concepts, do not exceed maxTreeDepth levels of nesting, and contain no
// duplicate concept strings anywhere in the tree.
func validateTreeNode(node TreeNodeInput, depth int) error {
	seen := make(map[string]bool)
	return validateTreeNodeInner(node, depth, seen)
}

// validateTreeNodeInner is the recursive helper for validateTreeNode.
// seen tracks all concept strings encountered so far across the whole tree so
// that a duplicate anywhere — including across separate branches — is caught.
func validateTreeNodeInner(node TreeNodeInput, depth int, seen map[string]bool) error {
	if strings.TrimSpace(node.Concept) == "" {
		return fmt.Errorf("tree node at depth %d has empty concept", depth)
	}
	if depth > maxTreeDepth {
		return fmt.Errorf("tree depth exceeds maximum of %d levels", maxTreeDepth)
	}
	if seen[node.Concept] {
		return fmt.Errorf("duplicate concept %q at depth %d", node.Concept, depth)
	}
	seen[node.Concept] = true
	for _, child := range node.Children {
		if err := validateTreeNodeInner(child, depth+1, seen); err != nil {
			return err
		}
	}
	return nil
}

// flatTreeItem is a single node in the flattened tree representation used by
// the batch write path of RememberTree.
type flatTreeItem struct {
	input     TreeNodeInput
	parentIdx int   // -1 for root
	ordinal   int32 // 0 for root (unused)
}

// flattenTree does a pre-order DFS traversal and returns a flat slice.
// parentIdx for root is -1. Ordinals are 1-based per parent.
func flattenTree(root TreeNodeInput) []flatTreeItem {
	items := []flatTreeItem{}
	var dfs func(node TreeNodeInput, parentIdx int, ordinal int32)
	dfs = func(node TreeNodeInput, parentIdx int, ordinal int32) {
		idx := len(items)
		items = append(items, flatTreeItem{input: node, parentIdx: parentIdx, ordinal: ordinal})
		for i, child := range node.Children {
			dfs(child, idx, int32(i+1))
		}
	}
	dfs(root, -1, 0)
	return items
}

// RememberTree writes all nodes, associations, and ordinal keys. All engram
// records are committed in a single atomic Pebble batch so that a crash cannot
// leave the constellation in a partially-written state. Associations and
// ordinal keys are wired after the batch commits (Phase 2).
func (e *Engine) RememberTree(ctx context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	if err := validateTreeNode(req.Root, 0); err != nil {
		return nil, fmt.Errorf("RememberTree: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(req.Vault)
	items := flattenTree(req.Root)

	// Build storage.Engram objects for every node.
	engrams := make([]*storage.Engram, len(items))
	for i, item := range items {
		eng := &storage.Engram{
			Concept: item.input.Concept,
			Content: item.input.Content,
			Tags:    item.input.Tags,
		}
		if item.input.Type != "" {
			if mt, ok := storage.ParseMemoryType(item.input.Type); ok {
				eng.MemoryType = mt
			} else {
				eng.TypeLabel = item.input.Type
			}
		}
		engrams[i] = eng
	}

	// Phase 1: write all engram records in a single atomic Pebble batch.
	batch := e.store.NewBatch()
	defer batch.Discard()

	for i, eng := range engrams {
		if err := batch.WriteEngram(ctx, ws, eng); err != nil {
			return nil, fmt.Errorf("RememberTree: queue node %q: %w", items[i].input.Concept, err)
		}
	}
	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("RememberTree: commit batch: %w", err)
	}

	// Build ID slices now that the batch has assigned ULIDs via defaulting.
	ids := make([]storage.ULID, len(items))
	idStrings := make([]string, len(items))
	for i, eng := range engrams {
		ids[i] = eng.ID
		idStrings[i] = eng.ID.String()
	}

	// Phase 2: wire associations and ordinals (individual writes — not crash-critical
	// because the engrams themselves are already durable from Phase 1).
	for i, item := range items {
		if item.parentIdx >= 0 {
			parentID := ids[item.parentIdx]
			assoc := &storage.Association{
				TargetID:   parentID,
				RelType:    storage.RelIsPartOf,
				Weight:     1.0,
				Confidence: 1.0,
				CreatedAt:  time.Now(),
			}
			if err := e.store.WriteAssociation(ctx, ws, ids[i], parentID, assoc); err != nil {
				return nil, fmt.Errorf("RememberTree: write association for %q: %w", item.input.Concept, err)
			}
			if err := e.store.WriteOrdinal(ctx, ws, parentID, ids[i], item.ordinal); err != nil {
				return nil, fmt.Errorf("RememberTree: write ordinal for %q: %w", item.input.Concept, err)
			}
		}
	}

	// Build nodeMap.
	nodeMap := make(map[string]string, len(items))
	for i, item := range items {
		nodeMap[item.input.Concept] = idStrings[i]
	}

	return &RememberTreeResult{RootID: idStrings[0], NodeMap: nodeMap}, nil
}

// CountChildren returns the number of direct children of engramID registered in the
// ordinal index. This is safe to call after a soft-delete of the parent because
// soft-delete does not clean up ordinal keys where the deleted engram is the parent.
func (e *Engine) CountChildren(ctx context.Context, vault, engramID string) (int, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	pid, err := storage.ParseULID(engramID)
	if err != nil {
		return 0, fmt.Errorf("count children: parse id: %w", err)
	}
	entries, err := e.store.ListChildOrdinals(ctx, ws, pid)
	if err != nil {
		return 0, fmt.Errorf("count children: %w", err)
	}
	return len(entries), nil
}

// AddChild writes a single child engram, wires the is_part_of association (child → parent),
// and assigns an ordinal key using a single atomic Pebble batch. This ensures that
// a crash between any two writes cannot leave the tree in an inconsistent state.
// If input.Ordinal is nil, appends after the last existing child.
func (e *Engine) AddChild(ctx context.Context, vault, parentID string, input *AddChildInput) (*AddChildResult, error) {
	if input == nil {
		return nil, fmt.Errorf("add child: input must not be nil")
	}
	ws := e.store.ResolveVaultPrefix(vault)
	pid, err := storage.ParseULID(parentID)
	if err != nil {
		return nil, fmt.Errorf("add child: parse parent id: %w", err)
	}

	// Verify parent exists and is active or archived.
	parentEng, err := e.store.GetEngram(ctx, ws, pid)
	if err != nil {
		return nil, fmt.Errorf("add child: read parent %s: %w", parentID, err)
	}
	if parentEng == nil {
		return nil, fmt.Errorf("add child: parent %s not found", parentID)
	}
	if parentEng.State == storage.StateSoftDeleted || parentEng.State == storage.StateCompleted {
		return nil, fmt.Errorf("add child: parent %s has state %s, must be active or archived",
			parentID, lifecycleStateString(parentEng.State))
	}

	// Build the child engram struct directly (same pattern as RememberTree).
	child := &storage.Engram{
		Concept: input.Concept,
		Content: input.Content,
		Tags:    input.Tags,
	}
	if input.Type != "" {
		if mt, ok := storage.ParseMemoryType(input.Type); ok {
			child.MemoryType = mt
		} else {
			child.TypeLabel = input.Type
		}
	}

	// Determine ordinal and commit all three writes atomically.
	// When appending (Ordinal == nil), we must hold the per-parent mutex to
	// serialize the read-modify-write so concurrent appends cannot collide.
	ordinal := int32(1)

	commitBatch := func(ord int32) error {
		batch := e.store.NewBatch()
		defer batch.Discard()
		if err := batch.WriteEngram(ctx, ws, child); err != nil {
			return fmt.Errorf("queue engram: %w", err)
		}
		assoc := &storage.Association{
			TargetID:   pid,
			RelType:    storage.RelIsPartOf,
			Weight:     1.0,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}
		if err := batch.WriteAssociation(ctx, ws, child.ID, pid, assoc); err != nil {
			return fmt.Errorf("queue association: %w", err)
		}
		if err := batch.WriteOrdinal(ctx, ws, pid, child.ID, ord); err != nil {
			return fmt.Errorf("queue ordinal: %w", err)
		}
		return batch.Commit()
	}

	if input.Ordinal != nil {
		ordinal = *input.Ordinal
		// Assign ID before the batch so we have it for the association key.
		child.ID = storage.NewULID()
		if err := commitBatch(ordinal); err != nil {
			return nil, fmt.Errorf("add child: commit batch: %w", err)
		}
	} else {
		mu := e.getChildMutex(parentID)
		mu.Lock()
		existing, err := e.store.ListChildOrdinals(ctx, ws, pid)
		if err != nil {
			mu.Unlock()
			return nil, fmt.Errorf("add child: list ordinals: %w", err)
		}
		// ListChildOrdinals is sorted ascending; the last entry has the max ordinal.
		if len(existing) > 0 {
			ordinal = existing[len(existing)-1].Ordinal + 1
		}
		// Assign ID under mutex so the ULID is stable before the batch commit.
		child.ID = storage.NewULID()
		commitErr := commitBatch(ordinal)
		mu.Unlock()
		if commitErr != nil {
			return nil, fmt.Errorf("add child: commit batch: %w", commitErr)
		}
	}

	// Post-commit side effects (async, non-critical for crash-safety).
	// These mirror what e.Write() triggers after the storage write.
	vaultName := vault
	if vaultName == "" {
		vaultName = "default"
	}
	_ = e.store.WriteVaultName(ws, vaultName)
	e.activity.Record(ws)

	if e.ftsWorker != nil {
		e.ftsWorker.Submit(fts.IndexJob{
			WS:      ws,
			ID:      [16]byte(child.ID),
			Concept: child.Concept,
			Content: child.Content,
			Tags:    child.Tags,
		})
	}

	if e.triggers != nil {
		vaultID := wsVaultID(ws)
		childCopy := *child
		childCopy.Tags = append([]string(nil), child.Tags...)
		e.triggers.NotifyWrite(vaultID, &childCopy, true)
	}

	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}

	return &AddChildResult{ChildID: child.ID.String(), Ordinal: ordinal}, nil
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

	node, err := e.recallTreeNode(ctx, ws, id, maxDepth, 0, limit, includeCompleted)
	if err != nil {
		return nil, err
	}
	if node == nil {
		// Root engram not found — this is a caller error (bad ID or already deleted).
		return nil, fmt.Errorf("root engram %s not found", id.String())
	}
	return node, nil
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
		// Ghost engram: the key was deleted (e.g., hard-deleted between an
		// ordinal write and now, or ordinal cleanup missed it on crash).
		// Treat as a skip — return (nil, nil) so the caller can continue.
		return nil, nil
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

	// Batch metadata lookup: collect all child IDs and call GetMetadata once
	// instead of once per child (avoids the N+1 query pattern).
	var metaByID map[storage.ULID]*storage.EngramMeta
	if !includeCompleted && len(ordinals) > 0 {
		childIDs := make([]storage.ULID, len(ordinals))
		for i, entry := range ordinals {
			childIDs[i] = entry.ChildID
		}
		metas, err := e.store.GetMetadata(ctx, ws, childIDs)
		if err != nil {
			return nil, fmt.Errorf("get metadata for children of %s: %w", id.String(), err)
		}
		metaByID = make(map[storage.ULID]*storage.EngramMeta, len(ordinals))
		for i, meta := range metas {
			if i < len(ordinals) {
				metaByID[ordinals[i].ChildID] = meta
			}
		}
	}

	for _, entry := range ordinals {
		if !includeCompleted {
			meta, ok := metaByID[entry.ChildID]
			// Filter out: missing metadata (hard-deleted ghost), completed, or
			// soft-deleted children. StateSoftDeleted != StateCompleted so both
			// states must be checked explicitly.
			if !ok || meta == nil || meta.State == storage.StateCompleted || meta.State == storage.StateSoftDeleted {
				continue
			}
		}
		child, err := e.recallTreeNode(ctx, ws, entry.ChildID, maxDepth, depth+1, limit, includeCompleted)
		if err != nil {
			return nil, err
		}
		if child == nil {
			// Ghost engram: child was hard-deleted; ordinal key is stale.
			// Skip it rather than panicking on nil dereference.
			continue
		}
		child.Ordinal = entry.Ordinal
		node.Children = append(node.Children, *child)
	}

	return node, nil
}
