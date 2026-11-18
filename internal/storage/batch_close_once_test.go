package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDeferredCloseOfAReplacedBatch is a source check for the shape behind
// the re-embed hang: `defer b.Close()` evaluates b when the defer runs, so if
// the function later closes b itself or assigns a new batch to b, the first
// batch is closed twice. A Pebble batch goes back to a shared pool on Close,
// and the second Close can reset a batch another writer is committing.
//
// It flags, per function, a batch variable (assigned from NewBatch or
// NewIndexedBatch) that has a direct `defer v.Close()` and is also closed
// explicitly or reassigned. `defer func() { v.Close() }()` is the safe form
// and is not flagged. Blind spots: batches held in struct fields and batches
// closed inside helpers they are passed to.
func TestNoDeferredCloseOfAReplacedBatch(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var findings []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		scanned++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			findings = append(findings, deferredCloseOfReplacedBatch(fset, fn.Body)...)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d Go files from %s; the walk is not seeing the repository", scanned, root)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

func deferredCloseOfReplacedBatch(fset *token.FileSet, body *ast.BlockStmt) []string {
	isNewBatch := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && (sel.Sel.Name == "NewBatch" || sel.Sel.Name == "NewIndexedBatch")
	}
	// Variables are keyed by their declaring object, not their name, so an
	// inner block's own `batch :=` is a different variable.
	closeOf := func(call *ast.CallExpr) *ast.Object {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Close" || len(call.Args) != 0 {
			return nil
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return nil
		}
		return id.Obj
	}

	batches := map[*ast.Object]bool{}
	reassigned := map[*ast.Object]bool{}
	deferred := map[*ast.Object]token.Pos{}
	explicit := map[*ast.Object]bool{}
	var directDeferCalls = map[*ast.CallExpr]bool{}

	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Obj == nil || i >= len(s.Rhs) || !isNewBatch(s.Rhs[i]) {
					continue
				}
				if !(s.Tok == token.DEFINE && id.Obj.Decl == s) {
					reassigned[id.Obj] = true
				}
				batches[id.Obj] = true
			}
		case *ast.DeferStmt:
			if v := closeOf(s.Call); v != nil {
				deferred[v] = s.Pos()
				directDeferCalls[s.Call] = true
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || directDeferCalls[call] {
			return true
		}
		if v := closeOf(call); v != nil {
			explicit[v] = true
		}
		return true
	})

	var out []string
	for v, pos := range deferred {
		if batches[v] && (reassigned[v] || explicit[v]) {
			out = append(out, fset.Position(pos).String()+": `defer "+v.Name+".Close()` binds the first batch, but "+v.Name+
				" is also closed or replaced in this function; use `defer func() { "+v.Name+".Close() }()`")
		}
	}
	return out
}
