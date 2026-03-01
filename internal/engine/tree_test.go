package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestRememberAndRecallTree(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "tree-test"

	req := &RememberTreeRequest{
		Vault: vault,
		Root: TreeNodeInput{
			Concept: "Project Alpha",
			Content: "Top-level project plan",
			Type:    "goal",
			Children: []TreeNodeInput{
				{
					Concept: "Phase 1",
					Content: "First phase",
					Type:    "goal",
					Children: []TreeNodeInput{
						{Concept: "Task 1.1", Content: "First task", Type: "task"},
						{Concept: "Task 1.2", Content: "Second task", Type: "task"},
					},
				},
				{
					Concept: "Phase 2",
					Content: "Second phase",
					Type:    "goal",
				},
			},
		},
	}

	result, err := eng.RememberTree(ctx, req)
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}
	if result.RootID == "" {
		t.Fatal("expected non-empty root ID")
	}
	if len(result.NodeMap) != 5 { // root + 2 phases + 2 tasks
		t.Fatalf("NodeMap: got %d entries, want 5", len(result.NodeMap))
	}

	tree, err := eng.RecallTree(ctx, vault, result.RootID, 5, 0, true)
	if err != nil {
		t.Fatalf("RecallTree: %v", err)
	}
	if tree.Concept != "Project Alpha" {
		t.Errorf("root concept: got %q, want %q", tree.Concept, "Project Alpha")
	}
	if len(tree.Children) != 2 {
		t.Fatalf("root children: got %d, want 2", len(tree.Children))
	}
	if tree.Children[0].Concept != "Phase 1" {
		t.Errorf("first child: got %q, want Phase 1", tree.Children[0].Concept)
	}
	if len(tree.Children[0].Children) != 2 {
		t.Fatalf("phase 1 tasks: got %d, want 2", len(tree.Children[0].Children))
	}
	if tree.Children[0].Children[0].Concept != "Task 1.1" {
		t.Errorf("first task: got %q, want Task 1.1", tree.Children[0].Children[0].Concept)
	}
	if tree.Children[0].Children[1].Concept != "Task 1.2" {
		t.Errorf("second task: got %q, want Task 1.2", tree.Children[0].Children[1].Concept)
	}
	// Phase 2 must come after Phase 1 (ordinal order)
	if tree.Children[1].Concept != "Phase 2" {
		t.Errorf("second child: got %q, want Phase 2", tree.Children[1].Concept)
	}
}

func TestRecallTree_LeafNode(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "leaf-tree"

	result, err := eng.RememberTree(ctx, &RememberTreeRequest{
		Vault: vault,
		Root:  TreeNodeInput{Concept: "Solo leaf", Content: "I am alone", Type: "task"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tree, err := eng.RecallTree(ctx, vault, result.RootID, 5, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Concept != "Solo leaf" {
		t.Errorf("concept: got %q", tree.Concept)
	}
	if len(tree.Children) != 0 {
		t.Errorf("expected no children, got %d", len(tree.Children))
	}
}

func TestRecallTree_FilterCompleted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "filter-completed-tree"

	// Create root with two children.
	result, err := eng.RememberTree(ctx, &RememberTreeRequest{
		Vault: vault,
		Root: TreeNodeInput{
			Concept: "Root",
			Content: "Root node",
			Type:    "goal",
			Children: []TreeNodeInput{
				{Concept: "Active Child", Content: "still active", Type: "task"},
				{Concept: "Completed Child", Content: "already done", Type: "task"},
			},
		},
	})
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}

	// Mark the second child as completed.
	completedID, ok := result.NodeMap["Completed Child"]
	if !ok {
		t.Fatal("expected Completed Child in NodeMap")
	}
	if err := eng.UpdateLifecycleState(ctx, vault, completedID, "completed"); err != nil {
		t.Fatalf("UpdateLifecycleState: %v", err)
	}

	// Recall without completed nodes.
	tree, err := eng.RecallTree(ctx, vault, result.RootID, 5, 0, false)
	if err != nil {
		t.Fatalf("RecallTree: %v", err)
	}

	if tree.Concept != "Root" {
		t.Errorf("root concept: got %q, want %q", tree.Concept, "Root")
	}
	if len(tree.Children) != 1 {
		t.Fatalf("expected 1 child after filtering completed, got %d", len(tree.Children))
	}
	if tree.Children[0].Concept != "Active Child" {
		t.Errorf("expected Active Child, got %q", tree.Children[0].Concept)
	}
}

func TestAddChild(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "addchild-test"

	// Create a parent.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "Parent", Content: "root node"})
	if err != nil {
		t.Fatal(err)
	}
	parentID := resp.ID

	// Add first child (no explicit ordinal — should get ordinal 1).
	r1, err := eng.AddChild(ctx, vault, parentID, &AddChildInput{
		Concept: "Child A", Content: "first child", Type: "task",
	})
	if err != nil {
		t.Fatalf("AddChild A: %v", err)
	}
	if r1.Ordinal != 1 {
		t.Errorf("Child A ordinal: got %d, want 1", r1.Ordinal)
	}

	// Add second child — should get ordinal 2.
	r2, err := eng.AddChild(ctx, vault, parentID, &AddChildInput{
		Concept: "Child B", Content: "second child", Type: "task",
	})
	if err != nil {
		t.Fatalf("AddChild B: %v", err)
	}
	if r2.Ordinal != 2 {
		t.Errorf("Child B ordinal: got %d, want 2", r2.Ordinal)
	}

	// Recall tree — children must appear in ordinal order.
	tree, err := eng.RecallTree(ctx, vault, parentID, 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(tree.Children))
	}
	if tree.Children[0].Concept != "Child A" {
		t.Errorf("first child: got %q, want Child A", tree.Children[0].Concept)
	}
	if tree.Children[1].Concept != "Child B" {
		t.Errorf("second child: got %q, want Child B", tree.Children[1].Concept)
	}
}

func TestRecallTree_Limit(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "limit-tree"

	// Create a root with 3 children.
	req := &RememberTreeRequest{
		Vault: vault,
		Root: TreeNodeInput{
			Concept: "Root",
			Content: "Root node",
			Type:    "goal",
			Children: []TreeNodeInput{
				{Concept: "Child 1", Content: "first", Type: "task"},
				{Concept: "Child 2", Content: "second", Type: "task"},
				{Concept: "Child 3", Content: "third", Type: "task"},
			},
		},
	}

	result, err := eng.RememberTree(ctx, req)
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}

	// Recall with limit=2.
	tree, err := eng.RecallTree(ctx, vault, result.RootID, 5, 2, true)
	if err != nil {
		t.Fatalf("RecallTree: %v", err)
	}

	if len(tree.Children) != 2 {
		t.Fatalf("expected 2 children with limit=2, got %d", len(tree.Children))
	}
	// Confirm first 2 by ordinal are returned, not arbitrary.
	if tree.Children[0].Concept != "Child 1" {
		t.Errorf("expected Child 1 as first child, got %q", tree.Children[0].Concept)
	}
	if tree.Children[1].Concept != "Child 2" {
		t.Errorf("expected Child 2 as second child, got %q", tree.Children[1].Concept)
	}
}

