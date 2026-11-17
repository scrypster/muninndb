package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func wiringOpen(t *testing.T, dir string) (*storage.PebbleStore, *pebble.DB) {
	t.Helper()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100}), db
}

// wiringSeed writes n memories through the store's own write path and saves
// the count, as a follower's recount at open would have.
func wiringSeed(t *testing.T, store *storage.PebbleStore, ws [8]byte, n int) []storage.ULID {
	t.Helper()
	var ids []storage.ULID
	for i := 0; i < n; i++ {
		id, err := store.WriteEngram(context.Background(), ws, &storage.Engram{
			Concept: fmt.Sprintf("signal box %d", i), Content: fmt.Sprintf("signal box %d was relevered", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	store.FlushCounters()
	return ids
}

// The applier wiring in server.go is what tells the store that a follower's
// applied records never moved its counts. Through it, a former follower's
// clean restart recounts instead of trusting the stale count.
func TestWireApplierToStore_FollowerCountsAreNotCertified(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, db := wiringOpen(t, dir)
	ws := store.VaultPrefix("signal-boxes")
	ids := wiringSeed(t, store, ws, 8)

	b := db.NewBatch() // the leader forgets 3: one replicated batch, never committed here
	for _, id := range ids[:3] {
		if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
			t.Fatal(err)
		}
	}
	repr := append([]byte(nil), b.Repr()...)
	b.Close()

	applier := replication.NewApplier(db)
	wireApplierToStore(applier, store)
	if err := applier.Apply(replication.ReplicationEntry{Seq: 1, Op: replication.OpBatch, Value: repr}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, _ := wiringOpen(t, dir)
	defer again.Close()
	if got := again.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("after a clean restart the former follower counts %d for a vault holding 5", got)
	}
}

// The promotion hook resets a stale count.
func TestVaultCountResetAtPromotion_ResetsAStaleCount(t *testing.T) {
	ctx := context.Background()
	store, db := wiringOpen(t, t.TempDir())
	defer store.Close()
	ws := store.VaultPrefix("signal-boxes")
	ids := wiringSeed(t, store, ws, 8)
	b := db.NewBatch() // raw deletes, as the applier writes them
	for _, id := range ids[:3] {
		if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	b.Close()
	if got := store.GetVaultCount(ctx, ws); got != 8 {
		t.Fatalf("fixture: stale count %d, want 8", got)
	}
	vaultCountResetAtPromotion(store)(1)
	if got := store.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("count %d after the promotion hook, the vault holds 5", got)
	}
}

// server.go must install both, and the promotion hook must be in place before
// the coordinator starts: set later (after startCoordinator, or from a
// goroutine) it can miss the first promotion. Removing or moving either line
// leaves the functions above tested and the server unprotected, so the source
// is checked: each call must exist, and the BeforeBecomingCortex assignment
// must sit in the same function as the startCoordinator call, outside any
// function literal, and before it.
func TestServerWiresVaultCountsIntoTheCluster(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stack []ast.Node // enclosing FuncDecl and FuncLit nodes
	var applierWired bool
	var hookFunc, startFunc ast.Node
	var hookPos, startPos token.Pos
	top := func() ast.Node {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		ast.Inspect(n, func(c ast.Node) bool {
			if c == nil || c == n {
				return c != nil
			}
			switch x := c.(type) {
			case *ast.FuncDecl, *ast.FuncLit:
				stack = append(stack, x)
				walk(x)
				stack = stack[:len(stack)-1]
				return false
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					switch id.Name {
					case "wireApplierToStore":
						applierWired = true
					case "startCoordinator":
						if startFunc == nil {
							startFunc, startPos = top(), x.Pos()
						}
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "BeforeBecomingCortex" || i >= len(x.Rhs) {
						continue
					}
					if call, ok := x.Rhs[i].(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "vaultCountResetAtPromotion" {
							hookFunc, hookPos = top(), x.Pos()
						}
					}
				}
			}
			return true
		})
	}
	walk(f)
	if !applierWired {
		t.Error("server.go no longer calls wireApplierToStore: a follower's clean Close certifies stale vault counts")
	}
	if hookFunc == nil {
		t.Fatal("server.go no longer sets BeforeBecomingCortex to vaultCountResetAtPromotion: a new leader prunes from a follower's stale counts")
	}
	if startFunc == nil {
		t.Fatal("server.go has no startCoordinator call: this check needs updating to where the coordinator now starts")
	}
	if hookFunc != startFunc || hookPos > startPos {
		t.Error("server.go sets BeforeBecomingCortex inside a function literal or after startCoordinator: " +
			"a promotion that runs first takes the leader role on a follower's stale counts")
	}
}
