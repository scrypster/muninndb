package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoLaunderedReadErrors is a source census over package storage that pins a
// DEFECT CLASS, not a single defect: a read error disjuncted with an absence
// check and returned as a zero value.
//
//	val, err := ps.pointGet(key)
//	if err != nil || val == nil {
//	    return 0, 1.0, time.Time{}, 0, 0, 0   // <- laundered
//	}
//
// Storage is the layer where that shape is load-bearing, because the tuple a
// reader fabricates here is the tuple a writer re-encodes a few lines later.
// The association write path did exactly that: a transient Pebble read failure
// reset a live edge's relType (reclassifying a directional relation as RelType
// 0, the Hebbian co-activation type), its createdAt, and its peakWeight — the
// COG-27 decay-ceiling anchor. Absence is a fact about the vault; failure is a
// fact about the disk. Returning the first when you observed the second is the
// silently-wrong class (principle #2).
//
// The census flags an `if` whose condition ORs `err != nil` with anything, and
// whose body is a lone `return` that never mentions the error. Rewriting such a
// site to propagate the error clears it. There is deliberately no allowlist: a
// site that genuinely must swallow a read error should say so by returning the
// error and having its CALLER decide, not by hiding it here.
//
// Known limits (documented rather than papered over): it does not see a
// laundering `continue`/`break` inside a loop, nor one split across a temporary
// variable. It catches the shape that reached production twice.
func TestNoLaunderedReadErrors(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("census found no source files — wrong working directory?")
	}

	fset := token.NewFileSet()
	scanned := 0
	var findings []string

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		file, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			ifStmt, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			errName, ok := errOredWithSomethingElse(ifStmt.Cond)
			if !ok {
				return true
			}
			if len(ifStmt.Body.List) != 1 {
				return true
			}
			ret, ok := ifStmt.Body.List[0].(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range ret.Results {
				if mentionsIdent(res, errName) {
					return true // the error survives — not laundered
				}
			}
			pos := fset.Position(ifStmt.Pos())
			findings = append(findings, pos.String())
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("census scanned no non-test files")
	}
	if len(findings) > 0 {
		t.Errorf("laundered read error(s): an `if %s != nil || ...` returning a zero tuple that drops the error.\n"+
			"Absence and failure are different facts; a writer downstream will re-encode the fabricated tuple.\n"+
			"Sites:\n  %s", "err", strings.Join(findings, "\n  "))
	}
	t.Logf("census scanned %d non-test files in package storage, %d findings", scanned, len(findings))
}

// errOredWithSomethingElse reports whether cond is a `||` expression with an
// `<err> != nil` operand, returning the error identifier's name.
func errOredWithSomethingElse(cond ast.Expr) (string, bool) {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.LOR {
		return "", false
	}
	var name string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		b, ok := e.(*ast.BinaryExpr)
		if !ok {
			return
		}
		if b.Op == token.LOR {
			walk(b.X)
			walk(b.Y)
			return
		}
		if b.Op != token.NEQ {
			return
		}
		lhs, lok := b.X.(*ast.Ident)
		rhs, rok := b.Y.(*ast.Ident)
		if lok && rok && rhs.Name == "nil" && isErrName(lhs.Name) {
			name = lhs.Name
		}
	}
	walk(bin)
	return name, name != ""
}

// isErrName matches the conventional error-variable names used in this package.
func isErrName(s string) bool {
	return s == "err" || strings.HasSuffix(s, "Err") || strings.HasPrefix(s, "err")
}

// mentionsIdent reports whether expr references the named identifier anywhere
// (covering both `return err` and `return fmt.Errorf("...: %w", err)`).
func mentionsIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