// TestRememberTree_AtomicBatch_AllNodesReadable verifies that after a single
// RememberTree call with 3 nodes, all three engrams are individually readable
// from the store. This is the integration-level proof that the atomic Pebble
// batch commit wrote all nodes, not just the root.
func TestRememberTree_AtomicBatch_AllNodesReadable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	vault := "atomic-batch-3nodes"

	result, err := eng.RememberTree(ctx, &RememberTreeRequest{
		Vault: vault,
		Root: TreeNodeInput{
			Concept: "Root Node",
			Content: "root content",
			Children: []TreeNodeInput{
				{Concept: "Child A", Content: "child A content"},
				{Concept: "Child B", Content: "child B content"},
			},
		},
	})
	if err != nil {
		t.Fatalf("RememberTree: %v", err)
	}
	if len(result.NodeMap) != 3 {
		t.Fatalf("NodeMap: got %d entries, want 3", len(result.NodeMap))
	}

	ws := eng.store.ResolveVaultPrefix(vault)

	// Verify each node is individually readable from the store.
	concepts := []string{"Root Node", "Child A", "Child B"}
	for _, concept := range concepts {
		idStr, ok := result.NodeMap[concept]
		if !ok {
			t.Errorf("concept %q missing from NodeMap", concept)
			continue
		}
		id, err := storage.ParseULID(idStr)
		if err != nil {
			t.Errorf("parse ULID for %q: %v", concept, err)
			continue
		}
		engram, err := eng.store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Errorf("GetEngram for %q: %v", concept, err)
			continue
		}
		if engram == nil {
			t.Errorf("engram for %q not found in store after RememberTree", concept)
			continue
		}
		if engram.Concept != concept {
			t.Errorf("concept mismatch: got %q want %q", engram.Concept, concept)
		}
	}
}

func TestAddChild_WritesAll_Atomically(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	parentResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Parent", Content: "parent content",
	})
	require.NoError(t, err)

	result, err := eng.AddChild(ctx, "test", parentResp.ID, &AddChildInput{
		Concept: "Child", Content: "child content",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.ChildID)

	ws := eng.store.ResolveVaultPrefix("test")
	pid, _ := storage.ParseULID(parentResp.ID)
	cid, _ := storage.ParseULID(result.ChildID)

	// All three must be readable after AddChild returns.
	child, err := eng.store.GetEngram(ctx, ws, cid)
	require.NoError(t, err)
	require.NotNil(t, child, "child engram must exist")

	assocs, err := eng.store.GetAssociations(ctx, ws, []storage.ULID{cid}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, assocs[cid], "is_part_of association must exist")

	ordinal, found, err := eng.store.ReadOrdinal(ctx, ws, pid, cid)
	require.NoError(t, err)
	require.True(t, found, "ordinal must exist")
	assert.Equal(t, int32(1), ordinal)
}

