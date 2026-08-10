package storage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPointGetReadersAreCovered is a COMPLETENESS check, not a defect detector.
// It asserts one decidable fact: every method in package storage whose body
// calls ps.pointGet appears in absenceVsFailureCases, the behavioural table in
// read_absence_vs_failure_test.go.
//
// # Why this shape and not a source census for the bad idiom
//
// The first version of this file pattern-matched the laundering idiom itself —
// an `if err != nil || <absence>` whose body returns a zero tuple. That census
// was replaced because it did not pin the class it claimed to. A full
// reintroduction of the original defect passed it in four of the five natural
// ways to write the same bug: naming the error variable `e`, adding a
// slog.Debug before the return, inverting to `if !(err == nil && val != nil)`,
// or — the most likely refactor of all — splitting the `||` into two `if`s:
//
//	val, err := ps.pointGet(key)
//	if err != nil {
//	    return 0.0, nil       // still laundered; no `||` anywhere
//	}
//
// Recognising every way to write a bad `if` is not decidable. Whether a
// method's OBSERVABLE behaviour under a failed read is an error is, so the
// table tests that directly and this census only guards the table's coverage.
// A partial idiom matcher reads as coverage it does not provide, which is worse
// than no matcher at all.
//
// # What this does and does not catch
//
// CATCHES: a new pointGet-routed reader added without a table row (so its
// absence-vs-failure behaviour is never exercised), and the removal of a table
// row for a reader that still exists.
//
// DOES NOT CATCH: a reader that launders a read error while reading through
// pebble directly (storage.Get, ps.db.Get, an iterator) instead of pointGet.
// Those are outside the seam and therefore outside this census — the honest
// boundary, stated so nobody reads more into a green run than it means.
// FlagContradiction (association.go) is exactly such a reader and is tracked
// separately as #804; adding it to pointGet would pull it into this census and
// require a table row, which is the intended way for it to be fixed.
func TestPointGetReadersAreCovered(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("census found no source files — wrong working directory?")
	}

	fset := token.NewFileSet()
	scanned := 0
	routers := map[string]string{} // method name -> position

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

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil {
				continue
			}
			if fn.Name.Name == "pointGet" {
				continue // the seam itself
			}
			if callsPointGet(fn.Body) {
				routers[fn.Name.Name] = fset.Position(fn.Pos()).String()
			}
		}
	}

	if scanned == 0 {
		t.Fatal("census scanned no non-test files")
	}
	if len(routers) == 0 {
		t.Fatal("census found no pointGet callers — the seam was removed or renamed, " +
			"and this census is now vacuous. Re-point it or delete it deliberately.")
	}

	covered := map[string]bool{}
	for _, c := range absenceVsFailureCases {
		covered[c.method] = true
	}

	var missing []string
	for name, pos := range routers {
		if !covered[name] {
			missing = append(missing, name+" ("+pos+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("pointGet-routed reader(s) with no row in absenceVsFailureCases:\n  %s\n"+
			"Every reader behind the pointGet seam must have its absence-vs-failure behaviour\n"+
			"exercised: absence is a fact about the vault, failure is a fact about the disk, and\n"+
			"a reader that reports the first when it observed the second hands a writer a\n"+
			"fabricated tuple to persist. Add a row to absenceVsFailureCases.",
			strings.Join(missing, "\n  "))
	}

	// The table must not name a method that no longer exists, or it silently
	// stops guarding anything.
	for _, c := range absenceVsFailureCases {
		if !c.routesPointGet {
			continue
		}
		if _, ok := routers[c.method]; !ok {
			t.Errorf("absenceVsFailureCases row %q claims to route through pointGet, but no such "+
				"method in package storage calls pointGet. Fix the row or clear routesPointGet.", c.method)
		}
	}

	names := make([]string, 0, len(routers))
	for name := range routers {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Logf("census scanned %d non-test files; pointGet-routed readers: %s", scanned, strings.Join(names, ", "))
}

// TestReadFaultIsArmedOnlyByTests asserts the structural fact that
// TestPointGetSeamIsNilInProductionConstructors relies on and cannot itself
// establish: nothing outside a _test.go file ever ASSIGNS PebbleStore.readFault
// or PebbleStore.iterFault, so there is no production path that can arm either
// seam. The fields are unexported, so this package-local scan is complete for
// the whole program.
//
// iterFault (#808) is the iterator sibling of readFault and joined this scan
// with it: a fault seam that production code can arm is not a test seam, and
// the two must not drift into different guarantees.
func TestReadFaultIsArmedOnlyByTests(t *testing.T) {
	faultSeams := map[string]bool{"readFault": true, "iterFault": true}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	scanned := 0

	record := func(n ast.Node) {
		offenders = append(offenders, fset.Position(n.Pos()).String())
	}

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
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && faultSeams[sel.Sel.Name] {
						record(node)
					}
				}
			case *ast.KeyValueExpr:
				// A composite literal field: PebbleStore{readFault: ...}.
				if id, ok := node.Key.(*ast.Ident); ok && faultSeams[id.Name] {
					record(node)
				}
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scan found no non-test files — wrong working directory?")
	}
	if len(offenders) > 0 {
		t.Errorf("a read/iterator fault seam is assigned in non-test source, so it is no longer test-only:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// callsPointGet reports whether body contains a call to a selector named
// pointGet (i.e. `<recv>.pointGet(...)`).
func callsPointGet(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "pointGet" {
			found = true
		}
		return !found
	})
	return found
}
